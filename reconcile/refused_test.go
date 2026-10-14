package reconcile

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux-gha/githubapp"
)

// shapedPool is the "shaped" pool's ladder without m-2-arm64, the size
// withSizeRoute asks for.
var shapedPool = map[string][]string{"shaped": {"m-05-arm64", "m-1-arm64", "m-4-arm64"}}

func TestSizeRefusalFailsTheJobAtOnceAndIsNotRetried(t *testing.T) {
	e := newEnv(t, withSizeRoute)
	e.lux.sizes = shapedPool
	e.gh.queue(7, "queued", "lux-m-2-arm64")
	e.gh.queue(8, "queued", lux3...)
	st := e.pass()
	if st.Error != ErrSizeRefused || e.lux.refusedCount() != 1 {
		t.Fatalf("status %+v refused %d", st, e.lux.refusedCount())
	}
	// The refusal does not stop provisioning of other jobs.
	if st.Provisioned != 1 || len(e.lux.liveForJob("8")) != 1 || len(e.lux.liveForJob("7")) != 0 {
		t.Fatalf("status %+v live %+v", st, e.lux.live())
	}
	// The runner registered for the refused Run is deleted at once.
	if !slices.Equal(e.gh.deletedNames(), []string{runner("7", 0)}) || slices.Contains(e.gh.runnerNames(), runner("7", 0)) {
		t.Fatalf("deleted %v runners %v", e.gh.deletedNames(), e.gh.runnerNames())
	}
	checks, reqs := e.gh.checks()
	if st.CheckRuns != 1 || len(checks) != 1 {
		t.Fatalf("status %+v checks %+v", st, checks)
	}
	c, req := checks[0], reqs[0]
	if c.Name != CheckName || c.HeadSHA != fakeSHA(70) || c.ExternalID != "job-7" || *c.Conclusion != "failure" {
		t.Fatalf("check run %+v", c)
	}
	if want := `Lux refused job "build-7": unknown_size`; req.Output.Title != want {
		t.Fatalf("title %q, want %q", req.Output.Title, want)
	}
	want := "Lux refused the Run for this job's runner, so no runner can take the job. " +
		"The bridge does not retry it and cancels the workflow run.\n\n" +
		"| | |\n|---|---|\n" +
		"| Route | `m-2` |\n" +
		"| Pool | `shaped` |\n" +
		"| Size | `m-2-arm64` |\n" +
		"| Lux error | `unknown_size` |\n" +
		"\nPool `shaped` has no size `m-2-arm64`. Use a size the pool has in `runs-on` (`lux pools sizes shaped` lists them), " +
		"or ask the bridge's operator to fix route `m-2`.\n"
	if req.Output.Summary != want {
		t.Fatalf("summary:\n%s\nwant:\n%s", req.Output.Summary, want)
	}
	if st.ForceCancelled != 1 || !slices.Equal(e.gh.cancels(), []int64{70}) {
		t.Fatalf("status %+v cancels %v", st, e.gh.cancels())
	}
	if j := e.gh.job(7); j.status != "completed" || j.conclusion != "cancelled" {
		t.Fatalf("job %+v", j)
	}
	for range 3 {
		e.clock.Add(time.Hour)
		if st := e.pass(); st.Error != "" {
			t.Fatalf("later pass %+v", st)
		}
	}
	if checks, _ := e.gh.checks(); e.lux.refusedCount() != 1 || len(checks) != 1 || len(e.gh.cancels()) != 1 || e.gh.jitCount() != 2 {
		t.Fatalf("refused %d checks %d cancels %v jit %d", e.lux.refusedCount(), len(checks), e.gh.cancels(), e.gh.jitCount())
	}
}

