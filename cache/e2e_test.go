//go:build s3integration

package cache

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// End-to-end tests drive the real @actions/cache client (cache/testdata/e2e.mjs)
// against the server on a real S3-compatible store. Besides the S3
// variables they need node on PATH and LUXGHA_CACHE_NODE_DIR, a directory
// whose node_modules holds @actions/cache 6.

type e2e struct {
	*harness
	nodeDir   string
	workspace string
	blocks    atomic.Int64
	blobs     atomic.Int64
}

func newE2E(t *testing.T) *e2e {
	nodeDir := os.Getenv("LUXGHA_CACHE_NODE_DIR")
	if nodeDir == "" {
		t.Fatal("LUXGHA_CACHE_NODE_DIR is required")
	}
	st, prefix := s3TestStore(t)
	h := newHarness(t)
	srv, err := newServer(st, Options{
		PublicURL: h.http.URL, Issuer: h.is.URL, Upstream: "http://upstream.invalid",
		SigningKeys: [][]byte{bytes.Repeat([]byte("k"), 32)}, Prefix: prefix, RepositoryIDs: []int64{100, 200},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	e := &e2e{harness: h, nodeDir: nodeDir, workspace: realTempDir(t)}
	// Count the Azure requests to tell the single-shot and block paths apart.
	h.http.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == blobPath && r.Method == http.MethodPut {
			switch r.URL.Query().Get("comp") {
			case "block":
				e.blocks.Add(1)
			case "":
				e.blobs.Add(1)
			}
		}
		srv.ServeHTTP(w, r)
	})
	return e
}

