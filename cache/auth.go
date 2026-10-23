package cache

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// DefaultIssuer is the GitHub Actions token issuer.
const DefaultIssuer = "https://token.actions.githubusercontent.com"

// Permission bits of an "ac" claim entry.
const (
	permRead  = 1
	permWrite = 2
)

// identity is what a verified runtime token grants.
type identity struct {
	RepoID string
	// OwnerID is the repository owner's numeric ID; empty when the token
	// has no such claim.
	OwnerID string
	// WriteScope is the one scope the job may save to; empty when none.
	WriteScope string
	// ReadScopes are searched on restore in this order: the write scope
	// first when readable, then the others in token order.
	ReadScopes []string
	// TokenHash binds an upload reservation to the token that made it.
	TokenHash string
}

// ownerIDClaim names the repository owner's numeric ID in a runtime token;
// verify's claims struct tag spells the same name. It is taken from
// GitHub's OIDC token claims and has not been confirmed in a runtime
// token; repositoryIDs does not depend on it.
const ownerIDClaim = "repository_owner_id"

var (
	errUnauthenticated = errors.New("unauthenticated")
	errIssuerDown      = errors.New("token issuer unavailable")
)

// verifier checks runtime tokens against the issuer's JWKS. Discovery is
// lazy and retried, so the listener starts while the issuer is unreachable
// and answers 503 until it is reachable.
type verifier struct {
	issuer string
	client *http.Client
	now    func() time.Time
	// keyRefresh is the least time between JWKS fetches.
	keyRefresh time.Duration

	mu        sync.Mutex
	v         *oidc.IDTokenVerifier
	keys      *keySet
	lastTry   time.Time
	lastError error
	// ready is v once discovery has succeeded, readable without mu.
	ready       atomic.Pointer[oidc.IDTokenVerifier]
	discovering atomic.Bool
	// verifications counts bearer tokens taken up for verification.
	verifications atomic.Int64
}

// Discovery failures are retried at most this often.
const discoveryRetry = 10 * time.Second

func newVerifier(issuer string, client *http.Client, now func() time.Time) *verifier {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &verifier{issuer: issuer, client: client, now: now, keyRefresh: defaultKeyRefresh}
}

func (v *verifier) idTokenVerifier() (*oidc.IDTokenVerifier, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.v != nil {
		return v.v, nil
	}
	if !v.lastTry.IsZero() && v.now().Sub(v.lastTry) < discoveryRetry {
		return nil, fmt.Errorf("%w: %v", errIssuerDown, v.lastError)
	}
	v.lastTry = v.now()
	dctx, cancel := context.WithTimeout(oidc.ClientContext(context.Background(), v.client), 15*time.Second)
	defer cancel()
	p, err := oidc.NewProvider(dctx, v.issuer)
	if err != nil {
		v.lastError = err
		return nil, fmt.Errorf("%w: %v", errIssuerDown, err)
	}
	v.keys = newKeySet(providerJWKS(p), v.client, v.now, v.keyRefresh)
	// No audience check: the runtime token's audience names the issuing
	// service and the run, not this server, so there is no stable value to
	// require. Issuer, signature, exp and nbf are checked.
	v.v = oidc.NewVerifier(v.issuer, v.keys, &oidc.Config{
		SkipClientIDCheck:    true,
		SupportedSigningAlgs: []string{oidc.RS256},
		Now:                  v.now,
	})
	v.ready.Store(v.v)
	return v.v, nil
}

// discoverInBackground starts a discovery attempt unless one started here
// is still running; idTokenVerifier applies discoveryRetry.
func (v *verifier) discoverInBackground() {
	if !v.discovering.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer v.discovering.Store(false)
		_, _ = v.idTokenVerifier()
	}()
}

func providerJWKS(p *oidc.Provider) string {
	var c struct {
		JWKS string `json:"jwks_uri"`
	}
	_ = p.Claims(&c)
	return c.JWKS
}

type acEntry struct {
	Scope      string `json:"Scope"`
	Permission int    `json:"Permission"`
}

// canonicalPayload returns the decoded payload of raw when raw is a compact
// JWS with three segments that each decode under strict unpadded
// base64url, and false otherwise. go-jose strips whitespace and ignores
// trailing padding bits, so without this check one signed token has many
// accepted spellings.
func canonicalPayload(raw string) ([]byte, bool) {
	segs := strings.Split(raw, ".")
	if len(segs) != 3 {
		return nil, false
	}
	var payload []byte
	for i, s := range segs {
		b, err := base64.RawURLEncoding.Strict().DecodeString(s)
		if err != nil {
			return nil, false
		}
		if i == 1 {
			payload = b
		}
	}
	return payload, true
}

