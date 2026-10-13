package reconcile

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux-gha/githubapp"
)

const diskStop = "disk limit exceeded; resumePolicy never"

// bigDisk gives the "big" route 40 GiB, the next disk size up from
// default's 20 GiB.
var bigDisk = replace(`"timeout": "2h", "nestedContainers": true`, `"timeout": "2h", "diskGiB": 40, "nestedContainers": true`)

// startJob queues job in its own workflow run, provisions it, has its runner
// pick it, and returns the Run id.
func (e *env) startJob(job int64) string {
	e.t.Helper()
	e.gh.queueIn(widgets, job*10, job, "queued", lux3...)
	e.pass()
	id := e.runID(itoa(job))
	if err := e.gh.pick(runner(itoa(job), 0), job); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func denied(op string) func(string, string, int) error {
	return func(got, _ string, _ int) error {
		if got == op {
			return &githubapp.APIError{StatusCode: 403}
		}
		return nil
	}
}

func TestDiskLimitStopPostsOneCheckRunWithReasonAndHint(t *testing.T) {
	e := newEnv(t, bigDisk)
	id := e.startJob(7)
	e.pass()
	if checks, _ := e.gh.checks(); len(checks) != 0 {
		t.Fatalf("check run while the Run is live: %+v", checks)
	}
	e.clock.Add(30 * time.Second)
	finished := e.clock.Now()
	e.lux.end(id, diskStop)
	e.clock.Add(15 * time.Second)

	st := e.pass()
	checks, reqs := e.gh.checks()
	if st.CheckRuns != 1 || len(checks) != 1 || st.Error != "" {
		t.Fatalf("status %+v checks %+v", st, checks)
	}
	c, req := checks[0], reqs[0]
	if c.Name != "lux-gha" || c.HeadSHA != fakeSHA(70) || c.ExternalID != id || *c.Conclusion != "failure" || c.Status != "completed" {
		t.Fatalf("check run %+v", c)
	}
	if want := `Lux stopped job "build-7": disk limit exceeded`; req.Output.Title != want {
		t.Fatalf("title %q, want %q", req.Output.Title, want)
	}
	if !req.CompletedAt.Equal(finished) {
		t.Fatalf("completed_at %s, want the Run's finishedAt %s", req.CompletedAt, finished)
	}
	for _, want := range []string{
		"| Route | `default` |", "| Lux Run | `" + id + "` |", "| State | terminated |",
		"| Reason | " + diskStop + " |", "| Finished | 2026-10-09T12:00:30Z |",
		"use a larger-disk profile such as `big` (runs-on: `linux, lux-big, self-hosted`, 40 GiB) or reduce disk use",
	} {
		if !strings.Contains(req.Output.Summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, req.Output.Summary)
		}
	}

	if st := e.pass(); st.CheckRuns != 0 {
		t.Fatalf("second pass posted again: %+v", st)
	}
	// A restarted bridge remembers nothing; GitHub says it was posted.
	e.rec = e.newReconciler()
	if st := e.pass(); st.CheckRuns != 0 || st.Error != "" {
		t.Fatalf("restarted bridge: %+v", st)
	}
	if checks, _ := e.gh.checks(); len(checks) != 1 {
		t.Fatalf("check runs after restart %+v", checks)
	}
}

func TestStuckJobIsForceCancelledOnceAfterStuckJobGrace(t *testing.T) {
	e := newEnv(t)
	id := e.startJob(7)
	e.lux.end(id, "host lost: missed heartbeats; resumePolicy never")
	e.pass()
	e.clock.Add(2*time.Minute - time.Second)
	// A restart before the grace keeps the Run's finishedAt as its start.
	e.rec = e.newReconciler()
	if st := e.pass(); st.ForceCancelled != 0 || len(e.gh.cancels()) != 0 {
		t.Fatalf("force-cancelled before stuckJobGrace: %+v", st)
	}
	e.clock.Add(time.Second)
	st := e.pass()
	if st.ForceCancelled != 1 || !slices.Equal(e.gh.cancels(), []int64{70}) || st.Error != "" {
		t.Fatalf("status %+v cancels %v", st, e.gh.cancels())
	}
	if j := e.gh.job(7); j.status != "completed" || j.conclusion != "cancelled" {
		t.Fatalf("job %+v", j)
	}
	for range 3 {
		e.clock.Add(time.Minute)
		e.pass()
	}
	if checks, _ := e.gh.checks(); len(e.gh.cancels()) != 1 || len(checks) != 1 {
		t.Fatalf("cancels %v checks %d", e.gh.cancels(), len(checks))
	}
}

