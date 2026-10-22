package cache

import (
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

func count(lines []string, prefix string) int {
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}