// verifyToken checks a bearer token's form, signature, issuer, exp and nbf
// and returns its verified form with tokenHash, the SHA-256 of its decoded
// payload. With wait false, a verifier not yet discovered is reported as
// errIssuerDown without waiting for a discovery in progress.
func (v *verifier) verifyToken(ctx context.Context, authz string, wait bool) (*oidc.IDToken, string, error) {
	raw, ok := strings.CutPrefix(authz, "Bearer ")
	if !ok || raw == "" {
		return nil, "", fmt.Errorf("%w: missing bearer token", errUnauthenticated)
	}
	v.verifications.Add(1)
	// RawURLEncoding has no '.' or whitespace, so a token passing this has
	// exactly one spelling of its header, payload and signature bytes.
	payload, ok := canonicalPayload(raw)
	if !ok {
		return nil, "", fmt.Errorf("%w: token is not a canonical compact JWS", errUnauthenticated)
	}
	iv := v.ready.Load()
	if iv == nil {
		if !wait {
			v.discoverInBackground()
			return nil, "", fmt.Errorf("%w: not yet discovered", errIssuerDown)
		}
		var err error
		if iv, err = v.idTokenVerifier(); err != nil {
			return nil, "", err
		}
	}
	tok, err := iv.Verify(ctx, raw)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", errUnauthenticated, err)
	}
	sum := sha256.Sum256(payload)
	return tok, hex.EncodeToString(sum[:]), nil
}

// runtimeClaims are the claims verify requires of a runtime token.
type runtimeClaims struct {
	AC     *string `json:"ac"`
	RepoID *string `json:"repository_id"`
	// Raw: numericClaim accepts a string or a number and fails closed.
	OwnerID json.RawMessage `json:"repository_owner_id"`
}

// job returns the limiter key of a runtime token's job, the hash of its
// verified payload, for a token verify would accept that the allowlist
// permits. It never waits for issuer discovery.
func (v *verifier) job(ctx context.Context, authz string, allow allowlist) (string, error) {
	id, err := v.identify(ctx, authz, false)
	if err != nil {
		return "", err
	}
	if !allow.permits(id) {
		return "", fmt.Errorf("%w: repository not served", errUnauthenticated)
	}
	return id.TokenHash, nil
}

// verify returns the identity of a bearer token. errUnauthenticated covers
// every token or claim defect; errIssuerDown means the keys are unknown.
func (v *verifier) verify(ctx context.Context, authz string) (*identity, error) {
	return v.identify(ctx, authz, true)
}

func (v *verifier) identify(ctx context.Context, authz string, wait bool) (*identity, error) {
	tok, hash, err := v.verifyToken(ctx, authz, wait)
	if err != nil {
		return nil, err
	}
	var claims runtimeClaims
	if err := tok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: claims: %v", errUnauthenticated, err)
	}
	if claims.RepoID == nil || !repoIDPattern.MatchString(*claims.RepoID) {
		return nil, fmt.Errorf("%w: repository_id claim missing or not numeric", errUnauthenticated)
	}
	if claims.AC == nil {
		return nil, fmt.Errorf("%w: ac claim missing", errUnauthenticated)
	}
	var acl []acEntry
	if err := json.Unmarshal([]byte(*claims.AC), &acl); err != nil {
		return nil, fmt.Errorf("%w: ac claim is not a JSON array of scopes", errUnauthenticated)
	}
	id := &identity{RepoID: *claims.RepoID, OwnerID: numericClaim(claims.OwnerID), TokenHash: hash}
	for _, e := range acl {
		if e.Scope == "" {
			return nil, fmt.Errorf("%w: ac entry without a scope", errUnauthenticated)
		}
		if e.Permission&permWrite != 0 && id.WriteScope == "" {
			id.WriteScope = e.Scope
		}
	}
	if id.WriteScope != "" {
		for _, e := range acl {
			if e.Scope == id.WriteScope && e.Permission&permRead != 0 {
				id.ReadScopes = append(id.ReadScopes, e.Scope)
				break
			}
		}
	}
	for _, e := range acl {
		if e.Permission&permRead != 0 && !slices.Contains(id.ReadScopes, e.Scope) {
			id.ReadScopes = append(id.ReadScopes, e.Scope)
		}
	}
	return id, nil
}

// numericClaim returns a claim holding a decimal ID, as a string or a JSON
// integer, and "" for anything else.
func numericClaim(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		s = string(raw)
	}
	if !repoIDPattern.MatchString(s) {
		return ""
	}
	return s
}

// allowlist names the repositories, by ID or by owner ID, this server
// serves. Every other token is denied.
type allowlist struct {
	owners map[string]bool
	repos  map[string]bool
}

func newAllowlist(owners, repos []int64) allowlist {
	a := allowlist{owners: map[string]bool{}, repos: map[string]bool{}}
	for _, o := range owners {
		a.owners[strconv.FormatInt(o, 10)] = true
	}
	for _, r := range repos {
		a.repos[strconv.FormatInt(r, 10)] = true
	}
	return a
}

func (a allowlist) empty() bool { return len(a.owners) == 0 && len(a.repos) == 0 }

func (a allowlist) permits(id *identity) bool {
	return a.repos[id.RepoID] || (id.OwnerID != "" && a.owners[id.OwnerID])
}
