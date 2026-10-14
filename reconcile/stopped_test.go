package reconcile

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// failOp fails every call of op with an APIError of the given status.
func failOp(op string, status int) func(string, string, int) error {
	return func(got, _ string, _ int) error {
		if got == op {
			return &githubapp.APIError{StatusCode: status}
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
	// Exact, so nothing else (env, labels, the JIT config) reaches a public checks tab.
	want := "Lux ended the Run that held this job's runner, so the runner could not finish the job.\n\n" +
		"| | |\n|---|---|\n" +
		"| Route | `default` |\n" +
		"| Lux Run | `" + id + "` |\n" +
		"| State | terminated |\n" +
		"| Reason | disk limit exceeded |\n" +
		"| Finished | 2026-10-09T12:00:30Z |\n" +
		"\nThe job wrote more than the route's 20 GiB disk: use a larger-disk profile such as `big` (runs-on: `linux, lux-big, self-hosted`, 40 GiB) or reduce disk use.\n"
	if req.Output.Summary != want {
		t.Fatalf("summary:\n%s\nwant:\n%s", req.Output.Summary, want)
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
	e := newEnv(t, noReruns)
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
	for _, state := range []string{"running", "stopping", "resuming"} {
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
	for _, op := range []string{opListChecks, opCreateCheck} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			id7 := e.startJob(7)
			id8 := e.startJob(8)
			e.lux.end(id7, diskStop)
			e.lux.end(id8, diskStop)
			e.gh.fail = failOp(op, 403)
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
		})
	}
}

func TestActionsPermissionMissingKeepsProvisioningAndChecks(t *testing.T) {
	e := newEnv(t)
	id7 := e.startJob(7)
	id8 := e.startJob(8)
	e.gh.fail = failOp(opCancel, 403)
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

func TestPermissionDeniedStopsFurtherCallsThisPass(t *testing.T) {
	for _, refusedOp := range []string{opListChecks, opCreateCheck, opCancel} {
		t.Run(refusedOp, func(t *testing.T) {
			e := newEnv(t)
			var ids []string
			for _, job := range []int64{7, 8, 9} {
				ids = append(ids, e.startJob(job))
			}
			for _, id := range ids {
				e.lux.end(id, diskStop)
			}
			e.clock.Add(2 * time.Minute)
			// Keep the jobs listed so the next pass can retry Checks too.
			var refused int
			e.gh.fail = func(op, _ string, _ int) error {
				if op == refusedOp {
					refused++
					return &githubapp.APIError{StatusCode: 403}
				}
				if op == opCancel {
					return &githubapp.APIError{StatusCode: 403}
				}
				return nil
			}
			if st := e.pass(); refused != 1 || st.Error != ErrPermission {
				t.Fatalf("first pass: refused %d, %+v", refused, st)
			}
			if st := e.pass(); refused != 2 || st.Error != ErrPermission {
				t.Fatalf("next pass: refused %d, %+v", refused, st)
			}
		})
	}
}

func TestJobsOnOtherRunnersAreIgnored(t *testing.T) {
	e := newEnv(t)
	names := []string{"hosted-runner", "lux-gha-t2-1001-7-0", "lux-gha-t1-1002-7-0", "lux-gha-t1-1001-x-0", "lux-gha-t1-1001-7-0-1",
		"lux-gha-t1-1001-007-0", "lux-gha-t1-1001-7-01", "lux-gha-t1-1001-+7-0", "lux-gha-t1-1001-7-+1", "lux-gha-t1-1001-7--1"}
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

func TestStatusReservationServesQueuedWorkflowBehindCappedActiveBacklog(t *testing.T) {
	e := newEnv(t)
	e.cfg.Routes[0].MaxRunners = 1
	e.startJob(7)
	e.cfg.Reconcile.MaxJobsPerPass = 2
	for _, job := range []int64{8, 9, 10, 11} {
		e.gh.queueIn(widgets, 70, job, "queued", lux3...)
	}
	e.gh.queueIn(widgets, 120, 12, "queued", "self-hosted", "linux", "lux-big")
	for range 3 {
		st := e.pass()
		if st.Demand["default"]+st.Demand["big"] > 2 {
			t.Fatalf("demand exceeds allowance: %+v", st)
		}
	}
	if len(e.lux.liveForJob("12")) != 1 {
		t.Fatal("queued workflow B did not get a Run")
	}
	for _, job := range []int64{8, 9, 10, 11} {
		if e.gh.job(job).status != "queued" || len(e.lux.liveForJob(itoa(job))) != 0 {
			t.Fatalf("capped backlog changed: %d", job)
		}
	}
}

func TestSingleDemandAllowanceAlternatesStatuses(t *testing.T) {
	for _, initialQueuedTurn := range []bool{false, true} {
		t.Run(strconv.FormatBool(initialQueuedTurn), func(t *testing.T) {
			e := newEnv(t)
			id := e.startJob(7)
			e.lux.end(id, diskStop)
			e.cfg.Reconcile.MaxJobsPerPass = 1
			e.rec.queuedTurn = initialQueuedTurn
			e.gh.queueIn(widgets, 80, 8, "queued", lux3...)
			for range 4 {
				st := e.pass()
				if st.Demand["default"] > 1 {
					t.Fatalf("demand exceeds allowance: %+v", st)
				}
				e.clock.Add(time.Minute)
			}
			checks, _ := e.gh.checks()
			if len(checks) != 1 || len(e.lux.liveForJob("8")) != 1 || !slices.Equal(e.gh.cancels(), []int64{70}) {
				t.Fatalf("checks %d, queued Runs %d, cancels %v", len(checks), len(e.lux.liveForJob("8")), e.gh.cancels())
			}
		})
	}
}

func TestMalformedRegistrationsAreNotOwnedHistory(t *testing.T) {
	for _, suffix := range []string{"007-0", "7-01", "+7-0", "7-+1", "7--1"} {
		t.Run(suffix, func(t *testing.T) {
			e := newEnv(t)
			e.gh.addRunner(widgets, 900, "lux-gha-t1-1001-"+suffix)
			e.gh.addRunner(widgets, 901, runner("8", 0))
			st := e.pass()
			if st.Error != "" || st.Provisioned != 0 || st.Live["default"] != 0 || e.lux.createCount() != 0 || !slices.Equal(e.lux.historyJobs, []string{"8"}) {
				t.Fatalf("status %+v, histories %v, creates %d", st, e.lux.historyJobs, e.lux.createCount())
			}
			if checks, _ := e.gh.checks(); len(checks) != 0 || len(e.gh.cancels()) != 0 || e.lux.runnerLists() != 0 {
				t.Fatalf("checks %v cancels %v runner reads %d", checks, e.gh.cancels(), e.lux.runnerLists())
			}
		})
	}
}

func TestMultibyteReasonSummaryRetainsExactPrefix(t *testing.T) {
	e := newEnv(t)
	id := e.startJob(7)
	e.lux.end(id, strings.Repeat("界", 30000))
	e.pass()
	_, reqs := e.gh.checks()
	if len(reqs) != 1 {
		t.Fatal(reqs)
	}
	prefix := "Lux ended the Run that held this job's runner, so the runner could not finish the job.\n\n" +
		"| | |\n|---|---|\n| Route | `default` |\n| Lux Run | `" + id + "` |\n| State | terminated |\n| Reason | "
	want := prefix + strings.Repeat("界", (githubapp.MaxCheckSummary-len(prefix))/3)
	got := reqs[0].Output.Summary
	if got != want || len(got) > githubapp.MaxCheckSummary || !utf8.ValidString(got) {
		t.Fatalf("summary length %d, want %d; exact prefix match %v, UTF8 %v", len(got), len(want), got == want, utf8.ValidString(got))
	}
}

func TestStoppedJobsProgressUnderPersistentQueuedBacklog(t *testing.T) {
	e := newEnv(t, replace(`"maxRunners": 10`, `"maxRunners": 10, "reconcile": {"maxJobsPerPass": 2}`))
	id := e.startJob(7)
	e.lux.end(id, diskStop)
	e.gh.queue(8, "queued", lux3...)
	e.gh.queue(9, "queued", lux3...)
	for range 4 {
		e.pass()
		e.clock.Add(time.Minute)
	}
	checks, _ := e.gh.checks()
	if e.lux.runnerLists() != 1 || len(checks) != 1 || !slices.Equal(e.gh.cancels(), []int64{70}) {
		t.Fatalf("runner lists %d, checks %d, cancels %v", e.lux.runnerLists(), len(checks), e.gh.cancels())
	}
	if e.gh.job(8).status != "queued" || e.gh.job(9).status != "queued" {
		t.Fatal("backlog did not persist")
	}
}

func TestStoppedJobsHaveOwnLuxAllowanceAndStillProcessCachedJobs(t *testing.T) {
	e := newEnv(t)
	for _, job := range []int64{1, 2, 3, 4} {
		e.startJob(job)
	}
	e.lux.end(e.runID("4"), diskStop)
	e.pass() // Cache the last job in listing order.
	lists := e.lux.runnerLists()
	for _, job := range []string{"1", "2", "3"} {
		e.lux.end(e.runID(job), diskStop)
	}
	e.cfg.MaxRunners = 1
	e.clock.Add(2 * time.Minute)
	st := e.pass()
	if e.lux.runnerLists()-lists != 1 || st.CheckRuns != 1 || st.ForceCancelled != 2 || !slices.Equal(e.gh.cancels(), []int64{10, 40}) {
		t.Fatalf("allowance exhausted before cached job: %+v lists %d cancels %v", st, e.lux.runnerLists()-lists, e.gh.cancels())
	}
}

func TestPartialWorkflowJobsStillExplainButDoNotCancel(t *testing.T) {
	e := newEnv(t)
	id := e.startJob(7)
	e.lux.end(id, diskStop)
	for _, job := range []int64{8, 9, 10} {
		e.gh.queueIn(widgets, 70, job, "queued", lux3...)
	}
	e.cfg.Reconcile.MaxJobsPerPass = 1
	e.clock.Add(2 * time.Minute)
	st := e.pass()
	if st.CheckRuns != 1 || st.ForceCancelled != 0 || len(e.gh.cancels()) != 0 {
		t.Fatalf("partial workflow: %+v cancels %v", st, e.gh.cancels())
	}
}

func TestCheckRunPaginationAfterRestart(t *testing.T) {
	e := newEnv(t)
	for _, job := range []int64{7, 8, 9} {
		e.gh.queueIn(widgets, 70, job, "queued", lux3...)
	}
	e.pass()
	for _, job := range []int64{7, 8, 9} {
		if err := e.gh.pick(runner(itoa(job), 0), job); err != nil {
			t.Fatal(err)
		}
		e.lux.end(e.runID(itoa(job)), diskStop)
	}
	if st := e.pass(); st.CheckRuns != 3 {
		t.Fatalf("initial: %+v", st)
	}
	e.rec = e.newReconciler()
	st := e.pass()
	checks, _ := e.gh.checks()
	if st.CheckRuns != 0 || len(checks) != 3 {
		t.Fatalf("restart: %+v checks %d", st, len(checks))
	}
}

func TestStoppedCacheSurvivesTruncationAndPrunesAfterCompletePass(t *testing.T) {
	e := newEnv(t)
	id := e.startJob(7)
	e.lux.end(id, diskStop)
	e.pass()
	lists := e.lux.runnerLists()
	// Hide the cached job behind a partially listed earlier workflow.
	e.gh.queueIn(widgets, 10, 1, "in_progress", "ubuntu-latest")
	for _, job := range []int64{2, 3, 4} {
		e.gh.queueIn(widgets, 10, job, "queued", lux3...)
	}
	e.cfg.Reconcile.MaxJobsPerPass = 1
	e.pass()
	for _, job := range []int64{1, 2, 3, 4} {
		e.gh.complete(job, "success")
	}
	e.cfg.Reconcile.MaxJobsPerPass = 200
	e.pass()
	if e.lux.runnerLists() != lists {
		t.Fatal("truncated pass discarded cache")
	}
	e.gh.complete(7, "cancelled")
	e.pass()
	e.gh.queueIn(widgets, 70, 7, "queued", lux3...)
	e.gh.pickElsewhere(widgets, 7, 900, runner("7", 0))
	e.pass()
	if e.lux.runnerLists() != lists+1 {
		t.Fatal("complete pass did not prune cache")
	}
}

func TestRemovedRepositoryPrunesStoppedCache(t *testing.T) {
	e := newDiscoveryEnv(t, 0)
	id := e.startJob(7)
	e.lux.end(id, diskStop)
	e.pass()
	if len(e.rec.stopped) != 1 {
		t.Fatal("no cached stopped job")
	}
	e.gh.repo(widgets).uninstalled = true
	e.rec.RequestRepositoryRefresh()
	e.pass()
	e.clock.Add(e.cfg.Reconcile.IdleGrace.Duration)
	e.pass()
	if len(e.rec.stopped) != 0 {
		t.Fatal("removed repository retained stopped jobs")
	}
}

func TestStoppedGitHubTransientErrorsRetry(t *testing.T) {
	for _, op := range []string{opCreateCheck, opListChecks, opCancel} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			id := e.startJob(7)
			e.lux.end(id, diskStop)
			if op == opCancel {
				e.clock.Add(2 * time.Minute)
			}
			e.gh.fail = failOp(op, 502)
			st := e.pass()
			checks, _ := e.gh.checks()
			if st.Error != ErrGitHub {
				t.Fatalf("failed call: %+v", st)
			}
			if op == opCancel {
				if st.ForceCancelled != 0 || len(e.gh.cancels()) != 0 || e.gh.job(7).status != "in_progress" {
					t.Fatalf("failed cancel changed workflow state: %+v", st)
				}
			} else if st.CheckRuns != 0 || len(checks) != 0 {
				t.Fatalf("failed Checks operation posted a check: %+v", st)
			}
			e.gh.fail = nil
			st = e.pass()
			if st.Error != "" || (op == opCancel && st.ForceCancelled != 1) || (op != opCancel && st.CheckRuns != 1) {
				t.Fatalf("retry: %+v", st)
			}
		})
	}
}