// realTempDir is t.TempDir with symlinks resolved: the client's tar makes
// paths relative to GITHUB_WORKSPACE and fails if the two spellings differ.
func realTempDir(t *testing.T) string {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// node runs one client operation with token and returns its result line.
func (e *e2e) node(token string, op map[string]any) map[string]any {
	e.t.Helper()
	arg, _ := json.Marshal(op)
	script, err := filepath.Abs(filepath.Join("testdata", "e2e.mjs"))
	if err != nil {
		e.t.Fatal(err)
	}
	cmd := exec.Command("node", script, string(arg))
	// The client resolves paths against the working directory.
	cmd.Dir = e.workspace
	cmd.Env = append(os.Environ(),
		"LUXGHA_CACHE_NODE_DIR="+e.nodeDir,
		"ACTIONS_RESULTS_URL="+e.http.URL+"/",
		"ACTIONS_RUNTIME_TOKEN="+token,
		"ACTIONS_CACHE_SERVICE_V2=true",
		"GITHUB_WORKSPACE="+e.workspace,
		"RUNNER_TEMP="+realTempDir(e.t),
	)
	out, err := cmd.CombinedOutput()
	e.t.Logf("node %s:\n%s", arg, out)
	if err != nil {
		e.t.Fatalf("node: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	last := lines[len(lines)-1]
	res := map[string]any{}
	if !strings.HasPrefix(last, "RESULT ") || json.Unmarshal([]byte(strings.TrimPrefix(last, "RESULT ")), &res) != nil {
		e.t.Fatalf("no result line in node output")
	}
	for _, l := range lines {
		// The client reports server failures as warnings or errors, not exits.
		if strings.HasPrefix(l, "::error::") || strings.HasPrefix(l, "::warning::") {
			res["warnings"] = str(res["warnings"]) + l + "\n"
		}
	}
	return res
}

func (e *e2e) write(rel string, data []byte) {
	e.t.Helper()
	p := filepath.Join(e.workspace, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *e2e) read(rel string) []byte {
	b, _ := os.ReadFile(filepath.Join(e.workspace, rel))
	return b
}

func (e *e2e) wipe(rel string) {
	if err := os.RemoveAll(filepath.Join(e.workspace, rel)); err != nil {
		e.t.Fatal(err)
	}
}

func saved(res map[string]any) bool {
	id, _ := res["cacheId"].(float64)
	return id > 0
}

func TestE2ESaveRestoreWithActionsCache(t *testing.T) {
	e := newE2E(t)
	tok := e.is.token(t, "100", writeMain())
	paths := []any{"deps"}

	e.write("deps/a.txt", []byte("first"))
	if res := e.node(tok, map[string]any{"op": "save", "paths": paths, "key": "deps-linux-old"}); !saved(res) || res["warnings"] != nil {
		t.Fatalf("save old: %v", res)
	}
	// LastModified has one-second resolution on many S3 servers: wait until
	// a new save is stamped later than this one.
	var first object
	e.srv.st.list(t.Context(), e.srv.lay.entriesRoot(), func(o object) bool { first = o; return false })
	for !time.Now().Truncate(time.Second).After(first.LastModified) {
		time.Sleep(50 * time.Millisecond)
	}
	e.write("deps/a.txt", []byte("second"))
	if res := e.node(tok, map[string]any{"op": "save", "paths": paths, "key": "deps-linux-new"}); !saved(res) {
		t.Fatalf("save new: %v", res)
	}
	if e.blobs.Load() != 2 || e.blocks.Load() != 0 {
		t.Fatalf("small saves used %d single-shot and %d block requests", e.blobs.Load(), e.blocks.Load())
	}

	// Exact restore.
	e.wipe("deps")
	if res := e.node(tok, map[string]any{"op": "restore", "paths": paths, "key": "deps-linux-old"}); res["matchedKey"] != "deps-linux-old" {
		t.Fatalf("exact restore: %v", res)
	}
	if got := string(e.read("deps/a.txt")); got != "first" {
		t.Fatalf("exact restore content %q", got)
	}
	// Restore-key fallback picks the newest match.
	e.wipe("deps")
	if res := e.node(tok, map[string]any{"op": "restore", "paths": paths, "key": "deps-linux-missing", "restoreKeys": []string{"deps-linux-"}}); res["matchedKey"] != "deps-linux-new" {
		t.Fatalf("restore keys: %v", res)
	}
	if got := string(e.read("deps/a.txt")); got != "second" {
		t.Fatalf("restore-key content %q", got)
	}
	// A miss.
	if res := e.node(tok, map[string]any{"op": "restore", "paths": paths, "key": "nothing-here", "restoreKeys": []string{"other-"}}); res["matchedKey"] != nil || res["warnings"] != nil {
		t.Fatalf("miss: %v", res)
	}
	// Other paths hash to another version: a miss for the same key.
	if res := e.node(tok, map[string]any{"op": "restore", "paths": []any{"deps", "more"}, "key": "deps-linux-old"}); res["matchedKey"] != nil {
		t.Fatalf("version mismatch: %v", res)
	}
	// Saving an existing key and version is skipped.
	if res := e.node(tok, map[string]any{"op": "save", "paths": paths, "key": "deps-linux-old"}); saved(res) {
		t.Fatalf("resave: %v", res)
	}
	// Another repository sees nothing.
	if res := e.node(e.is.token(t, "200", writeMain()), map[string]any{"op": "restore", "paths": paths, "key": "deps-linux-old", "restoreKeys": []string{"deps-"}}); res["matchedKey"] != nil {
		t.Fatalf("cross-repo: %v", res)
	}
	// A read-only token restores but cannot save.
	ro := e.is.token(t, "100", readMain())
	if res := e.node(ro, map[string]any{"op": "save", "paths": paths, "key": "ro-key"}); saved(res) || !strings.Contains(str(res["warnings"]), "cache write denied") {
		t.Fatalf("read-only save: %v", res)
	}
	if res := e.node(ro, map[string]any{"op": "restore", "paths": paths, "key": "deps-linux-old"}); res["matchedKey"] != "deps-linux-old" {
		t.Fatalf("read-only restore: %v", res)
	}
}

func TestE2ELargeArchiveUsesBlocks(t *testing.T) {
	e := newE2E(t)
	tok := e.is.token(t, "100", writeMain())
	// Random bytes do not compress: the archive exceeds the client's 128 MiB
	// single-shot limit and is sent as 64 MiB blocks.
	data := make([]byte, 150<<20)
	rand.Read(data)
	want := sha256.Sum256(data)
	e.write("big/blob.bin", data)
	data = nil
	if res := e.node(tok, map[string]any{"op": "save", "paths": []any{"big"}, "key": "big-1"}); !saved(res) || res["warnings"] != nil {
		t.Fatalf("save: %v", res)
	}
	if e.blocks.Load() < 3 || e.blobs.Load() != 0 {
		t.Fatalf("large save used %d single-shot and %d block requests", e.blobs.Load(), e.blocks.Load())
	}
	e.wipe("big")
	if res := e.node(tok, map[string]any{"op": "restore", "paths": []any{"big"}, "key": "big-1"}); res["matchedKey"] != "big-1" {
		t.Fatalf("restore: %v", res)
	}
	if got := sha256.Sum256(e.read("big/blob.bin")); got != want {
		t.Fatal("restored content differs")
	}
}

func TestE2EConcurrentSavesOfOneKey(t *testing.T) {
	e := newE2E(t)
	const jobs = 4
	// Each job saves from its own workspace with its own runtime token.
	workspaces := make([]string, jobs)
	for i := range jobs {
		workspaces[i] = realTempDir(t)
		p := filepath.Join(workspaces[i], "deps", "who.txt")
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, bytes.Repeat([]byte(fmt.Sprint(i)), 1<<20), 0o644)
	}
	results := make([]map[string]any, jobs)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := e.is.claims(t, "100", writeMain())
			c["jti"] = fmt.Sprint(i)
			je := &e2e{harness: e.harness, nodeDir: e.nodeDir, workspace: workspaces[i]}
			results[i] = je.node(e.is.sign(t, c), map[string]any{"op": "save", "paths": []any{"deps"}, "key": "same-key"})
		}()
	}
	wg.Wait()
	won := 0
	for _, r := range results {
		if saved(r) {
			won++
		}
	}
	// At least one save wins on any server; exactly one only where
	// concurrent conditional completes are serialised (see
	// concurrentConditional).
	if won < 1 || (concurrentConditional() && won != 1) {
		t.Fatalf("%d of %d concurrent saves succeeded: %v", won, jobs, results)
	}
	t.Logf("%d of %d concurrent saves succeeded", won, jobs)
	e.wipe("deps")
	if res := e.node(e.is.token(t, "100", readMain()), map[string]any{"op": "restore", "paths": []any{"deps"}, "key": "same-key"}); res["matchedKey"] != "same-key" {
		t.Fatalf("restore: %v", res)
	}
	got := e.read("deps/who.txt")
	if len(got) != 1<<20 || !bytes.Equal(got, bytes.Repeat(got[:1], 1<<20)) {
		t.Fatal("restored a mixed or partial archive")
	}
}
