package cache

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAllowlist(t *testing.T) {
	logs := &logRecorder{}
	h := newHarness(t, func(o *Options) {
		o.RepositoryIDs = []int64{300}
		o.RepositoryOwnerIDs = []int64{7}
		o.Log = slog.New(logs)
	})
	allowed := map[string]string{
		"owner as string":         h.is.ownedToken(t, "100", "7", writeMain()),
		"owner as number":         h.is.ownedToken(t, "101", 7, writeMain()),
		"repository, no owner":    h.is.token(t, "300", writeMain()),
		"repository, other owner": h.is.ownedToken(t, "300", "8", writeMain()),
	}
	for name, tok := range allowed {
		if r := h.create(tok, "k", v1); !r.ok() {
			t.Errorf("%s: %+v", name, r)
		}
	}
	denied := map[string]string{
		"foreign owner":       h.is.ownedToken(t, "100", "8", writeMain()),
		"missing owner claim": h.is.token(t, "100", writeMain()),
		"owner not numeric":   h.is.ownedToken(t, "100", "7x", writeMain()),
		"owner is repository": h.is.ownedToken(t, "7", "8", writeMain()),
	}
	for name, tok := range denied {
		for _, method := range []string{"CreateCacheEntry", "GetCacheEntryDownloadURL", "FinalizeCacheEntryUpload"} {
			r := h.call(tok, method, map[string]any{"key": "k", "version": v1, "size_bytes": "1"})
			if r.status != http.StatusForbidden || r.code != twirpPermissionDenied {
				t.Errorf("%s %s: %+v", name, method, r)
			}
		}
	}
	// Repository 100 was denied 9 times in this minute: logged once. 7 once.
	if n := count(logs.snapshot(), "cache: repository not served"); n != 2 {
		t.Errorf("denial logged %d times, want 2", n)
	}
	h.clk.add(time.Minute)
	h.create(denied["foreign owner"], "k", v1)
	if n := count(logs.snapshot(), "cache: repository not served"); n != 3 {
		t.Errorf("after a minute, denial logged %d times, want 3", n)
	}
}

func TestAllowlistByRepositoryOnly(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.RepositoryIDs = []int64{100} })
	if r := h.create(h.is.ownedToken(t, "100", "1", writeMain()), "k", v1); !r.ok() {
		t.Fatalf("listed repository: %+v", r)
	}
	if r := h.create(h.is.ownedToken(t, "101", "1", writeMain()), "k", v1); r.status != http.StatusForbidden {
		t.Fatalf("unlisted repository of the same owner: %+v", r)
	}
}