func TestStoppedRateLimitAborts(t *testing.T) {
	for _, op := range []string{opCreateCheck, opListChecks, opCancel} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			id := e.startJob(7)
			e.lux.end(id, diskStop)
			e.clock.Add(2 * time.Minute)
			e.gh.fail = func(got, _ string, _ int) error {
				if got == op {
					return &githubapp.APIError{StatusCode: 403, RateLimited: true, RetryAt: e.clock.Now().Add(time.Minute)}
				}
				return nil
			}
			p := &pass{r: e.rec, ctx: context.Background(), ended: map[string]bool{}, st: Status{Demand: map[string]int{}, Live: map[string]int{}}}
			if err := p.run(); !errors.Is(err, errAbort) || p.st.Error != ErrRateLimited {
				t.Fatalf("err %v status %+v", err, p.st)
			}
		})
	}
}

func TestEndedRunLuxFailureDoesNotBlockProvisioning(t *testing.T) {
	e := newEnv(t)
	id := e.startJob(7)
	e.lux.end(id, diskStop)
	e.gh.queue(8, "queued", lux3...)
	e.lux.failRunnerList = 1
	if st := e.pass(); st.Error != ErrLux || st.Provisioned != 1 || st.CheckRuns != 0 {
		t.Fatalf("status %+v", st)
	}
	if st := e.pass(); st.Error != "" || st.CheckRuns != 1 {
		t.Fatalf("retry %+v", st)
	}
}

