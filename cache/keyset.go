package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// defaultKeyRefresh is the least time between two JWKS fetches.
const defaultKeyRefresh = 60 * time.Second

// maxJWKSBytes bounds the issuer's JWKS response.
const maxJWKSBytes = 1 << 20

// keySet verifies RS256 JWT signatures against the issuer's JWKS. Unlike
// go-oidc's RemoteKeySet it never refetches for a token whose kid it
// already holds (a bad signature stays bad), and fetches for an unknown kid
// at most once per refresh, so forged tokens cannot drive requests to the
// issuer.
type keySet struct {
	url     string
	client  *http.Client
	now     func() time.Time
	refresh time.Duration

	mu        sync.Mutex
	keys      []jose.JSONWebKey
	lastFetch time.Time
	inflight  chan struct{}
	fetches   int
}

func newKeySet(url string, client *http.Client, now func() time.Time, refresh time.Duration) *keySet {
	return &keySet{url: url, client: client, now: now, refresh: refresh}
}

var errUnknownKey = errors.New("no issuer key with the token's key ID")

// VerifySignature implements oidc.KeySet. The verifier has already
// checked the algorithm and that there is exactly one signature.
func (ks *keySet) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	jws, err := jose.ParseSigned(jwt, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return nil, fmt.Errorf("malformed jwt: %v", err)
	}
	kid := jws.Signatures[0].Header.KeyID
	keys, err := ks.keysFor(ctx, kid)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if payload, err := jws.Verify(&k); err == nil {
			return payload, nil
		}
	}
	return nil, errors.New("signature does not verify")
}

// keysFor returns the keys a token with kid may be signed with, fetching
// the JWKS when none is held and the last fetch is older than refresh. An
// empty kid matches every key.
func (ks *keySet) keysFor(ctx context.Context, kid string) ([]jose.JSONWebKey, error) {
	ks.mu.Lock()
	if m := matching(ks.keys, kid); len(m) > 0 {
		ks.mu.Unlock()
		return m, nil
	}
	// Requests for an unknown kid during a fetch wait for it rather than
	// fail or start their own.
	if wait := ks.inflight; wait != nil {
		ks.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		ks.mu.Lock()
		defer ks.mu.Unlock()
		if m := matching(ks.keys, kid); len(m) > 0 {
			return m, nil
		}
		return nil, errUnknownKey
	}
	if !ks.lastFetch.IsZero() && ks.now().Sub(ks.lastFetch) < ks.refresh {
		ks.mu.Unlock()
		return nil, errUnknownKey
	}
	ks.lastFetch = ks.now()
	ks.fetches++
	done := make(chan struct{})
	ks.inflight = done
	ks.mu.Unlock()

	keys, err := ks.fetch(ctx)

	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.inflight = nil
	close(done)
	if err != nil {
		return nil, fmt.Errorf("fetching issuer keys: %w", err)
	}
	ks.keys = keys
	if m := matching(keys, kid); len(m) > 0 {
		return m, nil
	}
	return nil, errUnknownKey
}

func matching(keys []jose.JSONWebKey, kid string) []jose.JSONWebKey {
	var m []jose.JSONWebKey
	for _, k := range keys {
		if kid == "" || k.KeyID == kid {
			m = append(m, k)
		}
	}
	return m
}

func (ks *keySet) fetch(ctx context.Context) ([]jose.JSONWebKey, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ks.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := ks.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS answered %s", resp.Status)
	}
	var raw struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("JWKS: %v", err)
	}
	var keys []jose.JSONWebKey
	for _, r := range raw.Keys {
		var k jose.JSONWebKey
		// Keys of other algorithms or types are skipped, as go-oidc does.
		if json.Unmarshal(r, &k) != nil || !k.Valid() || !k.IsPublic() || (k.Algorithm != "" && k.Algorithm != string(jose.RS256)) {
			continue
		}
		keys = append(keys, k)
	}
	return keys, nil
}
