package cache

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// trickle sends a request head declaring a body of n bytes, then the body
// one byte per gap, and returns the response status (0 when the server
// closed the connection without one) and how long the exchange took.
func trickle(t *testing.T, base, method, path string, header map[string]string, n int, gap time.Duration) (int, time.Duration) {
	t.Helper()
	u, _ := url.Parse(base)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	start := time.Now()
	conn.SetDeadline(start.Add(10 * time.Second))
	var head strings.Builder
	head.WriteString(method + " " + path + " HTTP/1.1\r\nHost: " + u.Host + "\r\nContent-Length: " + strconv.Itoa(n) + "\r\n")
	for k, v := range header {
		head.WriteString(k + ": " + v + "\r\n")
	}
	head.WriteString("\r\n")
	if _, err := io.WriteString(conn, head.String()); err != nil {
		t.Fatal(err)
	}
	go func() {
		for range n {
			time.Sleep(gap)
			if _, err := conn.Write([]byte("x")); err != nil {
				return
			}
		}
	}()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, time.Since(start)
	}
	resp.Body.Close()
	return resp.StatusCode, time.Since(start)
}

// shortWindows sets a 300 ms progress window with a quota of 3 bytes: a
// body at 1 byte per 30 ms (10 a window) is steady; 1 byte per 250 ms, a
// byte inside every window, is below the minimum rate.
func shortWindows(h *harness) {
	h.srv.progressWindow = 300 * time.Millisecond
	h.srv.progressBytes = 3
	h.srv.twirpReadTimeout = 300 * time.Millisecond
}

const (
	steadyGap  = 30 * time.Millisecond
	trickleGap = 250 * time.Millisecond
)

// An upload body above the minimum rate may take many windows; one that
// sends a byte within every window but less than the quota is cut after
// one window.
func TestUploadProgressDeadline(t *testing.T) {
	h := newHarness(t)
	shortWindows(h)
	tok := h.is.token(t, "100", writeMain())
	u, _ := url.Parse(h.reserve(tok, "k", v1))
	blob := map[string]string{"x-ms-blob-type": "BlockBlob"}
	if code, took := trickle(t, h.http.URL, http.MethodPut, u.RequestURI(), blob, 40, steadyGap); code != http.StatusCreated || took < 1100*time.Millisecond {
		t.Fatalf("steady body: %d after %v", code, took)
	}
	u2, _ := url.Parse(h.reserve(tok, "k2", v1))
	// 20 bytes at 250 ms would take 5 s; the cut comes after the first
	// window (≈ 300 ms).
	code, took := trickle(t, h.http.URL, http.MethodPut, u2.RequestURI(), blob, 20, trickleGap)
	if code == http.StatusCreated || took > 1500*time.Millisecond {
		t.Fatalf("trickle: %d after %v", code, took)
	}
	// Only the steady upload's blob and manifest are staged.
	if left := h.st.keys(h.srv.lay.stagingRoot()); len(left) != 2 {
		t.Fatalf("staged %q", left)
	}
}

// A Twirp body is read within an absolute deadline, with or without a
// token: a body arriving above the minimum-progress rate is still cut
// once twirpReadTimeout has passed.
func TestTwirpReadDeadline(t *testing.T) {
	h := newHarness(t)
	shortWindows(h)
	tok := h.is.token(t, "100", writeMain())
	path := twirpPrefix + "CreateCacheEntry"
	ct := map[string]string{"Content-Type": "application/json"}
	authed := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + tok}
	for name, hd := range map[string]map[string]string{"no token": ct, "token": authed} {
		// 40 bytes at steadyGap take 1.2 s and pass the progress rule; only
		// the absolute 300 ms deadline cuts them before 1 s.
		code, took := trickle(t, h.http.URL, http.MethodPost, path, hd, 40, steadyGap)
		if code == http.StatusOK || took > time.Second {
			t.Errorf("%s: %d after %v", name, code, took)
		}
	}
}