func TestForceCancelConflictIsDoneWithoutRetry(t *testing.T) {
	e := newEnv(t)
	id := e.startJob(7)
	e.lux.end(id, diskStop)
	e.clock.Add(2 * time.Minute)
	calls := 0
	e.gh.fail = func(op, _ string, _ int) error {
		if op == opCancel {
			calls++
			return &githubapp.APIError{StatusCode: 409}
		}
		return nil
	}
	if st := e.pass(); st.Error != "" || st.ForceCancelled != 0 {
		t.Fatalf("status %+v", st)
	}
	e.gh.fail = func(op, _ string, _ int) error {
		if op == opCancel {
			calls++
		}
		return nil
	}
	for range 2 {
		e.pass()
	}
	if calls != 1 || len(e.gh.cancels()) != 0 {
		t.Fatalf("calls %d cancels %v", calls, e.gh.cancels())
	}
}

func TestMissingFinishedAtCountsFromFirstSight(t *testing.T) {
	e := newEnv(t)
	id := e.startJob(7)
	e.lux.endNoFinish(id, diskStop)
	e.clock.Add(time.Hour)
	e.pass()
	e.clock.Add(e.cfg.Reconcile.StuckJobGrace.Duration - time.Second)
	if st := e.pass(); st.ForceCancelled != 0 || len(e.gh.cancels()) != 0 {
		t.Fatalf("early %+v", st)
	}
	e.clock.Add(time.Second)
	if st := e.pass(); st.ForceCancelled != 1 {
		t.Fatalf("boundary %+v", st)
	}
}

