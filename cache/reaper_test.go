package cache

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func obj(key string, size int64, age time.Duration, now time.Time) object {
	return object{Key: key, Size: size, LastModified: now.Add(-age)}
}

func keysOf(objs []object) string {
	var ks []string
	for _, o := range objs {
		ks = append(ks, o.Key)
	}
	return strings.Join(ks, " ")
}

func TestSelectEntries(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	entries := []object{
		obj("fresh", 10, time.Hour, now),
		obj("expired", 10, 8*day, now),
		obj("older", 30, 3*day, now),
		obj("old", 30, 2*day, now),
		obj("new", 30, day, now),
	}
	if got := keysOf(selectEntries(entries, now, 7*day, 0)); got != "expired" {
		t.Fatalf("ttl only: %q", got)
	}
	// 100 bytes kept against a cap of 100: under it, nothing more.
	if got := keysOf(selectEntries(entries, now, 7*day, 100)); got != "expired" {
		t.Fatalf("at cap: %q", got)
	}
	// Cap 90 → target 81: delete oldest-first until ≤ 81 (100 → 70).
	if got := keysOf(selectEntries(entries, now, 7*day, 90)); got != "expired older" {
		t.Fatalf("over cap: %q", got)
	}
	// Cap 50 → target 45: 100 → 70 → 40.
	if got := keysOf(selectEntries(entries, now, 7*day, 50)); got != "expired older old" {
		t.Fatalf("well over cap: %q", got)
	}
	if got := selectEntries(nil, now, day, 1); len(got) != 0 {
		t.Fatalf("empty: %v", got)
	}
	// An entry of MaxEntryBytesFor(cap), the largest config admits, survives
	// the pass right after it is saved.
	big := MaxEntryBytesFor(100)
	if got := keysOf(selectEntries([]object{obj("a", 11, time.Hour, now), obj("just-saved", big, 0, now)}, now, 7*day, 100)); got != "a" {
		t.Fatalf("largest entry: %q", got)
	}
}

func TestSelectStagingKeepsLiveReservationsWhole(t *testing.T) {
	now := time.Now()
	root := "p/staging/"
	objs := []object{
		obj(root+"abandoned/block/01", 1, 30*time.Hour, now),
		obj(root+"abandoned/manifest", 1, 25*time.Hour, now),
		// An old block of an upload still receiving blocks stays.
		obj(root+"live/block/01", 1, 30*time.Hour, now),
		obj(root+"live/block/02", 1, time.Minute, now),
		obj(root+"recent/blob", 1, time.Hour, now),
	}
	got := strings.Join(selectStaging(objs, root, now, stagingMaxAge), " ")
	if got != root+"abandoned/block/01 "+root+"abandoned/manifest" {
		t.Fatalf("%q", got)
	}
}

type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *logRecorder) WithGroup(string) slog.Handler            { return l }
func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, r.Message)
	return nil
}