// A sibling holding a live runner holds the force-cancel back: the refused
// job stays queued, and is still neither resubmitted nor explained twice.
func TestSizeRefusedJobWaitingForCancelIsNotResubmitted(t *testing.T) {
	e := newEnv(t, withSizeRoute)
	e.lux.sizes = map[string][]string{}
	e.gh.queueIn(widgets, 70, 8, "queued", lux3...)
	e.pass()
	if err := e.gh.pick(runner("8", 0), 8); err != nil {
		t.Fatal(err)
	}
	e.gh.queueIn(widgets, 70, 7, "queued", "lux-m-2-arm64")
	if st := e.pass(); st.Error != ErrSizeRefused || e.lux.refusedCount() != 1 {
		t.Fatalf("status %+v refused %d", st, e.lux.refusedCount())
	}
	_, reqs := e.gh.checks()
	if len(reqs) != 1 || reqs[0].Output.Title != `Lux refused job "build-7": size_unsupported` ||
		!strings.Contains(reqs[0].Output.Summary, "Pool `shaped` has no shape, so it takes no size: route `m-2` must ask for cpus, memoryMiB and diskGiB instead, or name a shaped pool.") {
		t.Fatalf("check runs %+v", reqs)
	}
	for range 3 {
		e.clock.Add(time.Hour)
		e.pass()
	}
	if checks, _ := e.gh.checks(); e.lux.refusedCount() != 1 || len(checks) != 1 || len(e.gh.cancels()) != 0 || e.gh.job(7).status != "queued" {
		t.Fatalf("refused %d checks %d cancels %v job %+v", e.lux.refusedCount(), len(checks), e.gh.cancels(), e.gh.job(7))
	}
	// A restarted bridge submits once more, is refused, and finds its check run posted.
	e.rec = e.newReconciler()
	e.pass()
	if checks, _ := e.gh.checks(); e.lux.refusedCount() != 2 || len(checks) != 1 {
		t.Fatalf("after restart: refused %d checks %d", e.lux.refusedCount(), len(checks))
	}
	e.gh.complete(8, "success")
	e.lux.end(e.runID("8"), "succeeded; resumePolicy never")
	e.pass()
	if !slices.Equal(e.gh.cancels(), []int64{70}) || e.gh.job(7).conclusion != "cancelled" || e.lux.refusedCount() != 2 {
		t.Fatalf("cancels %v job %+v refused %d", e.gh.cancels(), e.gh.job(7), e.lux.refusedCount())
	}
}

// A refusal posted while GitHub refused the Checks or Actions write is
// done on a later pass, without another submission.
func TestSizeRefusalCheckAndCancelRetryWithoutResubmitting(t *testing.T) {
	e := newEnv(t, withSizeRoute)
	e.lux.sizes = shapedPool
	e.gh.fail = func(op, _ string, _ int) error {
		if op == opCreateCheck || op == opCancel {
			return &githubapp.APIError{StatusCode: 500}
		}
		return nil
	}
	e.gh.queue(7, "queued", "lux-m-2-arm64")
	e.pass()
	if checks, _ := e.gh.checks(); len(checks) != 0 || len(e.gh.cancels()) != 0 || e.lux.refusedCount() != 1 {
		t.Fatalf("checks %d cancels %v refused %d", len(checks), e.gh.cancels(), e.lux.refusedCount())
	}
	e.gh.fail = nil
	e.pass()
	if checks, _ := e.gh.checks(); len(checks) != 1 || !slices.Equal(e.gh.cancels(), []int64{70}) || e.lux.refusedCount() != 1 {
		t.Fatalf("checks %d cancels %v refused %d", len(checks), e.gh.cancels(), e.lux.refusedCount())
	}
}

// refuseKeptQueued refuses job 7 with Actions write denied, so the job
// stays queued with its refusal remembered.
func refuseKeptQueued(t *testing.T, e *env) {
	t.Helper()
	e.lux.sizes = shapedPool
	e.gh.fail = failOp(opCancel, 403)
	e.gh.queue(7, "queued", "lux-m-2-arm64")
	e.pass()
	if e.lux.refusedCount() != 1 || len(e.rec.refused) != 1 || e.gh.job(7).status != "queued" {
		t.Fatalf("refused %d records %d job %+v", e.lux.refusedCount(), len(e.rec.refused), e.gh.job(7))
	}
}

func TestRemovedRepositoryForgetsRefusals(t *testing.T) {
	e := newDiscoveryEnv(t, 0, withSizeRoute)
	refuseKeptQueued(t, e)
	e.gh.repo(widgets).uninstalled = true
	e.rec.RequestRepositoryRefresh()
	e.pass()
	e.clock.Add(e.cfg.Reconcile.IdleGrace.Duration)
	e.pass()
	if len(e.rec.refused) != 0 {
		t.Fatalf("removed repository retained %d refusals", len(e.rec.refused))
	}
}

// Refused jobs stay demand but take none of the queued budget, so a job
// listed after them is still provisioned.
func TestRefusedJobsDoNotUseTheQueuedBudget(t *testing.T) {
	e := newEnv(t, withSizeRoute, replace(`"maxRunners": 10`, `"maxRunners": 10, "reconcile": {"maxJobsPerPass": 2}`))
	refuseKeptQueued(t, e)
	e.gh.queue(8, "queued", lux3...)
	st := e.pass()
	if st.Provisioned != 1 || len(e.lux.liveForJob("8")) != 1 {
		t.Fatalf("status %+v live %+v", st, e.lux.live())
	}
	if e.lux.refusedCount() != 1 || len(e.rec.refused) != 1 {
		t.Fatalf("refused %d records %d", e.lux.refusedCount(), len(e.rec.refused))
	}
}