// Once a body has been read in full its read deadline is cleared, so a
// handler that works past the window afterwards (an upstream answering
// late, a slow Finalize copy) still answers.
func TestReadDeadlineClearedAfterBody(t *testing.T) {
	var upstreamErr atomic.Pointer[string]
	fail := func(err error) { s := err.Error(); upstreamErr.Store(&s) }
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			fail(err)
		}
		time.Sleep(900 * time.Millisecond)
		if err := r.Context().Err(); err != nil {
			fail(err)
		}
		io.WriteString(w, "done")
	}))
	defer up.Close()
	h := newHarness(t, func(o *Options) { o.Upstream = up.URL })
	shortWindows(h)

	t.Run("proxy", func(t *testing.T) {
		resp, err := http.Post(h.http.URL+"/twirp/x.ArtifactService/Upload", "application/octet-stream", strings.NewReader("0123456789"))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(b) != "done" {
			t.Fatalf("proxied call answering after 3 windows: %d %q", resp.StatusCode, b)
		}
		if err := upstreamErr.Load(); err != nil {
			t.Fatalf("the upstream saw %s", *err)
		}
	})
	t.Run("finalize", func(t *testing.T) {
		tok := h.is.token(t, "100", writeMain())
		h.putBlobOK(h.reserve(tok, "k", v1), []byte("data"))
		h.st.mu.Lock()
		h.st.assembleDelay = 900 * time.Millisecond
		h.st.mu.Unlock()
		if f := h.finalize(tok, "k", v1, 4); !f.ok() {
			t.Fatalf("finalize with a copy outlasting the Twirp deadline: %+v", f)
		}
	})
}

// A proxied request body is under the same minimum-progress rule, and the
// upstream sees the error rather than waiting for ever.
func TestProxyProgressDeadline(t *testing.T) {
	var upstreamErr atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			upstreamErr.Store(true)
		}
	}))
	defer up.Close()
	h := newHarness(t, func(o *Options) { o.Upstream = up.URL })
	shortWindows(h)
	if code, took := trickle(t, h.http.URL, http.MethodPost, "/twirp/x.ArtifactService/Upload", nil, 40, steadyGap); code != http.StatusOK || took < 1100*time.Millisecond {
		t.Fatalf("steady body: %d after %v", code, took)
	}
	code, took := trickle(t, h.http.URL, http.MethodPost, "/twirp/x.ArtifactService/Upload", nil, 20, trickleGap)
	if code == http.StatusOK || took > 1500*time.Millisecond {
		t.Fatalf("trickle: %d after %v", code, took)
	}
	// The upstream's read fails once the proxy aborts the forwarded body.
	deadline := time.Now().Add(5 * time.Second)
	for !upstreamErr.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !upstreamErr.Load() {
		t.Fatal("the upstream read the cut body without an error")
	}
}

// slowConn is a ResponseWriter whose client takes gap to accept each
// write, and which fails a write that ends past the write deadline, as a
// connection does.
type slowConn struct {
	http.ResponseWriter
	gap      time.Duration
	deadline time.Time
}

func (c *slowConn) SetWriteDeadline(t time.Time) error { c.deadline = t; return nil }

func (c *slowConn) Write(p []byte) (int, error) {
	time.Sleep(c.gap)
	if !c.deadline.IsZero() && time.Now().After(c.deadline) {
		return 0, errors.New("i/o timeout")
	}
	return len(p), nil
}

// A response is under the minimum-progress rule: a client reading above
// the rate takes as long as it needs; one accepting a write within every
// window but below the quota is cut after one window.
func TestResponseProgressDeadline(t *testing.T) {
	write := func(gap time.Duration, n int) (int, error) {
		c := &slowConn{ResponseWriter: httptest.NewRecorder(), gap: gap}
		w := newProgressWriter(c, http.NewResponseController(c), 300*time.Millisecond, 3)
		for i := range n {
			if _, err := w.Write([]byte("x")); err != nil {
				return i, err
			}
		}
		return n, nil
	}
	if n, err := write(steadyGap, 40); err != nil {
		t.Fatalf("steady reader cut after %d writes: %v", n, err)
	}
	// Two writes fit in the window; the third ends past it.
	if n, err := write(trickleGap, 20); err == nil || n > 2 {
		t.Fatalf("slow reader: %d writes, %v", n, err)
	}
}

// serveAs runs one pass-through request with the Authorization header
// authz (none when empty) from 192.0.2.1 in the background and returns
// its response once the handler returns.
func serveAs(h *harness, authz, path string) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "192.0.2.1:1000"
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		w := httptest.NewRecorder()
		h.srv.ServeHTTP(w, r)
		done <- w
	}()
	return done
}

// recv waits for a response from serveAs; a request the limiter wrongly
// admitted to a held upstream fails the test instead of hanging it.
func recv(t *testing.T, ch <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-ch:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("request did not finish: the limiter admitted it")
		return nil
	}
}

// heldUpstream answers /held only once release is called, which a test
// cleanup also does so that a failing test strands no handler.
func heldUpstream(t *testing.T) (url string, held *atomic.Int32, release func()) {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	held = &atomic.Int32{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/held" {
			held.Add(1)
			<-ch
		}
	}))
	t.Cleanup(up.Close)
	t.Cleanup(release)
	return up.URL, held, release
}