func (l *logRecorder) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func TestReaperPass(t *testing.T) {
	logs := &logRecorder{}
	// MaxBytes 15: after the expired entry, 30 bytes remain; the oldest go
	// until at most 14 (90 %) remain, which takes "oldest" and "stuck".
	h := newHarness(t, func(o *Options) { o.MaxBytes = 15; o.Log = slog.New(logs) })
	now := h.clk.now()
	l := h.srv.lay
	day := 24 * time.Hour
	keep := l.entryKey("1", "s", v1, "keep")
	h.st.setAt(l.entryKey("1", "s", v1, "expired"), []byte("x"), now.Add(-8*day))
	h.st.setAt(l.entryKey("1", "s", v1, "oldest"), bytes.Repeat([]byte("x"), 10), now.Add(-3*day))
	h.st.setAt(l.entryKey("2", "s", v1, "stuck"), bytes.Repeat([]byte("x"), 10), now.Add(-2*day))
	h.st.setAt(keep, bytes.Repeat([]byte("x"), 10), now.Add(-day))
	h.st.setAt(l.stagingRoot()+"old/blob", []byte("x"), now.Add(-25*time.Hour))
	h.st.setAt(l.stagingRoot()+"new/blob", []byte("x"), now.Add(-time.Hour))
	// Outside the prefix: never touched.
	h.st.setAt("elsewhere/v1/1/x", []byte("x"), now.Add(-30*day))
	h.st.uploads = []pendingUpload{
		{Key: l.entryKey("1", "s", v1, "a"), UploadID: "stale", Initiated: now.Add(-25 * time.Hour)},
		{Key: l.entryKey("1", "s", v1, "b"), UploadID: "live", Initiated: now.Add(-time.Hour)},
	}
	// "stuck" fails to delete this pass.
	h.st.failDelete = map[string]bool{l.entryKey("2", "s", v1, "stuck"): true}

	res, ran := h.srv.reaper.pass(context.Background())
	if !ran {
		t.Fatal("pass did not run")
	}
	want := reapResult{Entries: 4, Bytes: 31, DeletedEntries: 2, DeletedBytes: 11, DeletedStaging: 1, AbortedUploads: 1, Errors: 1}
	if res != want {
		t.Fatalf("result %+v, want %+v", res, want)
	}
	left := strings.Join(h.st.keys(""), " ")
	for _, k := range []string{keep, l.entryKey("2", "s", v1, "stuck"), l.stagingRoot() + "new/blob", "elsewhere/v1/1/x"} {
		if !strings.Contains(left, k) {
			t.Errorf("%s was deleted", k)
		}
	}
	if len(h.st.uploads) != 1 || h.st.uploads[0].UploadID != "live" {
		t.Errorf("uploads %+v", h.st.uploads)
	}
	if !contains(logs.lines, "cache reaper pass") {
		t.Errorf("no summary line: %q", logs.lines)
	}
	// The failed delete is retried next pass.
	h.st.failDelete = nil
	res, _ = h.srv.reaper.pass(context.Background())
	if res.DeletedEntries != 1 || res.Errors != 0 {
		t.Fatalf("second pass %+v", res)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestReaperIsSingleFlight(t *testing.T) {
	h := newHarness(t)
	h.srv.reaper.running.Lock()
	if _, ran := h.srv.reaper.pass(context.Background()); ran {
		t.Fatal("a second pass ran concurrently")
	}
	h.srv.reaper.running.Unlock()
}

func TestNonCacheRequestsPassThroughToUpstream(t *testing.T) {
	type seen struct {
		method, uri, auth, ct string
		body                  []byte
	}
	got := make(chan seen, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.Method, r.RequestURI, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), b}
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("upstream body"))
	}))
	defer up.Close()
	h := newHarness(t, func(o *Options) { o.Upstream = up.URL })

	path := "/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact?a=1&b=%2F"
	r, _ := http.NewRequest(http.MethodPost, h.http.URL+path, strings.NewReader(`{"name":"x"}`))
	r.Header.Set("Authorization", "Bearer runtime-token")
	r.Header.Set("Content-Type", "application/json")
	// A client-chosen host must not redirect the request.
	r.Host = "attacker.example.test"
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot || string(body) != "upstream body" || resp.Header.Get("X-Upstream") != "yes" {
		t.Fatalf("response %d %q %v", resp.StatusCode, body, resp.Header)
	}
	s := <-got
	if s.method != http.MethodPost || s.uri != path || s.auth != "Bearer runtime-token" ||
		s.ct != "application/json" || string(s.body) != `{"name":"x"}` {
		t.Fatalf("upstream saw %+v", s)
	}
	// A GET outside /twirp passes too.
	resp, err = http.Get(h.http.URL + "/some/other/path")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
	<-got
}

func TestUpstreamDownIs502(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Upstream = "http://127.0.0.1:1" })
	resp, err := http.Post(h.http.URL+"/twirp/x/y", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("%d", resp.StatusCode)
	}
}

func TestNewServerValidatesOptions(t *testing.T) {
	st := newMemStore(time.Now)
	good := Options{PublicURL: "https://cache.example.test", Upstream: DefaultUpstreamResultsURL, SigningKeys: [][]byte{make([]byte, 32)}, RepositoryIDs: []int64{1}}
	if _, err := newServer(st, good); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*Options){
		"no key":          func(o *Options) { o.SigningKeys = nil },
		"short key":       func(o *Options) { o.SigningKeys = [][]byte{make([]byte, 31)} },
		"public path":     func(o *Options) { o.PublicURL = "https://cache.example.test/x" },
		"public scheme":   func(o *Options) { o.PublicURL = "ftp://cache.example.test" },
		"upstream":        func(o *Options) { o.Upstream = "not a url" },
		"no allowlist":    func(o *Options) { o.RepositoryIDs = nil },
		"entry over 90 %": func(o *Options) { o.MaxBytes = 100; o.MaxEntryBytes = 91 },
	} {
		o := good
		edit(&o)
		if _, err := newServer(st, o); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