// A refused job listed after the job that spends the last of the queued
// budget stays demand, and its workflow run is not marked partially read,
// so its pending cancel is retried.
func TestRefusedJobAfterExhaustedBudgetIsStillClosed(t *testing.T) {
	e := newEnv(t, withSizeRoute)
	refuseKeptQueued(t, e)
	// Job 6 is listed before job 7 in workflow run 70 and takes the one
	// queued slot of maxJobsPerPass 2.
	e.gh.queueIn(widgets, 70, 6, "queued", lux3...)
	e.cfg.Reconcile.MaxJobsPerPass = 2
	e.gh.fail = nil
	st := e.pass()
	if st.Provisioned != 1 || len(e.lux.liveForJob("6")) != 1 {
		t.Fatalf("status %+v live %+v", st, e.lux.live())
	}
	if !slices.Equal(e.gh.cancels(), []int64{70}) || e.gh.job(7).conclusion != "cancelled" {
		t.Fatalf("cancels %v job %+v", e.gh.cancels(), e.gh.job(7))
	}
	if e.lux.refusedCount() != 1 {
		t.Fatalf("job 7 submitted again: refused %d", e.lux.refusedCount())
	}
}

// A refused job's runner left registered by a failed delete holds no cap
// slot, and orphan cleanup still deletes it.
func TestLeftoverRunnerOfRefusedJobHoldsNoCapSlot(t *testing.T) {
	e := newEnv(t, withSizeRoute, replace(`"maxRunners": 10`, `"maxRunners": 1`))
	e.lux.sizes = shapedPool
	e.gh.fail = func(op, _ string, _ int) error {
		switch op {
		case opCancel:
			return &githubapp.APIError{StatusCode: 403}
		case opDelete:
			return &githubapp.APIError{StatusCode: 500}
		}
		return nil
	}
	e.gh.queue(7, "queued", "lux-m-2-arm64")
	e.pass()
	if e.lux.refusedCount() != 1 || !slices.Contains(e.gh.runnerNames(), runner("7", 0)) {
		t.Fatalf("refused %d runners %v", e.lux.refusedCount(), e.gh.runnerNames())
	}
	e.gh.queue(8, "queued", lux3...)
	if st := e.pass(); st.Provisioned != 1 || len(e.lux.liveForJob("8")) != 1 {
		t.Fatalf("status %+v live %+v", st, e.lux.live())
	}
	e.gh.fail = failOp(opCancel, 403)
	e.clock.Add(e.cfg.Reconcile.IdleGrace.Duration)
	e.pass()
	if !slices.Equal(e.gh.deletedNames(), []string{runner("7", 0)}) || e.lux.refusedCount() != 1 {
		t.Fatalf("deleted %v refused %d", e.gh.deletedNames(), e.lux.refusedCount())
	}
}

// A pass truncated before job 7 keeps its refusal, so the next complete
// pass does not submit it again.
func TestTruncatedPassKeepsRefusal(t *testing.T) {
	e := newEnv(t, withSizeRoute)
	refuseKeptQueued(t, e)
	// An earlier workflow run's jobs use the whole queued budget.
	e.gh.queueIn(widgets, 10, 1, "queued", lux3...)
	e.gh.queueIn(widgets, 10, 2, "queued", lux3...)
	e.cfg.Reconcile.MaxJobsPerPass = 2
	e.pass()
	e.cfg.Reconcile.MaxJobsPerPass = 200
	e.pass()
	if e.lux.refusedCount() != 1 {
		t.Fatalf("job 7 submitted again: refused %d", e.lux.refusedCount())
	}
}

// A 422 with a code other than a size refusal leaves the outcome unknown:
// the runner is kept for orphan cleanup, and the job is neither explained
// nor cancelled.
func TestNonSize422KeepsUnknownOutcomePath(t *testing.T) {
	e := newEnv(t)
	e.lux.failCreate = 1
	e.lux.failCreateCode = "invalid_spec"
	e.gh.queue(7, "queued", lux3...)
	st := e.pass()
	if st.Error != ErrLux || !slices.Contains(e.gh.runnerNames(), runner("7", 0)) || len(e.gh.deletedNames()) != 0 {
		t.Fatalf("status %+v runners %v deleted %v", st, e.gh.runnerNames(), e.gh.deletedNames())
	}
	if checks, _ := e.gh.checks(); len(checks) != 0 || len(e.gh.cancels()) != 0 || len(e.rec.refused) != 0 {
		t.Fatalf("checks %d cancels %v refusals %d", len(checks), e.gh.cancels(), len(e.rec.refused))
	}
}
