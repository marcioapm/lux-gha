package cache

import (
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"
	"time"
)

// stagingOf returns the staging key a token's reservation of key uses.
func (h *harness) stagingOf(token, key, version string) string {
	h.t.Helper()
	id, err := h.srv.auth.verify(h.t.Context(), "Bearer "+token)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.srv.lay.stagingKey(id.RepoID, id.WriteScope, version, key, id.TokenHash)
}

// A Finalize retried after its CompleteMultipartUpload succeeded but the
// reply was lost reports the save, not "another job saved first".
func TestFinalizeRetryAfterLostSuccess(t *testing.T) {
	logs := &logRecorder{}
	h := newHarness(t, func(o *Options) { o.Log = slog.New(logs) })
	tok := h.is.token(t, "100", writeMain())
	h.putBlobOK(h.reserve(tok, "k", v1), []byte("mine"))
	h.st.loseAssembleReply = true
	if f := h.finalize(tok, "k", v1, 4); f.status != http.StatusInternalServerError || f.code != twirpInternal {
		t.Fatalf("lost reply: %+v", f)
	}
	f := h.finalize(tok, "k", v1, 4)
	if !f.ok() || str(f.body["entry_id"]) == "" {
		t.Fatalf("retry: %+v", f)
	}
	if !contains(logs.snapshot(), "cache: saved") {
		t.Fatalf("the retried save was not logged: %q", logs.snapshot())
	}
	if left := h.st.keys(h.stagingOf(tok, "k", v1) + "/"); len(left) != 0 {
		t.Fatalf("staging left %q", left)
	}
	// Another job's finalize of the same key still loses.
	c := h.is.claims(t, "100", writeMain())
	c["jti"] = "other-job"
	other := h.is.sign(t, c)
	h.putBlobOK(h.reserve2(other, "k", v1), []byte("theirs"))
	if f := h.finalize(other, "k", v1, 6); f.ok() || !strings.Contains(str(f.body["message"]), "another job saved") {
		t.Fatalf("other job: %+v", f)
	}
}

// reserve2 mints an upload URL even when the entry exists, as a Create
// that raced ahead of the first job's Finalize would have.
func (h *harness) reserve2(token, key, version string) string {
	h.t.Helper()
	staging := h.stagingOf(token, key, version)
	return h.http.URL + blobPath + "?sig=" + h.srv.sign.mint(staging, h.clk.now().Add(time.Hour))
}

func TestFinalizeErrorPaths(t *testing.T) {
	h := newHarness(t)
	tok := h.is.token(t, "100", writeMain())

	// Parts reaped between the block list and Finalize: ok:false, expired.
	up := h.reserve(tok, "gone", v1)
	h.put(up, "comp=block&blockid="+blockID(0), nil, []byte("abc"))
	if resp, b := h.put(up, "comp=blocklist", nil, blockListXML(blockID(0))); resp.StatusCode != http.StatusCreated {
		t.Fatalf("block list: %d %s", resp.StatusCode, b)
	}
	staging := h.stagingOf(tok, "gone", v1)
	failed, _ := h.st.deleteKeys(t.Context(), h.st.keys(staging+"/block/"))
	if len(failed) != 0 {
		t.Fatal(failed)
	}
	if f := h.finalize(tok, "gone", v1, 3); f.status != http.StatusOK || f.ok() || !strings.Contains(str(f.body["message"]), "expired") {
		t.Fatalf("parts gone: %+v", f)
	}
	if r := h.restore(tok, "gone", v1); r.body["ok"] != nil {
		t.Fatalf("an entry exists: %+v", r)
	}

	// A corrupt or empty manifest is a server error.
	for _, m := range []string{`{}`, `not json`, `{"parts":[],"size":0}`} {
		h.reserve(tok, "corrupt", v1)
		h.st.set(h.stagingOf(tok, "corrupt", v1)+"/manifest", []byte(m))
		if f := h.finalize(tok, "corrupt", v1, 0); f.status != http.StatusInternalServerError || f.code != twirpInternal {
			t.Fatalf("manifest %s: %+v", m, f)
		}
	}
}

