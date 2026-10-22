package cache

import (
	"bufio"
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

func shortWindows(h *harness) {
	h.srv.idleTimeout = 300 * time.Millisecond
	h.srv.twirpReadTimeout = 300 * time.Millisecond
}

// An upload body that keeps making progress may take longer than the idle
// window; one that stalls longer than the window is cut.
func TestUploadIdleDeadline(t *testing.T) {
	h := newHarness(t)
	shortWindows(h)
	tok := h.is.token(t, "100", writeMain())
	u, _ := url.Parse(h.reserve(tok, "k", v1))
	path := u.RequestURI()
	blob := map[string]string{"x-ms-blob-type": "BlockBlob"}
	if code, took := trickle(t, h.http.URL, http.MethodPut, path, blob, 10, 100*time.Millisecond); code != http.StatusCreated || took < 900*time.Millisecond {
		t.Fatalf("steady trickle: %d after %v", code, took)
	}
	u2, _ := url.Parse(h.reserve(tok, "k2", v1))
	code, took := trickle(t, h.http.URL, http.MethodPut, u2.RequestURI(), blob, 10, time.Second)
	if code == http.StatusCreated || took > 3*time.Second {
		t.Fatalf("stalled trickle: %d after %v", code, took)
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

// A proxied request body that stalls is cut, and the upstream sees the
// error rather than waiting for ever.
func TestProxyIdleDeadline(t *testing.T) {
	var upstreamErr atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			upstreamErr.Store(true)
		}
	}))
	defer up.Close()
	h := newHarness(t, func(o *Options) { o.Upstream = up.URL })
	shortWindows(h)
	if code, took := trickle(t, h.http.URL, http.MethodPost, "/twirp/x.ArtifactService/Upload", nil, 10, 50*time.Millisecond); code != http.StatusOK || took < 450*time.Millisecond {
		t.Fatalf("steady trickle: %d after %v", code, took)
	}
	code, took := trickle(t, h.http.URL, http.MethodPost, "/twirp/x.ArtifactService/Upload", nil, 10, time.Second)
	if code == http.StatusOK || took > 3*time.Second {
		t.Fatalf("stalled trickle: %d after %v", code, took)
	}
}

// Pass-through requests beyond the concurrency limit get 503.
func TestProxyConcurrencyLimit(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer up.Close()
	defer close(release)
	h := newHarness(t, func(o *Options) { o.Upstream = up.URL })
	h.srv.proxies = make(chan struct{}, 1)
	go http.Get(h.http.URL + "/held")
	deadline := time.Now().Add(5 * time.Second)
	for len(h.srv.proxies) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := http.Get(h.http.URL + "/second")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("%d", resp.StatusCode)
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
