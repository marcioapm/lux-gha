package cache

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is a settable time shared by the server and the memory store.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type harness struct {
	t     *testing.T
	is    *testIssuer
	st    *memStore
	clk   *clock
	srv   *Server
	http  *httptest.Server
	proto bool
}

func newHarness(t *testing.T, edit ...func(*Options)) *harness {
	t.Helper()
	h := &harness{t: t, is: newTestIssuer(t), clk: &clock{t: time.Now()}}
	h.st = newMemStore(h.clk.now)
	o := Options{
		PublicURL:   "http://placeholder.invalid",
		Issuer:      h.is.URL,
		Upstream:    "http://upstream.invalid",
		SigningKeys: [][]byte{bytes.Repeat([]byte("k"), 32)},
		Prefix:      "gha-cache",
		Now:         h.clk.now,
	}
	for _, e := range edit {
		e(&o)
	}
	h.http = httptest.NewUnstartedServer(nil)
	h.http.Start()
	t.Cleanup(h.http.Close)
	o.PublicURL = h.http.URL
	srv, err := newServer(h.st, o)
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	h.http.Config.Handler = srv
	return h
}

type twirpResult struct {
	status int
	body   map[string]any
	code   string
	msg    string
}

// call posts a Twirp request as JSON (or protobuf when h.proto) and
// decodes the reply into a generic map with proto field names.
func (h *harness) call(token, method string, req map[string]any) twirpResult {
	h.t.Helper()
	var body []byte
	ct := "application/json"
	if h.proto {
		ct = "application/protobuf"
		body = encodeProtoRequest(h.t, method, req)
	} else {
		body, _ = json.Marshal(req)
	}
	r, _ := http.NewRequest(http.MethodPost, h.http.URL+twirpPrefix+method, bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	res := twirpResult{status: resp.StatusCode, body: map[string]any{}}
	if resp.StatusCode != http.StatusOK {
		var e struct{ Code, Msg string }
		if err := json.Unmarshal(raw, &e); err != nil {
			h.t.Fatalf("%s: non-JSON Twirp error %q", method, raw)
		}
		res.code, res.msg = e.Code, e.Msg
		return res
	}
	if h.proto {
		res.body = decodeProtoResponse(h.t, method, raw)
	} else if err := json.Unmarshal(raw, &res.body); err != nil {
		h.t.Fatal(err)
	}
	return res
}

// encodeProtoRequest and decodeProtoResponse are the test's own protobuf
// codec for the generic maps, built from the package's field helpers.
func encodeProtoRequest(t *testing.T, method string, req map[string]any) []byte {
	var b []byte
	b = pbAppendString(b, 2, str(req["key"]))
	switch method {
	case "CreateCacheEntry":
		b = pbAppendString(b, 3, str(req["version"]))
	case "FinalizeCacheEntryUpload":
		var n int64
		fmt.Sscan(str(req["size_bytes"]), &n)
		b = pbAppendInt64(b, 3, n)
		b = pbAppendString(b, 4, str(req["version"]))
	case "GetCacheEntryDownloadURL":
		if rk, ok := req["restore_keys"].([]string); ok {
			for _, k := range rk {
				b = pbAppendString(b, 3, k)
				if k == "" {
					t.Fatal("empty restore key")
				}
			}
		}
		b = pbAppendString(b, 4, str(req["version"]))
	}
	return b
}

func decodeProtoResponse(t *testing.T, method string, raw []byte) map[string]any {
	names := map[string][4]string{
		"CreateCacheEntry":         {"", "ok", "signed_upload_url", "message"},
		"FinalizeCacheEntryUpload": {"", "ok", "entry_id", "message"},
		"GetCacheEntryDownloadURL": {"", "ok", "signed_download_url", "matched_key"},
	}[method]
	out := map[string]any{}
	err := pbFields(raw, func(f pbField) error {
		name := names[f.num]
		switch {
		case f.num == 1:
			out[name] = f.u == 1
		case f.wire == wireVarint:
			out[name] = fmt.Sprint(f.u)
		default:
			out[name] = string(f.bytes)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func (r twirpResult) ok() bool { return r.status == http.StatusOK && r.body["ok"] == true }

func (h *harness) create(token, key, version string) twirpResult {
	return h.call(token, "CreateCacheEntry", map[string]any{"key": key, "version": version})
}

func (h *harness) finalize(token, key, version string, size int) twirpResult {
	return h.call(token, "FinalizeCacheEntryUpload", map[string]any{"key": key, "version": version, "size_bytes": fmt.Sprint(size)})
}

func (h *harness) restore(token, key, version string, restoreKeys ...string) twirpResult {
	req := map[string]any{"key": key, "version": version}
	if len(restoreKeys) > 0 {
		req["restore_keys"] = restoreKeys
	}
	return h.call(token, "GetCacheEntryDownloadURL", req)
}

// put sends one Azure Blob request to the signed upload URL.
func (h *harness) put(uploadURL, query string, header map[string]string, body []byte) (*http.Response, string) {
	h.t.Helper()
	u := uploadURL
	if query != "" {
		u += "&" + query
	}
	r, _ := http.NewRequest(http.MethodPut, u, bytes.NewReader(body))
	for k, v := range header {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (h *harness) putBlobOK(uploadURL string, data []byte) {
	h.t.Helper()
	resp, body := h.put(uploadURL, "", map[string]string{"x-ms-blob-type": "BlockBlob"}, data)
	if resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("put blob: %d %s", resp.StatusCode, body)
	}
}

// save runs Create, a single-shot upload and Finalize.
func (h *harness) save(token, key, version string, data []byte) {
	h.t.Helper()
	c := h.create(token, key, version)
	if !c.ok() {
		h.t.Fatalf("create %q: %+v", key, c)
	}
	h.putBlobOK(str(c.body["signed_upload_url"]), data)
	if f := h.finalize(token, key, version, len(data)); !f.ok() {
		h.t.Fatalf("finalize %q: %+v", key, f)
	}
}

// restored returns the entry content behind a download URL from memStore.
func (h *harness) restored(r twirpResult) []byte {
	h.t.Helper()
	u, err := url.Parse(str(r.body["signed_download_url"]))
	if err != nil {
		h.t.Fatal(err)
	}
	key, _ := url.PathUnescape(strings.TrimPrefix(u.EscapedPath(), "/cache-bucket/"))
	b, ok := h.st.get(key)
	if !ok {
		h.t.Fatalf("download URL names a missing object %q", key)
	}
	return b
}

var v1 = strings.Repeat("1", 64)
var v2 = strings.Repeat("2", 64)

func TestSaveAndRestoreExact(t *testing.T) {
	for _, proto := range []bool{false, true} {
		t.Run(fmt.Sprintf("proto=%v", proto), func(t *testing.T) {
			h := newHarness(t)
			h.proto = proto
			tok := h.is.token(t, "100", writeMain())
			h.save(tok, "deps-linux-abc", v1, []byte("archive"))
			r := h.restore(tok, "deps-linux-abc", v1)
			if !r.ok() || r.body["matched_key"] != "deps-linux-abc" {
				t.Fatalf("restore: %+v", r)
			}
			if got := h.restored(r); string(got) != "archive" {
				t.Fatalf("content %q", got)
			}
			if !strings.Contains(str(r.body["signed_download_url"]), "ttl=10m0s") {
				t.Fatalf("download URL TTL: %v", r.body["signed_download_url"])
			}
		})
	}
}

func TestRestoreMissAndVersionMismatchAreOkFalse(t *testing.T) {
	h := newHarness(t)
	tok := h.is.token(t, "100", writeMain())
	h.save(tok, "k", v1, []byte("x"))
	for _, r := range []twirpResult{h.restore(tok, "other", v1), h.restore(tok, "k", v2, "k")} {
		if r.status != http.StatusOK || r.body["ok"] != nil {
			t.Fatalf("want a miss, got %+v", r)
		}
	}
}

func TestRestoreKeysPickNewestPrefixMatchAndExactWins(t *testing.T) {
	h := newHarness(t)
	tok := h.is.token(t, "100", writeMain())
	h.save(tok, "deps-linux-old", v1, []byte("old"))
	h.clk.add(time.Minute)
	h.save(tok, "deps-linux-new", v1, []byte("new"))
	h.clk.add(time.Minute)
	h.save(tok, "deps-other", v1, []byte("other"))

	r := h.restore(tok, "deps-linux-missing", v1, "deps-linux-", "deps-")
	if !r.ok() || r.body["matched_key"] != "deps-linux-new" || string(h.restored(r)) != "new" {
		t.Fatalf("restore keys: %+v", r)
	}
	// The primary key is itself a prefix: the newest entry under it.
	r = h.restore(tok, "deps-linux", v1)
	if r.body["matched_key"] != "deps-linux-new" {
		t.Fatalf("primary prefix: %+v", r)
	}
	// An exact match wins over a newer prefix match.
	r = h.restore(tok, "deps-linux-old", v1, "deps-")
	if r.body["matched_key"] != "deps-linux-old" {
		t.Fatalf("exact: %+v", r)
	}
	// Restore keys are tried in order: the first that matches wins even if
	// a later one matches something newer.
	r = h.restore(tok, "none", v1, "deps-linux-o", "deps-")
	if r.body["matched_key"] != "deps-linux-old" {
		t.Fatalf("order: %+v", r)
	}
}

func TestCreateOnExistingEntryIsOkFalse(t *testing.T) {
	h := newHarness(t)
	tok := h.is.token(t, "100", writeMain())
	h.save(tok, "k", v1, []byte("x"))
	r := h.create(tok, "k", v1)
	if r.status != http.StatusOK || r.ok() || !strings.Contains(str(r.body["message"]), "already exists") {
		t.Fatalf("%+v", r)
	}
}

func TestInvalidRequestsAreTwirpErrors(t *testing.T) {
	h := newHarness(t)
	tok := h.is.token(t, "100", writeMain())
	for _, c := range []struct {
		r    twirpResult
		code string
	}{
		{h.create(tok, "", v1), twirpInvalidArgument},
		{h.create(tok, "a,b", v1), twirpInvalidArgument},
		{h.create(tok, strings.Repeat("k", 513), v1), twirpInvalidArgument},
		{h.create(tok, "k", "not-a-version"), twirpInvalidArgument},
		{h.restore(tok, "k", v1, "1", "2", "3", "4", "5", "6", "7", "8", "9", "10"), twirpInvalidArgument},
		{h.call(tok, "DeleteCacheEntry", map[string]any{}), twirpBadRoute},
	} {
		if c.r.code != c.code || c.r.status != twirpStatus[c.code] {
			t.Errorf("got %+v, want %s", c.r, c.code)
		}
	}
	for _, ct := range []string{"text/plain", ""} {
		r, _ := http.NewRequest(http.MethodPost, h.http.URL+twirpPrefix+"CreateCacheEntry", strings.NewReader("{}"))
		r.Header.Set("Content-Type", ct)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("content type %q: %d", ct, resp.StatusCode)
		}
	}
	resp, err := http.Post(h.http.URL+twirpPrefix+"CreateCacheEntry", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		// Authentication precedes decoding: no token, no parsing.
		t.Errorf("malformed body without a token: %d", resp.StatusCode)
	}
	r := h.callRaw(tok, "CreateCacheEntry", "application/json", []byte("{not json"))
	if r.code != twirpMalformed {
		t.Errorf("malformed JSON: %+v", r)
	}
	r = h.callRaw(tok, "CreateCacheEntry", "application/protobuf", []byte{0x12, 0x05})
	if r.code != twirpMalformed {
		t.Errorf("malformed protobuf: %+v", r)
	}
}

func (h *harness) callRaw(token, method, ct string, body []byte) twirpResult {
	h.t.Helper()
	r, _ := http.NewRequest(http.MethodPost, h.http.URL+twirpPrefix+method, bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)
	r.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var e struct{ Code, Msg string }
	json.NewDecoder(resp.Body).Decode(&e)
	return twirpResult{status: resp.StatusCode, code: e.Code, msg: e.Msg}
}

// Every token or claim defect is a 401 Twirp unauthenticated error.
func TestRejectsInvalidRuntimeTokens(t *testing.T) {
	h := newHarness(t)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	valid := func() map[string]any { return h.is.claims(t, "100", writeMain()) }
	with := func(edit func(map[string]any)) string {
		c := valid()
		edit(c)
		return h.is.sign(t, c)
	}
	good := h.is.token(t, "100", writeMain())
	parts := strings.Split(good, ".")
	cases := map[string]string{
		"no token":      "",
		"not a JWT":     "garbage",
		"wrong issuer":  with(func(c map[string]any) { c["iss"] = "https://token.example.test" }),
		"bad signature": signJWT(t, otherKey, h.is.kid, valid()),
		"unknown kid":   signJWT(t, otherKey, "other-kid", valid()),
		"tampered claims": parts[0] + "." + b64([]byte(strings.Replace(
			string(mustB64(t, parts[1])), `"100"`, `"200"`, 1))) + "." + parts[2],
		"alg none":              b64([]byte(`{"alg":"none"}`)) + "." + parts[1] + ".",
		"expired":               with(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }),
		"not yet valid":         with(func(c map[string]any) { c["nbf"] = time.Now().Add(time.Hour).Unix() }),
		"missing ac":            with(func(c map[string]any) { delete(c, "ac") }),
		"ac not a string":       with(func(c map[string]any) { c["ac"] = []any{map[string]any{"Scope": mainScope, "Permission": 3}} }),
		"ac garbled":            with(func(c map[string]any) { c["ac"] = "[{" }),
		"ac entry no scope":     with(func(c map[string]any) { c["ac"] = `[{"Permission":3}]` }),
		"missing repository_id": with(func(c map[string]any) { delete(c, "repository_id") }),
		"numeric repository_id": with(func(c map[string]any) { c["repository_id"] = 100 }),
		"path repository_id":    with(func(c map[string]any) { c["repository_id"] = "../100" }),
	}
	for name, tok := range cases {
		for _, method := range []string{"CreateCacheEntry", "GetCacheEntryDownloadURL", "FinalizeCacheEntryUpload"} {
			r := h.call(tok, method, map[string]any{"key": "k", "version": v1, "size_bytes": "1"})
			if r.status != http.StatusUnauthorized || r.code != twirpUnauthenticated {
				t.Errorf("%s %s: %+v", name, method, r)
			}
		}
	}
	if r := h.create(good, "k", v1); !r.ok() {
		t.Fatalf("the valid token was rejected: %+v", r)
	}
}

func mustB64(t *testing.T, s string) []byte {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestIssuerUnavailableIs503(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Issuer = "http://127.0.0.1:1" })
	r := h.create(h.is.token(t, "100", writeMain()), "k", v1)
	if r.status != http.StatusServiceUnavailable || r.code != twirpUnavailable {
		t.Fatalf("%+v", r)
	}
}

func TestReadOnlyTokenCannotSave(t *testing.T) {
	h := newHarness(t)
	ro := h.is.token(t, "100", readMain())
	// Create answers ok:false with the prefix the client classifies as a
	// policy denial; Finalize, which a client never reaches without a
	// successful Create, is a permission_denied error.
	if r := h.create(ro, "k", v1); r.status != http.StatusOK || r.ok() || !strings.HasPrefix(str(r.body["message"]), "cache write denied:") {
		t.Fatalf("create: %+v", r)
	}
	if r := h.finalize(ro, "k", v1, 1); r.status != http.StatusForbidden || r.code != twirpPermissionDenied || !strings.HasPrefix(r.msg, "cache write denied:") {
		t.Fatalf("finalize: %+v", r)
	}
	// It may still restore.
	h.save(h.is.token(t, "100", writeMain()), "k", v1, []byte("x"))
	if r := h.restore(ro, "k", v1); !r.ok() {
		t.Fatalf("read-only restore: %+v", r)
	}
}

func TestTokenWithoutReadScopeCannotRestore(t *testing.T) {
	h := newHarness(t)
	r := h.restore(h.is.token(t, "100", scopeGrant{mainScope, permWrite}), "k", v1)
	if r.status != http.StatusForbidden || !strings.HasPrefix(r.msg, "cache read denied:") {
		t.Fatalf("%+v", r)
	}
	r = h.restore(h.is.token(t, "100"), "k", v1)
	if r.status != http.StatusForbidden {
		t.Fatalf("empty ac: %+v", r)
	}
}

func TestRepositoriesAreIsolated(t *testing.T) {
	h := newHarness(t)
	h.save(h.is.token(t, "100", writeMain()), "k", v1, []byte("repo-100"))
	other := h.is.token(t, "200", writeMain())
	if r := h.restore(other, "k", v1, "k"); r.body["ok"] != nil {
		t.Fatalf("cross-repo restore hit: %+v", r)
	}
	// Repo 200 saving the same key creates its own entry.
	h.save(other, "k", v1, []byte("repo-200"))
	if got := h.restored(h.restore(h.is.token(t, "100", readMain()), "k", v1)); string(got) != "repo-100" {
		t.Fatalf("repo 100 reads %q", got)
	}
}

func TestScopeVisibility(t *testing.T) {
	h := newHarness(t)
	prTok := h.is.token(t, "100", writePR(), readMain())
	mainTok := h.is.token(t, "100", writeMain())
	h.save(prTok, "pr-only", v1, []byte("pr"))
	// A main-scope reader without the PR scope does not see PR entries.
	if r := h.restore(mainTok, "pr-only", v1, "pr-"); r.body["ok"] != nil {
		t.Fatalf("main reader saw a PR entry: %+v", r)
	}
	// The PR job reads main's entries, and prefers its own scope.
	h.save(mainTok, "shared", v1, []byte("from-main"))
	if r := h.restore(prTok, "shared", v1); string(h.restored(r)) != "from-main" {
		t.Fatalf("PR read of main: %+v", r)
	}
	h.save(prTok, "shared", v1, []byte("from-pr"))
	if r := h.restore(prTok, "shared", v1); string(h.restored(r)) != "from-pr" {
		t.Fatalf("PR scope priority: %+v", r)
	}
	// Scope order: a prefix hit in the write scope beats an exact hit in
	// a lower-priority scope, as GitHub searches scope by scope.
	h.save(mainTok, "lib-1", v1, []byte("main-exact"))
	h.save(prTok, "lib-10", v1, []byte("pr-prefix"))
	if r := h.restore(prTok, "lib-1", v1); string(h.restored(r)) != "pr-prefix" {
		t.Fatalf("scope priority over exactness: %+v", r)
	}
}

func TestReadScopeOrder(t *testing.T) {
	h := newHarness(t)
	id, err := h.srv.auth.verify(context.Background(), "Bearer "+h.is.token(t, "100",
		scopeGrant{"refs/heads/base", permRead}, readMain(), writePR(), scopeGrant{"refs/heads/w", permWrite}))
	if err != nil {
		t.Fatal(err)
	}
	if id.WriteScope != prScope || strings.Join(id.ReadScopes, " ") != prScope+" refs/heads/base "+mainScope {
		t.Fatalf("%+v", id)
	}
}