// A HEAD error is a 500 the client retries, not a miss or a fresh save.
func TestHeadErrorsAreServerErrors(t *testing.T) {
	h := newHarness(t)
	tok := h.is.token(t, "100", writeMain())
	h.save(tok, "k", v1, []byte("x"))
	id, _ := h.srv.auth.verify(t.Context(), "Bearer "+tok)
	final := h.srv.lay.entryKey(id.RepoID, id.WriteScope, v1, "k")
	h.st.failHead = map[string]error{final: errors.New("SlowDown")}
	if r := h.restore(tok, "k", v1); r.status != http.StatusInternalServerError || r.code != twirpInternal {
		t.Fatalf("restore: %+v", r)
	}
	if r := h.create(tok, "k", v1); r.status != http.StatusInternalServerError || r.code != twirpInternal {
		t.Fatalf("create: %+v", r)
	}
}

// An exact match wins over a newer entry it is a prefix of.
func TestExactMatchBeatsNewerPrefixMatch(t *testing.T) {
	h := newHarness(t)
	tok := h.is.token(t, "100", writeMain())
	h.save(tok, "lib-1", v1, []byte("exact"))
	h.clk.add(time.Minute)
	h.save(tok, "lib-10", v1, []byte("newer"))
	r := h.restore(tok, "lib-1", v1)
	if r.body["matched_key"] != "lib-1" || string(h.restored(r)) != "exact" {
		t.Fatalf("%+v", r)
	}
}

// Entries with the same LastModified: the greater key wins, whatever the
// listing order.
func TestNewestTieBreak(t *testing.T) {
	h := newHarness(t)
	tok := h.is.token(t, "100", writeMain())
	at := h.clk.now().Add(-time.Hour)
	for _, k := range []string{"dep-b", "dep-a", "dep-c"} {
		h.st.setAt(h.srv.lay.entryKey("100", mainScope, v1, k), []byte(k), at)
	}
	if r := h.restore(tok, "x", v1, "dep-"); r.body["matched_key"] != "dep-c" {
		t.Fatalf("%+v", r)
	}
}

func TestRestoreKeyValidation(t *testing.T) {
	h := newHarness(t)
	tok := h.is.token(t, "100", writeMain())
	for _, rk := range [][]string{{"a,b"}, {""}, {strings.Repeat("k", 513)}} {
		req := map[string]any{"key": "k", "version": v1, "restore_keys": rk}
		if r := h.call(tok, "GetCacheEntryDownloadURL", req); r.code != twirpInvalidArgument {
			t.Errorf("%q: %+v", rk, r)
		}
	}
}

// Without length prefixes, ("1", "2x") and ("12", "x") would hash alike.
func TestStagingKeyLengthPrefixes(t *testing.T) {
	l := newLayout("p")
	if l.stagingKey("1", "2x", v1, "k", "t") == l.stagingKey("12", "x", v1, "k", "t") {
		t.Fatal("repository and scope collide")
	}
	if l.stagingKey("1", "s", v1, "ab", "c") == l.stagingKey("1", "s", v1, "a", "bc") {
		t.Fatal("key and token hash collide")
	}
}

func TestUploadTokenVersionByte(t *testing.T) {
	s := signer{keys: [][]byte{[]byte("key-one-key-one-key-one-key-one!")}}
	now := time.Unix(1_700_000_000, 0)
	payload := append([]byte{uploadTokenVersion + 1}, make([]byte, 8)...)
	payload = append(payload, "staging/x"...)
	payload[8] = 0xff // far future
	tok := b64(payload) + "." + b64(s.mac(s.keys[0], payload))
	if _, err := s.open(tok, now); err != errBadToken {
		t.Fatalf("version %d accepted: %v", uploadTokenVersion+1, err)
	}
}

// entryID is positive and below 2^53, exact as a JavaScript number.
func TestEntryIDRange(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := range 10000 {
		k := fmt.Sprintf("v1/%d/%x", i, r.Uint64())
		if id := entryID(k); id < 1 || id >= 1<<53 {
			t.Fatalf("%s: %d", k, id)
		}
	}
}
