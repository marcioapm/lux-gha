package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// mib is minAccountedBytes: sizes in these tests are multiples of it, so
// accounting and real sizes agree.
const mib = int64(minAccountedBytes)

func selectEntries(entries []object, now time.Time, ttl time.Duration, maxBytes int64) []object {
	del, _ := selectReap(entries, nil, true, "staging/", now, ttl, maxBytes)
	return del
}

func TestSelectEntries(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	entries := []object{
		obj("fresh", 10*mib, time.Hour, now),
		obj("expired", 10*mib, 8*day, now),
		obj("older", 30*mib, 3*day, now),
		obj("old", 30*mib, 2*day, now),
		obj("new", 30*mib, day, now),
	}
	if got := keysOf(selectEntries(entries, now, 7*day, 0)); got != "expired" {
		t.Fatalf("ttl only: %q", got)
	}
	// 100 MiB kept against a cap of 100: under it, nothing more.
	if got := keysOf(selectEntries(entries, now, 7*day, 100*mib)); got != "expired" {
		t.Fatalf("at cap: %q", got)
	}
	// Cap 90 → target 81: delete oldest-first until ≤ 81 (100 → 70).
	if got := keysOf(selectEntries(entries, now, 7*day, 90*mib)); got != "expired older" {
		t.Fatalf("over cap: %q", got)
	}
	// Cap 50 → target 45: 100 → 70 → 40.
	if got := keysOf(selectEntries(entries, now, 7*day, 50*mib)); got != "expired older old" {
		t.Fatalf("well over cap: %q", got)
	}
	if got := selectEntries(nil, now, day, 1); len(got) != 0 {
		t.Fatalf("empty: %v", got)
	}
	// An entry of MaxEntryBytesFor(cap), the largest config admits, survives
	// the pass right after it is saved.
	big := MaxEntryBytesFor(100 * mib)
	if got := keysOf(selectEntries([]object{obj("a", 11*mib, time.Hour, now), obj("just-saved", big, 0, now)}, now, 7*day, 100*mib)); got != "a" {
		t.Fatalf("largest entry: %q", got)
	}
	// The cap trims to 90 %, not 100 %: 105 over a cap of 100 loses two
	// 10 MiB entries (to 85), not one (to 95).
	var tens []object
	for i := range 10 {
		tens = append(tens, obj(fmt.Sprintf("e%d", i), 10*mib, time.Duration(20-i)*time.Hour, now))
	}
	tens = append(tens, obj("small", 5*mib, 0, now))
	if got := keysOf(selectEntries(tens, now, 7*day, 100*mib)); got != "e0 e1" {
		t.Fatalf("hysteresis: %q", got)
	}
}