func waitHeld(t *testing.T, held *atomic.Int32, n int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for held.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d requests reached the upstream", held.Load(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// syncBuffer collects full log records, attributes included.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The pass-through limit is per job: two jobs behind one address are
// limited independently, requests without a valid token (none, or a bad
// signature) share one small bucket that does not touch the jobs' slots,
// and the server-wide ceiling applies across all of them. No token reaches
// the log.
func TestProxyConcurrencyLimits(t *testing.T) {
	upURL, held, release := heldUpstream(t)
	logs := &syncBuffer{}
	h := newHarness(t, func(o *Options) {
		o.Upstream = upURL
		o.Log = slog.New(slog.NewTextHandler(logs, nil))
	})
	// 2 per job, 1 anonymous, 5 in total.
	h.srv.proxies = newProxyLimiter(2, 1, 5)
	// Any repository's job counts as a job: there is no allowlist here.
	tokA := "Bearer " + h.is.token(t, "100", writeMain())
	tokB := "Bearer " + h.is.token(t, "999", writeMain())
	tokC := "Bearer " + h.is.token(t, "100", writePR())
	tokD := "Bearer " + h.is.token(t, "200", readMain())
	forged := "Bearer " + signJWT(t, mustKey(t), h.is.kid, h.is.claims(t, "100", writeMain()))

	a1, a2 := serveAs(h, tokA, "/held"), serveAs(h, tokA, "/held")
	waitHeld(t, held, 2)
	if w := recv(t, serveAs(h, tokA, "/other")); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("job over its limit: %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	if w := recv(t, serveAs(h, tokB, "/other")); w.Code != http.StatusOK {
		t.Fatalf("a second job behind the same address: %d", w.Code)
	}

	// One tokenless request fills the anonymous bucket; more tokenless or
	// forged requests are refused, and a job is still served.
	anon := serveAs(h, "", "/held")
	waitHeld(t, held, 3)
	if w := recv(t, serveAs(h, "", "/other")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("tokenless request over the anonymous bucket: %d", w.Code)
	}
	if w := recv(t, serveAs(h, forged, "/other")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a forged token escaped the anonymous bucket: %d", w.Code)
	}
	if w := recv(t, serveAs(h, tokB, "/other")); w.Code != http.StatusOK {
		t.Fatalf("a job during a tokenless flood: %d", w.Code)
	}

	// B and C take the last server-wide slots; D, under its own limit, is
	// refused by the ceiling.
	b, c := serveAs(h, tokB, "/held"), serveAs(h, tokC, "/held")
	waitHeld(t, held, 5)
	if w := recv(t, serveAs(h, tokD, "/other")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("over the server-wide ceiling: %d", w.Code)
	}
	release()
	for _, ch := range []<-chan *httptest.ResponseRecorder{a1, a2, anon, b, c} {
		if w := recv(t, ch); w.Code != http.StatusOK {
			t.Fatalf("held request: %d", w.Code)
		}
	}
	// Every slot was returned.
	for _, authz := range []string{tokA, ""} {
		if w := recv(t, serveAs(h, authz, "/other")); w.Code != http.StatusOK {
			t.Fatalf("after release: %d", w.Code)
		}
	}

	out := logs.String()
	for _, limit := range []string{"limit=job", "limit=anonymous", "limit=total"} {
		if !strings.Contains(out, limit) {
			t.Errorf("no 503 log line with %s in %q", limit, out)
		}
	}
	for _, tok := range []string{tokA, tokB, tokC, tokD, forged} {
		raw := strings.TrimPrefix(tok, "Bearer ")
		sig := raw[strings.LastIndex(raw, ".")+1:]
		if strings.Contains(out, sig) || strings.Contains(out, tokenHash(raw)) {
			t.Fatalf("a token or its hash was logged: %q", out)
		}
	}
}

func mustKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// A streamed upstream response reaches the client chunk by chunk, even
// with a declared length (ReverseProxy flushes unknown-length responses
// regardless of FlushInterval).
func TestProxyFlushesStreamedResponses(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len("first chunk\nrest\n")))
		io.WriteString(w, "first chunk\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "rest\n")
	}))
	defer up.Close()
	defer close(release)
	h := newHarness(t, func(o *Options) { o.Upstream = up.URL })
	line := make(chan string, 1)
	go func() {
		resp, err := http.Get(h.http.URL + "/stream")
		if err != nil {
			line <- err.Error()
			return
		}
		defer resp.Body.Close()
		l, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- l
	}()
	select {
	case l := <-line:
		if l != "first chunk\n" {
			t.Fatalf("read %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first chunk was not flushed before the upstream finished")
	}
}
