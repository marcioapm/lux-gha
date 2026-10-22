package cache

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// reaper deletes expired entries, enforces the total size cap, and removes
// abandoned staging data and multipart uploads.
type reaper struct {
	st       store
	lay      layout
	ttl      time.Duration
	maxBytes int64
	now      func() time.Time
	log      *slog.Logger
	running  sync.Mutex
}

// reapResult is one pass's summary.
type reapResult struct {
	Entries        int
	Bytes          int64
	DeletedEntries int
	DeletedBytes   int64
	DeletedStaging int
	AbortedUploads int
	Errors         int
}

// selectEntries returns the entries to delete: every one last modified
// before now-ttl, then, while the rest exceed maxBytes, the oldest of the
// rest until they fit in 90 % of maxBytes. maxBytes 0 is no cap.
func selectEntries(entries []object, now time.Time, ttl time.Duration, maxBytes int64) []object {
	sorted := slices.Clone(entries)
	slices.SortStableFunc(sorted, func(a, b object) int {
		if c := a.LastModified.Compare(b.LastModified); c != 0 {
			return c
		}
		return strings.Compare(a.Key, b.Key)
	})
	cutoff := now.Add(-ttl)
	var del []object
	var kept int64
	i := 0
	for ; i < len(sorted) && sorted[i].LastModified.Before(cutoff); i++ {
		del = append(del, sorted[i])
	}
	for _, o := range sorted[i:] {
		kept += o.Size
	}
	if maxBytes <= 0 || kept <= maxBytes {
		return del
	}
	target := maxBytes / 10 * 9
	for ; i < len(sorted) && kept > target; i++ {
		del = append(del, sorted[i])
		kept -= sorted[i].Size
	}
	return del
}

// selectStaging returns the staging objects of reservations whose newest
// object is older than maxAge. A reservation is deleted whole or not at
// all, so a pass never removes part of an upload still being written.
func selectStaging(objs []object, root string, now time.Time, maxAge time.Duration) []string {
	newest := map[string]time.Time{}
	for _, o := range objs {
		res := reservationOf(root, o.Key)
		if t, ok := newest[res]; !ok || o.LastModified.After(t) {
			newest[res] = o.LastModified
		}
	}
	cutoff := now.Add(-maxAge)
	var del []string
	for _, o := range objs {
		if newest[reservationOf(root, o.Key)].Before(cutoff) {
			del = append(del, o.Key)
		}
	}
	return del
}

func reservationOf(root, key string) string {
	rest := strings.TrimPrefix(key, root)
	res, _, _ := strings.Cut(rest, "/")
	return res
}

const deleteBatch = 1000

// deleteAll deletes keys in batches and returns the ones not deleted.
func (rp *reaper) deleteAll(ctx context.Context, keys []string) (failed []string) {
	for len(keys) > 0 {
		n := min(len(keys), deleteBatch)
		f, err := rp.st.deleteKeys(ctx, keys[:n])
		if err != nil {
			rp.log.Warn("cache reaper: delete failed; retried next pass", "error", err)
			f = keys[:n]
		}
		failed = append(failed, f...)
		keys = keys[n:]
	}
	return failed
}

// pass runs one reap. Returns false when another pass is running.
func (rp *reaper) pass(ctx context.Context) (reapResult, bool) {
	if !rp.running.TryLock() {
		return reapResult{}, false
	}
	defer rp.running.Unlock()
	var res reapResult
	now := rp.now()

	var entries []object
	err := rp.st.list(ctx, rp.lay.entriesRoot(), func(o object) bool {
		entries = append(entries, o)
		return true
	})
	if err != nil {
		// A partial listing would undercount the total; delete nothing.
		rp.log.Warn("cache reaper: listing entries failed", "error", err)
		res.Errors++
	} else {
		for _, o := range entries {
			res.Entries++
			res.Bytes += o.Size
		}
		del := selectEntries(entries, now, rp.ttl, rp.maxBytes)
		keys := make([]string, len(del))
		for i, o := range del {
			keys[i] = o.Key
		}
		failed := rp.deleteAll(ctx, keys)
		res.Errors += len(failed)
		for _, o := range del {
			if !slices.Contains(failed, o.Key) {
				res.DeletedEntries++
				res.DeletedBytes += o.Size
			}
		}
	}

	var staged []object
	root := rp.lay.stagingRoot()
	if err := rp.st.list(ctx, root, func(o object) bool {
		staged = append(staged, o)
		return true
	}); err != nil {
		rp.log.Warn("cache reaper: listing staging failed", "error", err)
		res.Errors++
	} else {
		keys := selectStaging(staged, root, now, stagingMaxAge)
		failed := rp.deleteAll(ctx, keys)
		res.DeletedStaging, res.Errors = len(keys)-len(failed), res.Errors+len(failed)
	}

	var stale []pendingUpload
	if err := rp.st.listUploads(ctx, rp.lay.prefix, func(u pendingUpload) bool {
		if u.Initiated.Before(now.Add(-stagingMaxAge)) {
			stale = append(stale, u)
		}
		return true
	}); err != nil {
		rp.log.Warn("cache reaper: listing multipart uploads failed", "error", err)
		res.Errors++
	}
	for _, u := range stale {
		if err := rp.st.abortUpload(ctx, u); err != nil {
			res.Errors++
			continue
		}
		res.AbortedUploads++
	}
	rp.log.Info("cache reaper pass",
		"entries", res.Entries, "bytes", res.Bytes,
		"deleted_entries", res.DeletedEntries, "deleted_bytes", res.DeletedBytes,
		"deleted_staging", res.DeletedStaging, "aborted_uploads", res.AbortedUploads,
		"errors", res.Errors)
	return res, true
}
