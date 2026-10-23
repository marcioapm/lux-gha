package cache

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
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

// A Twirp body is read within its own short deadline, with or without a
// token.
func TestTwirpReadDeadline(t *testing.T) {
	h := newHarness(t)
	shortWindows(h)
	tok := h.is.token(t, "100", writeMain())
	path := twirpPrefix + "CreateCacheEntry"
	ct := map[string]string{"Content-Type": "application/json"}
	authed := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + tok}
	for name, hd := range map[string]map[string]string{"no token": ct, "token": authed} {
		code, took := trickle(t, h.http.URL, http.MethodPost, path, hd, 100, 100*time.Millisecond)
		if code == http.StatusOK || took > 3*time.Second {
			t.Errorf("%s: %d after %v", name, code, took)
		}
	}
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

// serveAs runs one pass-through request from addr in the background and
// returns its response once the handler returns.
func serveAs(h *harness, addr, path string) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		h.srv.ServeHTTP(w, r)
		done <- w
	}()
	return done
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

// One client address at its pass-through limit gets 503 with Retry-After,
// a second address is still served, and the server-wide ceiling applies
// across addresses. Forwarded headers do not name the client.
func TestProxyConcurrencyLimits(t *testing.T) {
	release := make(chan struct{})
	var held atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/held" {
			held.Add(1)
			<-release
		}
	}))
	defer up.Close()
	h := newHarness(t, func(o *Options) { o.Upstream = up.URL })
	h.srv.proxies = newProxyLimiter(2, 3)
	a1, a2 := serveAs(h, "192.0.2.1:1000", "/held"), serveAs(h, "192.0.2.1:1001", "/held")
	waitHeld(t, &held, 2)

	full := <-serveAs(h, "192.0.2.1:1002", "/other")
	if full.Code != http.StatusServiceUnavailable || full.Header().Get("Retry-After") == "" {
		t.Fatalf("client over its limit: %d, Retry-After %q", full.Code, full.Header().Get("Retry-After"))
	}
	r := httptest.NewRequest(http.MethodGet, "/other", nil)
	r.RemoteAddr = "192.0.2.1:1003"
	r.Header.Set("X-Forwarded-For", "198.51.100.9")
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a forwarded header escaped the client limit: %d", w.Code)
	}
	if other := <-serveAs(h, "192.0.2.2:1000", "/other"); other.Code != http.StatusOK {
		t.Fatalf("second client: %d", other.Code)
	}

	// The second address takes the last server-wide slot; a third is refused.
	b := serveAs(h, "192.0.2.2:1001", "/held")
	waitHeld(t, &held, 3)
	if c := <-serveAs(h, "192.0.2.3:1000", "/other"); c.Code != http.StatusServiceUnavailable {
		t.Fatalf("over the server-wide ceiling: %d", c.Code)
	}
	close(release)
	for _, ch := range []<-chan *httptest.ResponseRecorder{a1, a2, b} {
		if w := <-ch; w.Code != http.StatusOK {
			t.Fatalf("held request: %d", w.Code)
		}
	}
	// Every slot was returned.
	if c := <-serveAs(h, "192.0.2.1:1004", "/other"); c.Code != http.StatusOK {
		t.Fatalf("after release: %d", c.Code)
	}
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