func TestStuckJobGraceIsConfigurable(t *testing.T) {
	e := newEnv(t, replace(`"maxRunners": 10`, `"maxRunners": 10, "reconcile": {"stuckJobGrace": "10m"}`))
	id := e.startJob(7)
	e.lux.end(id, "timeout; resumePolicy never")
	e.clock.Add(10*time.Minute - time.Second)
	e.pass()
	if len(e.gh.cancels()) != 0 {
		t.Fatalf("cancels %v", e.gh.cancels())
	}
	e.clock.Add(time.Second)
	e.pass()
	if !slices.Equal(e.gh.cancels(), []int64{70}) {
		t.Fatalf("cancels %v", e.gh.cancels())
	}
}

func TestJobThatCompletesOnItsOwnIsNotForceCancelled(t *testing.T) {
	e := newEnv(t)
	id := e.startJob(7)
	e.lux.end(id, diskStop)
	e.pass()
	// The runner caught SIGTERM and reported the job cancelled.
	e.gh.complete(7, "cancelled")
	e.clock.Add(time.Hour)
	e.pass()
	e.pass()
	if checks, _ := e.gh.checks(); len(e.gh.cancels()) != 0 || len(checks) != 1 {
		t.Fatalf("cancels %v checks %+v", e.gh.cancels(), checks)
	}
}

func TestSucceededRunWithLaggingJobGetsNeither(t *testing.T) {
	e := newEnv(t)
	id := e.startJob(7)
	e.lux.end(id, "succeeded; resumePolicy never")
	e.clock.Add(time.Hour)
	e.pass()
	if checks, _ := e.gh.checks(); len(e.gh.cancels()) != 0 || len(checks) != 0 {
		t.Fatalf("cancels %v checks %+v", e.gh.cancels(), checks)
	}
}

func TestLiveRunGetsNeitherCheckRunNorForceCancel(t *testing.T) {
	e := newEnv(t)
	id := e.startJob(7)
	for _, state := range []string{"running", "stopping"} {
		e.lux.setState(id, state)
		e.clock.Add(time.Hour)
		e.pass()
	}
	if checks, _ := e.gh.checks(); len(e.gh.cancels()) != 0 || len(checks) != 0 || e.lux.runnerLists() != 0 {
		t.Fatalf("cancels %v checks %+v runner lists %d", e.gh.cancels(), checks, e.lux.runnerLists())
	}
}

func TestSiblingWithALiveRunBlocksForceCancel(t *testing.T) {
	e := newEnv(t)
	// Jobs 7 and 8 of one workflow run, each on its own runner.
	e.gh.queueIn(widgets, 70, 7, "queued", lux3...)
	e.gh.queueIn(widgets, 70, 8, "queued", lux3...)
	e.pass()
	for _, job := range []int64{7, 8} {
		if err := e.gh.pick(runner(itoa(job), 0), job); err != nil {
			t.Fatal(err)
		}
	}
	e.lux.end(e.runID("7"), diskStop)
	e.clock.Add(time.Hour)
	e.pass()
	checks, _ := e.gh.checks()
	if len(checks) != 1 || len(e.gh.cancels()) != 0 {
		t.Fatalf("checks %+v cancels %v", checks, e.gh.cancels())
	}
	// Once job 8 finishes, nothing live is left to cancel.
	e.gh.complete(8, "success")
	e.lux.end(e.runID("8"), "succeeded; resumePolicy never")
	e.pass()
	if !slices.Equal(e.gh.cancels(), []int64{70}) {
		t.Fatalf("cancels %v", e.gh.cancels())
	}
}

func TestChecksPermissionMissingKeepsProvisioningAndForceCancel(t *testing.T) {
	e := newEnv(t)
	id7 := e.startJob(7)
	id8 := e.startJob(8)
	e.lux.end(id7, diskStop)
	e.lux.end(id8, diskStop)
	e.gh.fail = denied(opListChecks)
	e.clock.Add(2 * time.Minute)
	e.gh.queue(9, "queued", lux3...)
	st := e.pass()
	if st.Error != ErrPermission || st.Provisioned != 1 || st.ForceCancelled != 2 || st.CheckRuns != 0 {
		t.Fatalf("status %+v", st)
	}
	if !slices.Equal(e.gh.cancels(), []int64{70, 80}) {
		t.Fatalf("cancels %v", e.gh.cancels())
	}
	e.gh.fail = nil
	e.gh.queue(10, "queued", lux3...)
	if st := e.pass(); st.Error != "" || st.Provisioned != 1 {
		t.Fatalf("after the permission is granted: %+v", st)
	}
}