func TestEndedRunSelectsNewestAndIgnoresOtherRepository(t *testing.T) {
	e := newEnv(t)
	id := e.startJob(7)
	e.lux.end(id, diskStop)
	spec := e.lux.all()[0].spec
	newID := e.lux.seed("new", spec)
	e.lux.end(newID, "timeout")
	spec.Labels[LabelRepo] = "1002"
	foreignID := e.lux.seed("foreign", spec)
	e.lux.end(foreignID, "terminated")
	e.pass()
	checks, _ := e.gh.checks()
	if len(checks) != 1 || checks[0].ExternalID != newID {
		t.Fatalf("checks %+v", checks)
	}
}

func TestPerRunnerLiveListingBlocksExplainAndCancel(t *testing.T) {
	e := newEnv(t)
	id := e.startJob(7)
	e.lux.end(id, diskStop)
	e.lux.runnerListState = "running"
	e.clock.Add(time.Hour)
	e.pass()
	checks, _ := e.gh.checks()
	if e.lux.runnerLists() != 1 || len(checks) != 0 || len(e.gh.cancels()) != 0 {
		t.Fatalf("lists %d checks %+v cancels %v", e.lux.runnerLists(), checks, e.gh.cancels())
	}
}

func TestStoppedHints(t *testing.T) {
	const lost = "The Run's host was taken away (preemption, drain or loss); "
	e := newEnv(t, bigDisk)
	for _, tc := range []struct {
		reason  string
		route   int
		attempt int64
		want    string
	}{
		{"disk limit exceeded", 0, 1, "The job wrote more than the route's 20 GiB disk: use a larger-disk profile such as `big` (runs-on: `linux, lux-big, self-hosted`, 40 GiB) or reduce disk use."},
		{"disk limit exceeded", 1, 1, "The job wrote more than the route's 40 GiB disk, the largest configured: reduce disk use."},
		{"disk limit exceeded", -1, 1, "The job wrote more than its disk limit: reduce disk use."},
		{"timeout", 0, 1, "The Run reached the route's Lux timeout (1h30m0s): shorten the job, or raise the route's timeout."},
		{"preempt: not resumed (resumePolicy never)", 0, 1, lost + "lux-gha re-runs the job automatically (attempt 2 of 3)."},
		{"drain: not resumed (resumePolicy never)", 0, 2, lost + "lux-gha re-runs the job automatically (attempt 3 of 3)."},
		{"host lost: missed heartbeats", 0, 3, lost + "re-run the job. Automatic re-runs are used up (this was attempt 3; lux-gha re-runs up to attempt 3)."},
		{"terminated", 0, 1, "The Run was terminated through the Lux API."},
		{"exit code 3", 0, 1, ""},
	} {
		t.Run(fmt.Sprintf("%s/%d/%d", tc.reason, tc.route, tc.attempt), func(t *testing.T) {
			var got string
			if tc.route < 0 {
				got = e.rec.hint(tc.reason, nil, tc.attempt)
			} else {
				got = e.rec.hint(tc.reason, &e.cfg.Routes[tc.route], tc.attempt)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
	off := newEnv(t, bigDisk, noReruns)
	if got, want := off.rec.hint("host lost", nil, 1), lost+"re-run the job. Automatic re-runs are off."; got != want {
		t.Fatalf("off: got %q want %q", got, want)
	}
}

// A size route's disk is its pool's size, unknown to the config: its hint
// names no route, and it is never offered as a larger-disk route.
func TestStoppedHintsForSizeRoutes(t *testing.T) {
	e := newEnv(t, bigDisk, withSizeRoute, replace(`"diskGiB": 20`, `"diskGiB": 10`))
	if got, want := e.rec.hint("disk limit exceeded", &e.cfg.Routes[0], 1),
		"The job wrote more than size `m-2-arm64`'s disk: ask for a larger size in `runs-on` or reduce disk use."; got != want {
		t.Fatalf("size route: got %q want %q", got, want)
	}
	if got, want := e.rec.hint("disk limit exceeded", &e.cfg.Routes[1], 1),
		"The job wrote more than the route's 10 GiB disk: use a larger-disk profile such as `big` (runs-on: `linux, lux-big, self-hosted`, 40 GiB) or reduce disk use."; got != want {
		t.Fatalf("raw route: got %q want %q", got, want)
	}
}

func TestMultibyteJobTitleIsBoundedAndValidUTF8(t *testing.T) {
	e := newEnv(t)
	id := e.startJob(7)
	e.gh.editJob(7, func(j *fakeJob) { j.name = strings.Repeat("界", 100) })
	e.lux.end(id, diskStop)
	e.pass()
	_, reqs := e.gh.checks()
	if len(reqs) != 1 {
		t.Fatal(reqs)
	}
	title := reqs[0].Output.Title
	want := `Lux stopped job "` + strings.Repeat("界", 79)
	if title != want || !utf8.ValidString(title) {
		t.Fatalf("title %q, want %q", title, want)
	}
}

func TestForceCancelIncludesQueuedAndHostedSiblings(t *testing.T) {
	for _, status := range []string{"queued", "in_progress"} {
		t.Run(status, func(t *testing.T) {
			e := newEnv(t)
			id := e.startJob(7)
			e.gh.queueIn(widgets, 70, 8, "queued", "ubuntu-latest")
			if status == "in_progress" {
				e.gh.pickElsewhere(widgets, 8, 900, "GitHub-hosted")
			}
			e.lux.end(id, diskStop)
			e.clock.Add(2 * time.Minute)
			e.pass()
			if !slices.Equal(e.gh.cancels(), []int64{70}) || e.gh.job(8).conclusion != "cancelled" {
				t.Fatalf("cancels %v sibling %+v", e.gh.cancels(), e.gh.job(8))
			}
		})
	}
}

func TestRequeuedJobUsesLiveNewGeneration(t *testing.T) {
	e := newEnv(t)
	id := e.startJob(7)
	e.lux.end(id, diskStop)
	e.gh.complete(7, "cancelled")
	e.gh.queueIn(widgets, 70, 7, "queued", lux3...)
	e.pass()
	if err := e.gh.pick(runner("7", 1), 7); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(time.Hour)
	e.pass()
	checks, _ := e.gh.checks()
	if len(checks) != 0 || len(e.gh.cancels()) != 0 || e.lux.runnerLists() != 0 {
		t.Fatalf("checks %+v cancels %v lists %d", checks, e.gh.cancels(), e.lux.runnerLists())
	}
}
