package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// listenHandler reports the bound address of each listener by its log message.
type listenHandler struct{ addrs chan [2]string }

func (h listenHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h listenHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h listenHandler) WithGroup(string) slog.Handler            { return h }
func (h listenHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "serving" && r.Message != "cache serving" {
		return nil
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "listen" {
			h.addrs <- [2]string{r.Message, a.Value.String()}
			return false
		}
		return true
	})
	return nil
}

func TestRunServesCacheOnSecondListener(t *testing.T) {
	// Static credentials keep the AWS default chain off the network; the
	// endpoint is closed, so the reaper's S3 calls fail fast.
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "upstream:"+r.URL.Path+":"+r.Header.Get("Authorization"))
	}))
	defer upstream.Close()
	keyFile := filepath.Join(t.TempDir(), "cache-signing-key")
	if err := os.WriteFile(keyFile, []byte(strings.Repeat("k", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, fakeLux(t, "v0.1.14"), closedGitHub, func(c map[string]any) {
		c["cache"] = map[string]any{
			"listen": "127.0.0.1:0", "publicURL": "http://127.0.0.1:1",
			"s3":             map[string]any{"bucket": "cache-bucket", "region": "us-east-1", "endpoint": "http://127.0.0.1:1"},
			"signingKeyFile": keyFile, "issuer": "http://127.0.0.1:1", "upstreamResultsURL": upstream.URL,
			"repositoryIDs": []int64{100},
		}
	})
	addrs := make(chan [2]string, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, path, slog.New(listenHandler{addrs})) }()
	got := map[string]string{}
	for len(got) < 2 {
		select {
		case a := <-addrs:
			got[a[0]] = "http://" + a[1]
		case err := <-done:
			t.Fatalf("run returned before serving: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatalf("listeners %v", got)
		}
	}
	webhook, cacheBase := got["serving"], got["cache serving"]
	if webhook == cacheBase {
		t.Fatal("one listener for both")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	post := func(base, path, auth string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	cachePath := "/twirp/github.actions.results.api.v1.CacheService/GetCacheEntryDownloadURL"
	if code, body := post(cacheBase, cachePath, ""); code != http.StatusUnauthorized || !strings.Contains(body, `"unauthenticated"`) {
		t.Fatalf("cache without a token: %d %s", code, body)
	}
	if code, body := post(cacheBase, "/twirp/x.ArtifactService/Create", "Bearer t"); code != http.StatusOK || body != "upstream:/twirp/x.ArtifactService/Create:Bearer t" {
		t.Fatalf("pass-through: %d %s", code, body)
	}
	// The webhook listener serves no cache or pass-through route, and the
	// cache listener does not serve /status.
	if code, _ := post(webhook, cachePath, ""); code != http.StatusNotFound {
		t.Fatalf("webhook listener answered a cache request: %d", code)
	}
	resp, err := client.Get(cacheBase + "/status")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(string(b), "upstream:/status") {
		t.Fatalf("cache listener /status: %s", b)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown err=%v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
	if _, err := client.Get(cacheBase + "/x"); err == nil {
		t.Fatal("cache still serving after shutdown")
	}
}

// A stop while a transfer outlasts the drain closes the cache listener,
// logs a warning, and returns no error.
func TestRunStopsCleanlyWithATransferInFlight(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	old := shutdownTimeout
	shutdownTimeout = 300 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = old })
	// The upstream reads the proxied body slowly, so the transfer is still
	// running when the stop arrives.
	inFlight := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inFlight <- struct{}{}
		io.Copy(io.Discard, r.Body)
	}))
	defer upstream.Close()
	keyFile := filepath.Join(t.TempDir(), "cache-signing-key")
	os.WriteFile(keyFile, []byte(strings.Repeat("k", 32)+"\n"), 0o600)
	path := writeConfig(t, fakeLux(t, "v0.1.14"), closedGitHub, func(c map[string]any) {
		c["cache"] = map[string]any{
			"listen": "127.0.0.1:0", "publicURL": "http://127.0.0.1:1",
			"s3":             map[string]any{"bucket": "cache-bucket", "region": "us-east-1", "endpoint": "http://127.0.0.1:1"},
			"signingKeyFile": keyFile, "issuer": "http://127.0.0.1:1", "upstreamResultsURL": upstream.URL,
			"repositoryIDs": []int64{100},
		}
	})
	addrs := make(chan [2]string, 2)
	warn := &warnRecorder{listenHandler: listenHandler{addrs}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, path, slog.New(warn)) }()
	var cacheBase string
	for cacheBase == "" {
		select {
		case a := <-addrs:
			if a[0] == "cache serving" {
				cacheBase = a[1]
			}
		case err := <-done:
			t.Fatalf("run returned: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("cache listener did not start")
		}
	}
	// A body that keeps trickling: never finishes on its own.
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() {
		for {
			if _, err := pw.Write([]byte("x")); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://"+cacheBase+"/upload", pr)
		req.ContentLength = 1 << 30
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-inFlight:
	case <-time.After(10 * time.Second):
		t.Fatal("transfer did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v on a normal stop", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return")
	}
	if !warn.saw("cache listener: transfers still running at shutdown were dropped") {
		t.Fatal("no warning about dropped transfers")
	}
}

type warnRecorder struct {
	listenHandler
	mu   sync.Mutex
	msgs []string
}

func (w *warnRecorder) Handle(ctx context.Context, r slog.Record) error {
	w.mu.Lock()
	w.msgs = append(w.msgs, r.Message)
	w.mu.Unlock()
	return w.listenHandler.Handle(ctx, r)
}

func (w *warnRecorder) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnRecorder) WithGroup(string) slog.Handler      { return w }

func (w *warnRecorder) saw(msg string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, m := range w.msgs {
		if m == msg {
			return true
		}
	}
	return false
}

func TestRunRefusesCacheWithBadSigningKey(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "short")
	os.WriteFile(keyFile, []byte("short\n"), 0o600)
	path := writeConfig(t, fakeLux(t, "v0.1.14"), closedGitHub, func(c map[string]any) {
		c["cache"] = map[string]any{
			"listen": "127.0.0.1:0", "publicURL": "http://127.0.0.1:1",
			"s3": map[string]any{"bucket": "cache-bucket", "region": "us-east-1"}, "signingKeyFile": keyFile,
			"repositoryOwnerIDs": []int64{7},
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := run(ctx, path, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "cache.signingKeyFile") || strings.Contains(err.Error(), "short\n") {
		t.Fatalf("err=%v", err)
	}
}
