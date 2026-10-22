package cache

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// testIssuer is a local OIDC issuer: discovery, a JWKS and RS256 tokens.
type testIssuer struct {
	URL string
	key *rsa.PrivateKey
	kid string
	// down makes discovery answer 503.
	down atomic.Bool
	// extra, once set, is published as "rotated-key" too.
	extra                   atomic.Pointer[rsa.PrivateKey]
	discoveryHits, jwksHits atomic.Int64
}

func newTestIssuer(t testing.TB) *testIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &testIssuer{key: key, kid: "test-key"}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	is.URL = srv.URL
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		is.discoveryHits.Add(1)
		if is.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": is.URL, "jwks_uri": is.URL + "/.well-known/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /.well-known/jwks", func(w http.ResponseWriter, _ *http.Request) {
		is.jwksHits.Add(1)
		keys := []map[string]string{jwk(&is.key.PublicKey, is.kid)}
		if k := is.extra.Load(); k != nil {
			keys = append(keys, jwk(&k.PublicKey, "rotated-key"))
		}
		json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})
	return is
}

func jwk(pub *rsa.PublicKey, kid string) map[string]string {
	return map[string]string{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid,
		"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signJWT signs claims with key under kid.
func signJWT(t testing.TB, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	c, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	in := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + b64(sig)
}

type scopeGrant struct {
	Scope      string
	Permission int
}

// acClaim renders scopes as the "ac" claim: a JSON string holding an array.
func acClaim(t testing.TB, scopes ...scopeGrant) string {
	t.Helper()
	b, err := json.Marshal(scopes)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// claims returns valid runtime token claims for repo with scopes.
func (is *testIssuer) claims(t testing.TB, repoID string, scopes ...scopeGrant) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": is.URL, "aud": "vso:example", "sub": "example",
		"iat": now.Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
		"repository_id": repoID, "ac": acClaim(t, scopes...),
	}
}

func (is *testIssuer) token(t testing.TB, repoID string, scopes ...scopeGrant) string {
	return signJWT(t, is.key, is.kid, is.claims(t, repoID, scopes...))
}

// ownedToken is token with the repository owner ID claim.
func (is *testIssuer) ownedToken(t testing.TB, repoID string, owner any, scopes ...scopeGrant) string {
	c := is.claims(t, repoID, scopes...)
	c[ownerIDClaim] = owner
	return signJWT(t, is.key, is.kid, c)
}

func (is *testIssuer) sign(t testing.TB, claims map[string]any) string {
	return signJWT(t, is.key, is.kid, claims)
}

const (
	mainScope = "refs/heads/main"
	prScope   = "refs/pull/7/merge"
)

func writeMain() scopeGrant { return scopeGrant{mainScope, permRead | permWrite} }
func readMain() scopeGrant  { return scopeGrant{mainScope, permRead} }
func writePR() scopeGrant   { return scopeGrant{prScope, permRead | permWrite} }