func TestActionsPermissionMissingKeepsProvisioningAndChecks(t *testing.T) {
	e := newEnv(t)
	id7 := e.startJob(7)
	id8 := e.startJob(8)
	e.gh.fail = denied(opCancel)
	e.lux.end(id7, diskStop)
	e.lux.end(id8, diskStop)
	e.clock.Add(2 * time.Minute)
	e.gh.queue(9, "queued", lux3...)
	st := e.pass()
	checks, _ := e.gh.checks()
	if st.Error != ErrPermission || st.Provisioned != 1 || st.CheckRuns != 2 || len(checks) != 2 || st.ForceCancelled != 0 {
		t.Fatalf("status %+v checks %+v", st, checks)
	}
	// Retried next pass, and done once granted.
	e.gh.fail = nil
	if st := e.pass(); st.ForceCancelled != 2 || st.Error != "" {
		t.Fatalf("status %+v", st)
	}
}

func TestPermissionErrorIsLoggedOncePerPass(t *testing.T) {
	e := newEnv(t)
	var ids []string
	for _, job := range []int64{7, 8, 9} {
		ids = append(ids, e.startJob(job))
	}
	for _, id := range ids {
		e.lux.end(id, diskStop)
	}
	var refused int
	e.gh.fail = func(op, _ string, _ int) error {
		if op == opListChecks {
			refused++
			return &githubapp.APIError{StatusCode: 403}
		}
		return nil
	}
	e.pass()
	if refused != 1 {
		t.Fatalf("Checks called %d times after a 403 in one pass", refused)
	}
}

func TestJobsOnOtherRunnersAreIgnored(t *testing.T) {
	e := newEnv(t)
	names := []string{"hosted-runner", "lux-gha-t2-1001-7-0", "lux-gha-t1-1002-7-0", "lux-gha-t1-1001-x-0", "lux-gha-t1-1001-7-0-1"}
	for i, name := range names {
		job := int64(20 + i)
		e.gh.queueIn(widgets, job*10, job, "queued", "ubuntu-latest")
		e.gh.pickElsewhere(widgets, job, int64(900+i), name)
	}
	e.clock.Add(time.Hour)
	e.pass()
	e.pass()
	if checks, _ := e.gh.checks(); len(checks) != 0 || len(e.gh.cancels()) != 0 || e.lux.runnerLists() != 0 {
		t.Fatalf("checks %+v cancels %v runner lists %d", checks, e.gh.cancels(), e.lux.runnerLists())
	}
}

func TestStoppedJobsShareTheJobBudget(t *testing.T) {
	e := newEnv(t, replace(`"maxRunners": 10`, `"maxRunners": 10, "reconcile": {"maxJobsPerPass": 2}`))
	var ids []string
	for _, job := range []int64{1, 2, 3} {
		ids = append(ids, e.startJob(job))
	}
	for _, id := range ids {
		e.lux.end(id, diskStop)
	}
	// Two queued jobs take the whole budget: no Run is read.
	e.gh.queue(4, "queued", "self-hosted", "gpu")
	e.gh.queue(5, "queued", lux3...)
	e.gh.queue(6, "queued", lux3...)
	lists := e.lux.runnerLists()
	if st := e.pass(); st.CheckRuns != 0 || e.lux.runnerLists() != lists {
		t.Fatalf("budget spent on demand, yet %d Runs read: %+v", e.lux.runnerLists()-lists, st)
	}
	e.gh.complete(5, "cancelled")
	e.gh.complete(6, "cancelled")
	// Each pass reads at most two Runs; a read Run is not read again.
	st := e.pass()
	if st.CheckRuns != 2 || e.lux.runnerLists()-lists != 2 {
		t.Fatalf("first pass: %+v, %d Runs read", st, e.lux.runnerLists()-lists)
	}
	st = e.pass()
	if st.CheckRuns != 1 || e.lux.runnerLists()-lists != 3 {
		t.Fatalf("second pass: %+v, %d Runs read", st, e.lux.runnerLists()-lists)
	}
	if checks, _ := e.gh.checks(); len(checks) != 3 {
		t.Fatalf("checks %+v", checks)
	}
}
