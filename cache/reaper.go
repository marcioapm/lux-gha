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

// minAccountedBytes is the least size any entry or staging object counts
// for against its budget, so the budgets also bound how many objects the
// reaper lists and holds in memory (about maxBytes / 1 MiB).
const minAccountedBytes = 1 << 20

func accounted(size int64) int64 { return max(size, minAccountedBytes) }

// reservation is one upload's staging objects: first and newest are the
// earliest and latest LastModified among them, bytes the sum of their
// accounted sizes.
type reservation struct {
	name          string
	first, newest time.Time
	bytes         int64
	keys          []string
}

// groupStaging groups staging objects by reservation, ordered by their
// first object, oldest first. Writing to a reservation again moves its
// newest object, not its first, so it cannot overtake later uploads.
func groupStaging(objs []object, root string) []reservation {
	idx := map[string]int{}
	var res []reservation
	for _, o := range objs {
		name := reservationOf(root, o.Key)
		i, ok := idx[name]
		if !ok {
			i = len(res)
			idx[name] = i
			res = append(res, reservation{name: name, first: o.LastModified, newest: o.LastModified})
		}
		r := &res[i]
		if o.LastModified.After(r.newest) {
			r.newest = o.LastModified
		}
		if o.LastModified.Before(r.first) {
			r.first = o.LastModified
		}
		r.bytes += accounted(o.Size)
		r.keys = append(r.keys, o.Key)
	}
	slices.SortFunc(res, func(a, b reservation) int {
		if c := a.first.Compare(b.first); c != 0 {
			return c
		}
		return strings.Compare(a.name, b.name)
	})
	return res
}

// selectReap returns the entries and staging objects to delete. maxBytes
// is split into two budgets (StagingBudgetFor, EntryBudgetFor); 0 is no
// cap. Every entry and staging object counts at least minAccountedBytes.
//
//   - Every staging reservation whose newest object is older than
//     stagingMaxAge goes, whole, so a pass never removes part of an upload.
//   - While the remaining staging exceeds the staging budget, whole
//     reservations go, oldest first object first, live ones included. The
//     newest reservation (latest first object) always stays: one upload is
//     bounded on its own by maxEntryBytes, so the newest upload completes
//     under any budget.
//   - Every entry last modified before now-ttl goes.
//   - While the remaining entries exceed the entry budget, the oldest go.
//     Staging never counts here, so an upload never pushes out an entry,
//     and since maxEntryBytes ≤ the entry budget, the newest entry stays.
//
// entriesOK false means the entry listing failed: no entry is deleted,
// since the total is unknown; staging is still trimmed.
func selectReap(entries, staged []object, entriesOK bool, root string, now time.Time, ttl time.Duration, maxBytes int64) (delEntries []object, delStaging []string) {
	var live []reservation
	for _, r := range groupStaging(staged, root) {
		if r.newest.Before(now.Add(-stagingMaxAge)) {
			delStaging = append(delStaging, r.keys...)
		} else {
			live = append(live, r)
		}
	}
	if maxBytes > 0 {
		var total int64
		for _, r := range live {
			total += r.bytes
		}
		for g := 0; g < len(live)-1 && total > StagingBudgetFor(maxBytes); g++ {
			delStaging = append(delStaging, live[g].keys...)
			total -= live[g].bytes
		}
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
	for ; i < len(sorted) && total > EntryBudgetFor(maxBytes); i++ {
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

	entries, err := listAll(ctx, rp.st, rp.lay.entriesRoot())
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

	root := rp.lay.stagingRoot()
	staged, err := listAll(ctx, rp.st, root)
	if err != nil {
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
		rp.deleteEntries(ctx, delEntries, &res)
	}
	rp.abortStaleUploads(ctx, now, &res)
	rp.log.Info("cache reaper pass",
		"entries", res.Entries, "bytes", res.Bytes, "staging_bytes", res.StagingBytes,
		"deleted_entries", res.DeletedEntries, "deleted_bytes", res.DeletedBytes,
		"deleted_staging", res.DeletedStaging, "aborted_uploads", res.AbortedUploads,
		"errors", res.Errors)
	return res, true
}

func (rp *reaper) deleteEntries(ctx context.Context, del []object, res *reapResult) {
	keys := make([]string, len(del))
	for i, o := range del {
		keys[i] = o.Key
	}
	failed := map[string]bool{}
	for _, k := range deleteBatched(ctx, rp.st, rp.log, keys) {
		failed[k] = true
	}
	res.Errors += len(failed)
	for _, o := range del {
		if !failed[o.Key] {
			res.DeletedEntries++
			res.DeletedBytes += o.Size
		}
	}
}

// abortStaleUploads aborts the multipart uploads under the prefix started
// more than stagingMaxAge before now.
func (rp *reaper) abortStaleUploads(ctx context.Context, now time.Time, res *reapResult) {
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
}