// Every entry counts for at least 1 MiB, so many tiny entries are capped
// by count.
func TestTinyEntriesCountAgainstTheCap(t *testing.T) {
	now := time.Now()
	var tiny []object
	for i := range 10 {
		tiny = append(tiny, obj(fmt.Sprintf("t%d", i), 1, time.Duration(10-i)*time.Minute, now))
	}
	// 10 accounted MiB against a 5 MiB cap: trim to 4.5, keeping 4.
	if got := keysOf(selectEntries(tiny, now, 24*time.Hour, 5*mib)); got != "t0 t1 t2 t3 t4 t5" {
		t.Fatalf("%q", got)
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
	_, del := selectReap(nil, objs, true, root, now, 7*24*time.Hour, 0)
	if got := strings.Join(del, " "); got != root+"abandoned/block/01 "+root+"abandoned/manifest" {
		t.Fatalf("%q", got)
	}
}

// Staging counts against the cap and goes first, oldest reservation first,
// but never one written within the upload URL lifetime.
func TestStagingCountsAgainstTheCap(t *testing.T) {
	now := time.Now()
	root := "staging/"
	entries := []object{obj("v1/entry", 30*mib, 72*time.Hour, now)}
	staged := []object{
		obj(root+"idle/block/01", 25*mib, 10*time.Hour, now),
		obj(root+"idle/block/02", 25*mib, 7*time.Hour, now),
		// Its first block is old, its newest within uploadURLTTL: in flight.
		obj(root+"live/block/01", 25*mib, 20*time.Hour, now),
		obj(root+"live/block/02", 25*mib, uploadURLTTL-time.Minute, now),
	}
	// 130 MiB over a cap of 100 → target 90: the idle reservation goes
	// whole (130 → 80) and the older entry stays.
	de, ds := selectReap(entries, staged, true, root, now, 7*24*time.Hour, 100*mib)
	if keysOf(de) != "" || strings.Join(ds, " ") != root+"idle/block/01 "+root+"idle/block/02" {
		t.Fatalf("cap 100: entries %q staging %q", keysOf(de), ds)
	}
	// Cap 60 → target 54: the idle reservation, then the entry; the live
	// reservation is never a candidate.
	de, ds = selectReap(entries, staged, true, root, now, 7*24*time.Hour, 60*mib)
	if keysOf(de) != "v1/entry" || strings.Join(ds, " ") != root+"idle/block/01 "+root+"idle/block/02" {
		t.Fatalf("cap 60: entries %q staging %q", keysOf(de), ds)
	}
	// A failed entry listing: no entry is deleted and the cap is not
	// applied; abandoned staging still goes.
	staged = append(staged, obj(root+"abandoned/blob", 1, 30*time.Hour, now))
	de, ds = selectReap(nil, staged, false, root, now, 7*24*time.Hour, 1)
	if len(de) != 0 || strings.Join(ds, " ") != root+"abandoned/blob" {
		t.Fatalf("failed listing: entries %q staging %q", keysOf(de), ds)
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
	// Every object counts as 1 MiB. MaxBytes 2.5 MiB: after the expired
	// entry and the abandoned reservation, 3 entries and 1 live
	// reservation (4 MiB) remain; the oldest entries go until at most
	// 2.25 MiB remain, which takes "oldest" and "stuck". The live
	// reservation, written within the upload URL lifetime, stays.
	h := newHarness(t, func(o *Options) { o.MaxBytes = 5 * mib / 2; o.Log = slog.New(logs) })
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
		// Another application's upload in a shared bucket.
		{Key: "other-app/x", UploadID: "foreign", Initiated: now.Add(-48 * time.Hour)},
	}
	// "stuck" fails to delete this pass.
	h.st.failDelete = map[string]bool{l.entryKey("2", "s", v1, "stuck"): true}

	res, ran := h.srv.reaper.pass(context.Background())
	if !ran {
		t.Fatal("pass did not run")
	}
	want := reapResult{Entries: 4, Bytes: 31, StagingBytes: 2, DeletedEntries: 2, DeletedBytes: 11, DeletedStaging: 1, AbortedUploads: 1, Errors: 1}
	if res != want {
		t.Fatalf("result %+v, want %+v", res, want)
	}
	left := strings.Join(h.st.keys(""), " ")
	for _, k := range []string{keep, l.entryKey("2", "s", v1, "stuck"), l.stagingRoot() + "new/blob", "elsewhere/v1/1/x"} {
		if !strings.Contains(left, k) {
			t.Errorf("%s was deleted", k)
		}
	}
	if len(h.st.uploads) != 2 || h.st.uploads[0].UploadID != "live" || h.st.uploads[1].UploadID != "foreign" {
		t.Errorf("uploads %+v", h.st.uploads)
	}
	if !contains(logs.snapshot(), "cache reaper pass") {
		t.Errorf("no summary line: %q", logs.snapshot())
	}
	// The failed delete is retried next pass.
	h.st.failDelete = nil
	res, _ = h.srv.reaper.pass(context.Background())
	if res.DeletedEntries != 1 || res.Errors != 0 {
		t.Fatalf("second pass %+v", res)
	}
}

// A listing that fails part-way deletes no entry: the oldest of a partial
// listing is not the oldest overall.
func TestReaperDeletesNoEntryFromAPartialListing(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxBytes = 2 * mib })
	now := h.clk.now()
	l := h.srv.lay
	var all []string
	for i := range 6 {
		k := l.entryKey("1", "s", v1, fmt.Sprintf("e%d", i))
		age := 8 * 24 * time.Hour // expired too
		if i%2 == 1 {
			age = time.Hour
		}
		h.st.setAt(k, []byte("x"), now.Add(-age))
		all = append(all, k)
	}
	h.st.failList = map[string]error{l.entriesRoot(): errors.New("page 2: InternalError")}
	res, _ := h.srv.reaper.pass(context.Background())
	if res.DeletedEntries != 0 || res.Errors != 1 {
		t.Fatalf("%+v", res)
	}
	h.st.failList = nil
	if got := h.st.keys(l.entriesRoot()); len(got) != len(all) {
		t.Fatalf("entries left %q", got)
	}
}

// More than 1000 staged objects are deleted in batches S3 accepts.
func TestStagingCleanupIsBatched(t *testing.T) {
	h := newHarness(t)
	tok := h.is.token(t, "100", writeMain())
	h.reserve(tok, "k", v1)
	id, err := h.srv.auth.verify(context.Background(), "Bearer "+tok)
	if err != nil {
		t.Fatal(err)
	}
	staging := h.srv.lay.stagingKey(id.RepoID, id.WriteScope, v1, "k", id.TokenHash)
	for i := range 2500 {
		h.st.set(fmt.Sprintf("%s/block/%04x", staging, i), []byte("x"))
	}
	h.srv.cleanStaging(context.Background(), staging)
	if left := h.st.keys(staging + "/"); len(left) != 0 || h.st.deleteCalls != 3 {
		t.Fatalf("%d left after %d calls", len(left), h.st.deleteCalls)
	}
}

// Each pass runs under a deadline of one reap interval.
func TestReaperPassHasDeadline(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.ReapInterval = time.Minute })
	st := &deadlineStore{memStore: h.st}
	h.srv.reaper.st = st
	h.srv.reaper.pass(context.Background())
	if st.deadline.IsZero() || time.Until(st.deadline) > time.Minute {
		t.Fatalf("listing ran with deadline %v", st.deadline)
	}
}

type deadlineStore struct {
	*memStore
	deadline time.Time
}

func (d *deadlineStore) list(ctx context.Context, prefix string, fn func(object) bool) error {
	d.deadline, _ = ctx.Deadline()
	return d.memStore.list(ctx, prefix, fn)
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