// Forged tokens do not make the server fetch the issuer's keys: a bad
// signature under a known kid never refetches, and an unknown kid refetches
// at most once per refresh window. A real key rotation is picked up once
// the window has passed.
func TestJWKSRefetchIsRateLimited(t *testing.T) {
	logs := &logRecorder{}
	h := newHarness(t, func(o *Options) { o.Log = slog.New(logs) })
	good := h.is.token(t, "100", writeMain())
	if r := h.create(good, "k", v1); !r.ok() {
		t.Fatalf("valid token: %+v", r)
	}
	if n := h.is.jwksHits.Load(); n != 1 {
		t.Fatalf("first verification fetched the JWKS %d times", n)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	claims := h.is.claims(t, "100", writeMain())
	for range 20 {
		if r := h.create(signJWT(t, other, h.is.kid, claims), "k", v1); r.status != http.StatusUnauthorized {
			t.Fatalf("bad signature: %+v", r)
		}
	}
	if n := h.is.jwksHits.Load(); n != 1 {
		t.Fatalf("bad signatures under a known kid fetched the JWKS %d times", n)
	}
	// The first fetch opened a window: unknown kids within it fetch nothing.
	for range 5 {
		if r := h.create(signJWT(t, other, "rotated-key", claims), "k", v1); r.status != http.StatusUnauthorized {
			t.Fatalf("unknown kid: %+v", r)
		}
	}
	if n := h.is.jwksHits.Load(); n != 1 {
		t.Fatalf("unknown kids within the window fetched the JWKS %d times", n-1)
	}
	h.clk.add(defaultKeyRefresh)
	for range 20 {
		if r := h.create(signJWT(t, other, "rotated-key", claims), "k", v1); r.status != http.StatusUnauthorized {
			t.Fatalf("unknown kid: %+v", r)
		}
	}
	if n := h.is.jwksHits.Load(); n != 2 {
		t.Fatalf("20 unknown-kid tokens in a new window fetched the JWKS %d times, want 1", n-1)
	}
	// The issuer rotates to the other key: tokens under the new kid are
	// accepted once the refresh window has passed.
	h.is.extra.Store(other)
	if r := h.create(signJWT(t, other, "rotated-key", claims), "k", v1); r.status != http.StatusUnauthorized {
		t.Fatalf("within the window: %+v", r)
	}
	h.clk.add(defaultKeyRefresh)
	if r := h.create(signJWT(t, other, "rotated-key", claims), "k", v1); !r.ok() {
		t.Fatalf("after the window: %+v", r)
	}
	if n := h.is.jwksHits.Load(); n != 3 {
		t.Fatalf("JWKS fetched %d times, want 3", n)
	}
	// 46 rejected tokens over two minutes of the fake clock: logged twice.
	if n := count(logs.snapshot(), "cache: rejected token"); n != 2 {
		t.Fatalf("rejections logged %d times, want 2", n)
	}
}

// Discovery that failed is retried, at most once per discoveryRetry.
func TestIssuerDiscoveryIsRetriedWithBackoff(t *testing.T) {
	h := newHarness(t)
	h.is.down.Store(true)
	tok := h.is.token(t, "100", writeMain())
	if r := h.create(tok, "k", v1); r.status != http.StatusServiceUnavailable || r.code != twirpUnavailable {
		t.Fatalf("issuer down: %+v", r)
	}
	h.is.down.Store(false)
	for range 3 {
		if r := h.create(tok, "k", v1); r.status != http.StatusServiceUnavailable {
			t.Fatalf("within back-off: %+v", r)
		}
	}
	if n := h.is.discoveryHits.Load(); n != 1 {
		t.Fatalf("discovery tried %d times within the back-off", n)
	}
	h.clk.add(discoveryRetry)
	if r := h.create(tok, "k", v1); !r.ok() {
		t.Fatalf("after back-off: %+v", r)
	}
	h.create(tok, "k", v1)
	if n := h.is.discoveryHits.Load(); n != 2 {
		t.Fatalf("discovery tried %d times, want 2", n)
	}
}

// Bearer is matched exactly; other schemes are 401.
func TestAuthorizationScheme(t *testing.T) {
	h := newHarness(t)
	tok := h.is.token(t, "100", writeMain())
	for _, authz := range []string{"bearer " + tok, "Basic " + tok, "Bearer", tok} {
		r := h.callRawAuthz(authz, "CreateCacheEntry", []byte(`{"key":"k","version":"`+v1+`"}`))
		if r.status != http.StatusUnauthorized {
			t.Errorf("%.12q: %+v", authz, r)
		}
	}
}

// Other spellings of one signed token (whitespace inside it, a set
// padding bit in its signature) are rejected or carry the token's own
// TokenHash, so they cannot bind more than one upload reservation.
func TestTokenHashIsCanonical(t *testing.T) {
	h := newHarness(t)
	raw := h.is.token(t, "100", writeMain())
	id, err := h.srv.auth.verify(t.Context(), "Bearer "+raw)
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.srv.auth.verify(t.Context(), "Bearer "+raw)
	if err != nil || again.TokenHash != id.TokenHash {
		t.Fatalf("same token, another hash: %v", err)
	}
	for name, v := range map[string]string{
		"space":        raw[:10] + " " + raw[10:],
		"tab":          raw[:20] + "\t" + raw[20:],
		"trailing":     raw + " ",
		"padding bits": paddingVariant(t, raw),
	} {
		got, err := h.srv.auth.verify(t.Context(), "Bearer "+v)
		if err == nil && got.TokenHash != id.TokenHash {
			t.Errorf("%s: verified with another TokenHash", name)
		} else if err != nil && !errors.Is(err, errUnauthenticated) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func count(lines []string, prefix string) int {
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}
