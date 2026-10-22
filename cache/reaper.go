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
	// timeout bounds one pass, so a stuck S3 call cannot stop later passes.
	timeout time.Duration
	now     func() time.Time
	log     *slog.Logger
	running sync.Mutex
}

// reapResult is one pass's summary.
type reapResult struct {
	Entries        int
	Bytes          int64
	StagingBytes   int64
	DeletedEntries int
	DeletedBytes   int64
	DeletedStaging int
	AbortedUploads int
	Errors         int
}

// minAccountedBytes is the least size an entry or a staging reservation
// counts for against maxBytes, so the cap also bounds how many objects the
// reaper lists and holds in memory (maxBytes / 1 MiB).
const minAccountedBytes = 1 << 20

func accounted(size int64) int64 { return max(size, minAccountedBytes) }

// reservation is one upload's staging objects.
type reservation struct {
	name   string
	newest time.Time
	bytes  int64
	keys   []string
}

// groupStaging groups staging objects by reservation, oldest newest-object
// first.
func groupStaging(objs []object, root string) []reservation {
	idx := map[string]int{}
	var res []reservation
	for _, o := range objs {
		name := reservationOf(root, o.Key)
		i, ok := idx[name]
		if !ok {
			i = len(res)
			idx[name] = i
			res = append(res, reservation{name: name, newest: o.LastModified})
		}
		r := &res[i]
		if o.LastModified.After(r.newest) {
			r.newest = o.LastModified
		}
		r.bytes += o.Size
		r.keys = append(r.keys, o.Key)
	}
	slices.SortFunc(res, func(a, b reservation) int {
		if c := a.newest.Compare(b.newest); c != 0 {
			return c
		}
		return strings.Compare(a.name, b.name)
	})
	return res
}

// selectReap returns the entries and staging objects to delete:
//   - every entry last modified before now-ttl;
//   - every staging reservation whose newest object is older than
//     stagingMaxAge, whole, so a pass never removes part of a live upload;
//   - then, while entries and staging together exceed maxBytes (each
//     counted at least minAccountedBytes), staging reservations oldest
//     first, never one written to within uploadURLTTL, and then the oldest
//     entries, until 90 % of maxBytes remain. maxBytes 0 is no cap.
//
// entriesOK false means the entry listing failed: no entry is deleted and
// the cap is not applied, since the total is unknown.
func selectReap(entries, staged []object, entriesOK bool, root string, now time.Time, ttl time.Duration, maxBytes int64) (delEntries []object, delStaging []string) {
	groups := groupStaging(staged, root)
	g := 0
	for ; g < len(groups) && groups[g].newest.Before(now.Add(-stagingMaxAge)); g++ {
		delStaging = append(delStaging, groups[g].keys...)
	}
	if !entriesOK {
		return nil, delStaging
	}
	sorted := slices.Clone(entries)
	slices.SortStableFunc(sorted, func(a, b object) int {
		if c := a.LastModified.Compare(b.LastModified); c != 0 {
			return c
		}
		return strings.Compare(a.Key, b.Key)
	})
	i := 0
	for ; i < len(sorted) && sorted[i].LastModified.Before(now.Add(-ttl)); i++ {
		delEntries = append(delEntries, sorted[i])
	}
	if maxBytes <= 0 {
		return delEntries, delStaging
	}
	var total int64
	for _, o := range sorted[i:] {
		total += accounted(o.Size)
	}
	for _, r := range groups[g:] {
		total += accounted(r.bytes)
	}
	if total <= maxBytes {
		return delEntries, delStaging
	}
	target := maxBytes - maxBytes/10
	// Groups are ordered by their newest object, so the first one written
	// within uploadURLTTL ends the staging candidates.
	for ; g < len(groups) && total > target && groups[g].newest.Before(now.Add(-uploadURLTTL)); g++ {
		delStaging = append(delStaging, groups[g].keys...)
		total -= accounted(groups[g].bytes)
	}
	for ; i < len(sorted) && total > target; i++ {
		delEntries = append(delEntries, sorted[i])
		total -= accounted(sorted[i].Size)
	}
	return delEntries, delStaging
}

func reservationOf(root, key string) string {
	rest := strings.TrimPrefix(key, root)
	res, _, _ := strings.Cut(rest, "/")
	return res
}

const deleteBatch = 1000

// deleteBatched deletes keys in batches of deleteBatch, the S3
// DeleteObjects limit, and returns the ones not deleted.
func deleteBatched(ctx context.Context, st store, log *slog.Logger, keys []string) (failed []string) {
	for len(keys) > 0 {
		n := min(len(keys), deleteBatch)
		f, err := st.deleteKeys(ctx, keys[:n])
		if err != nil {
			log.Warn("cache: delete failed; the reaper retries", "error", err)
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
	if rp.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, rp.timeout)
		defer cancel()
	}
	var res reapResult
	now := rp.now()

	var entries []object
	err := rp.st.list(ctx, rp.lay.entriesRoot(), func(o object) bool {
		entries = append(entries, o)
		return true
	})
	entriesOK := err == nil
	if !entriesOK {
		// A partial listing would undercount the total; delete no entry.
		rp.log.Warn("cache reaper: listing entries failed", "error", err)
		res.Errors++
		entries = nil
	}
	for _, o := range entries {
		res.Entries++
		res.Bytes += o.Size
	}

	var staged []object
	root := rp.lay.stagingRoot()
	if err := rp.st.list(ctx, root, func(o object) bool {
		staged = append(staged, o)
		return true
	}); err != nil {
		// Without the staging listing the cap counts entries only, which
		// can only delete fewer.
		rp.log.Warn("cache reaper: listing staging failed", "error", err)
		res.Errors++
		staged = nil
	}
	for _, o := range staged {
		res.StagingBytes += o.Size
	}

	delEntries, delStaging := selectReap(entries, staged, entriesOK, root, now, rp.ttl, rp.maxBytes)
	if len(delStaging) > 0 {
		failed := deleteBatched(ctx, rp.st, rp.log, delStaging)
		res.DeletedStaging, res.Errors = len(delStaging)-len(failed), res.Errors+len(failed)
	}
	if len(delEntries) > 0 {
		keys := make([]string, len(delEntries))
		for i, o := range delEntries {
			keys[i] = o.Key
		}
		failed := map[string]bool{}
		for _, k := range deleteBatched(ctx, rp.st, rp.log, keys) {
			failed[k] = true
		}
		res.Errors += len(failed)
		for _, o := range delEntries {
			if !failed[o.Key] {
				res.DeletedEntries++
				res.DeletedBytes += o.Size
			}
		}
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
		"entries", res.Entries, "bytes", res.Bytes, "staging_bytes", res.StagingBytes,
		"deleted_entries", res.DeletedEntries, "deleted_bytes", res.DeletedBytes,
		"deleted_staging", res.DeletedStaging, "aborted_uploads", res.AbortedUploads,
		"errors", res.Errors)
	return res, true
}
