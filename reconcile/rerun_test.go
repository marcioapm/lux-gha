package reconcile

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux-gha/githubapp"
)

var noReruns = replace(`"maxRunners": 10`, `"maxRunners": 10, "reconcile": {"hostLossReruns": 0}`)

const (
	hostLoss = "host lost: missed heartbeats; resumePolicy never"
	preempt  = "preempt: not resumed (resumePolicy never)"
)

// countOps counts the calls of each fake GitHub operation; failing, when it
// returns an error for op, fails that call.
func countOps(e *env, failing func(op string) error) map[string]int {
	n := map[string]int{}
	e.gh.fail = func(op, _ string, _ int) error {
		n[op]++
		if failing != nil {
			return failing(op)
		}
		return nil
	}
	return n
}

func failing(op string, err error) func(string) error {
	return func(got string) error {
		if got == op {
			return err
		}
		return nil
	}
}

// lose queues jobs in workflow run 70 at run_attempt attempt, provisions
// them, has each picked by its runner and ends its Run for reason.
func (e *env) lose(attempt int64, reason string, jobs ...int64) {
	e.t.Helper()
	for _, job := range jobs {
		e.gh.queueIn(widgets, 70, job, "queued", lux3...)
		e.gh.editJob(job, func(j *fakeJob) { j.attempt = attempt })
	}
	e.pass()
	for _, job := range jobs {
		id := e.runID(itoa(job))
		if err := e.gh.pick(runner(itoa(job), 0), job); err != nil {
			e.t.Fatal(err)
		}
		e.lux.end(id, reason)
	}
}

// loseHost loses job 7's Run at run_attempt attempt; the job stays
// in_progress, as with a host gone without SIGTERM.
func (e *env) loseHost(attempt int64, reason string) { e.lose(attempt, reason, 7) }

// runnerCancels loses job 7's Run to a preemption whose SIGTERM made the
// runner cancel the job itself before the bridge saw it in_progress.
func (e *env) runnerCancels() {
	e.lose(1, preempt, 7)
	e.gh.complete(7, "cancelled")
}

func TestHostLossRerunWaitsForTheWorkflowRunToComplete(t *testing.T) {
	e := newEnv(t)
	e.loseHost(1, hostLoss)
	ops := countOps(e, nil)
	st := e.pass()
	if st.Rerun != 0 || st.Error != "" {
		t.Fatalf("re-ran an in_progress workflow run: %+v ops %v", st, ops)
	}
	_, reqs := e.gh.checks()
	if len(reqs) != 1 || !strings.HasSuffix(reqs[0].Output.Summary,
		"\nThe Run's host was taken away (preemption, drain or loss); lux-gha re-runs the job automatically (attempt 2 of 3).\n") {
		t.Fatalf("check runs %+v", reqs)
	}
	// unstick force-cancels it in a pass that listed the run in_progress.
	e.clock.Add(2 * time.Minute)
	if st := e.pass(); st.ForceCancelled != 1 || st.Rerun != 0 {
		t.Fatalf("unstick pass: %+v", st)
	}
	if ops[opGetRun]+ops[opGetJob]+ops[opRerun] != 0 {
		t.Fatalf("read GitHub for a run listed in progress: ops %v", ops)
	}
	if st := e.pass(); st.Rerun != 1 || !slices.Equal(e.gh.rerunJobs(), []int64{7}) || st.Error != "" || ops[opGetRun] != 1 || ops[opGetJob] != 0 {
		t.Fatalf("status %+v reruns %v ops %v", st, e.gh.rerunJobs(), ops)
	}
	next := e.gh.rerunOf(7)
	if j := e.gh.job(next); j.status != "queued" || j.attempt != 2 {
		t.Fatalf("job %+v", j)
	}
	// The new attempt's job gets a Run of its own.
	if st := e.pass(); st.Provisioned != 1 || len(e.lux.liveForJob(itoa(next))) != 1 {
		t.Fatalf("new attempt: %+v", st)
	}
	for range 3 {
		e.clock.Add(time.Minute)
		e.pass()
	}
	if len(e.gh.rerunJobs()) != 1 {
		t.Fatalf("reruns %v", e.gh.rerunJobs())
	}
}

// The common preemption: SIGTERM within the grace makes the runner cancel
// its job, so the bridge never sees it in_progress under an ended Run.
func TestHostLossRerunWhenTheRunnerCancelsItsOwnJob(t *testing.T) {
	e := newEnv(t)
	e.runnerCancels()
	ops := countOps(e, nil)
	st := e.pass()
	if st.Rerun != 1 || !slices.Equal(e.gh.rerunJobs(), []int64{7}) || len(e.gh.cancels()) != 0 || st.Error != "" {
		t.Fatalf("status %+v reruns %v cancels %v", st, e.gh.rerunJobs(), e.gh.cancels())
	}
	if ops[opGetJob] != 1 || ops[opGetRun] != 1 || ops[opRerun] != 1 {
		t.Fatalf("ops %v", ops)
	}
	for range 3 {
		e.pass()
	}
	if len(e.gh.rerunJobs()) != 1 || ops[opGetJob] != 1 {
		t.Fatalf("reruns %v ops %v", e.gh.rerunJobs(), ops)
	}
}

func TestHostLossRerunAfterTheRunnerReportsFailure(t *testing.T) {
	for conclusion, reruns := range map[string]int{"failure": 1, "success": 0} {
		t.Run(conclusion, func(t *testing.T) {
			e := newEnv(t)
			e.loseHost(1, preempt)
			e.pass()
			e.gh.complete(7, conclusion)
			e.pass()
			e.pass()
			if len(e.gh.rerunJobs()) != reruns || len(e.gh.cancels()) != 0 {
				t.Fatalf("reruns %v cancels %v", e.gh.rerunJobs(), e.gh.cancels())
			}
		})
	}
}

// A Run preempted before its runner took the job: the job ran on the next
// gen and failed there, so it is not the lost Run's to re-run.
func TestLostRunThatNeverHeldTheJobIsNotRerun(t *testing.T) {
	e := newEnv(t)
	e.gh.queueIn(widgets, 70, 7, "queued", lux3...)
	e.pass()
	e.lux.end(e.runID("7"), preempt)
	if st := e.pass(); st.Provisioned != 1 {
		t.Fatalf("next gen: %+v", st)
	}
	if err := e.gh.pick(runner("7", 1), 7); err != nil {
		t.Fatal(err)
	}
	e.gh.complete(7, "failure")
	ops := countOps(e, nil)
	e.pass()
	e.pass()
	if len(e.gh.rerunJobs()) != 0 || ops[opGetJob] != 1 || ops[opGetRun] != 0 {
		t.Fatalf("reruns %v ops %v", e.gh.rerunJobs(), ops)
	}
}

func TestJobCausedStopsAreNeverRerun(t *testing.T) {
	for _, reason := range []string{diskStop, "timeout; resumePolicy never", "terminated", "exit code 3"} {
		t.Run(reason, func(t *testing.T) {
			e := newEnv(t)
			e.loseHost(1, reason)
			ops := countOps(e, nil)
			e.pass()
			e.clock.Add(2 * time.Minute)
			e.pass()
			e.pass()
			if len(e.gh.cancels()) != 1 || len(e.gh.rerunJobs()) != 0 || ops[opGetRun]+ops[opGetJob] != 0 {
				t.Fatalf("cancels %v reruns %v ops %v", e.gh.cancels(), e.gh.rerunJobs(), ops)
			}
		})
	}
}

func TestHostLossRerunBound(t *testing.T) {
	for _, tc := range []struct {
		edit    func(string) string
		attempt int64
		reruns  int
		hint    string
	}{
		{nil, 2, 1, "lux-gha re-runs the job automatically (attempt 3 of 3)."},
		{nil, 3, 0, "re-run the job. Automatic re-runs are used up (this was attempt 3; lux-gha re-runs up to attempt 3)."},
		{noReruns, 1, 0, "re-run the job. Automatic re-runs are off."},
		{replace(`"maxRunners": 10`, `"maxRunners": 10, "reconcile": {"hostLossReruns": 5}`), 5, 1, "(attempt 6 of 6)."},
	} {
		t.Run(tc.hint, func(t *testing.T) {
			var edits []func(string) string
			if tc.edit != nil {
				edits = append(edits, tc.edit)
			}
			e := newEnv(t, edits...)
			e.loseHost(tc.attempt, hostLoss)
			e.pass()
			e.clock.Add(2 * time.Minute)
			for range 3 {
				e.pass()
			}
			_, reqs := e.gh.checks()
			if len(e.gh.rerunJobs()) != tc.reruns || len(reqs) != 1 || !strings.HasSuffix(reqs[0].Output.Summary, tc.hint+"\n") {
				t.Fatalf("reruns %v checks %+v", e.gh.rerunJobs(), reqs)
			}
		})
	}
}

// A second bridge, or this one before a restart, re-ran the workflow run:
// its run_attempt moved past the lost job's, so nothing is posted.
func TestRerunOfAnAdvancedAttemptIsNotPosted(t *testing.T) {
	e := newEnv(t)
	e.loseHost(1, hostLoss)
	other := e.newReconciler()
	e.pass()
	other.Pass(t.Context())
	e.clock.Add(2 * time.Minute)
	e.pass()
	if st := e.pass(); st.Rerun != 1 {
		t.Fatalf("first bridge: %+v", st)
	}
	// The new attempt ran elsewhere and failed: the run is completed and
	// failed again, and only its run_attempt tells it was re-run.
	e.gh.editJob(e.gh.rerunOf(7), func(j *fakeJob) { j.status, j.conclusion = "completed", "failure" })
	ops := countOps(e, nil)
	if st := other.Pass(t.Context()); st.Rerun != 0 || ops[opRerun] != 0 || ops[opGetRun] != 1 || st.Error != "" {
		t.Fatalf("second bridge: %+v ops %v", st, ops)
	}
	if st := other.Pass(t.Context()); ops[opGetRun] != 1 {
		t.Fatalf("second bridge kept the job: %+v ops %v", st, ops)
	}
}

// The lost queue is in memory; a restart finds the job again from Lux.
func TestRestartRediscoversALostJob(t *testing.T) {
	e := newEnv(t)
	e.loseHost(1, hostLoss)
	e.pass()
	e.clock.Add(2 * time.Minute)
	if st := e.pass(); st.ForceCancelled != 1 || st.Rerun != 0 {
		t.Fatalf("before the restart: %+v", st)
	}
	e.clock.Add(time.Hour)
	e.rec = e.newReconciler()
	if st := e.pass(); st.Rerun != 1 || !slices.Equal(e.gh.rerunJobs(), []int64{7}) {
		t.Fatalf("after the restart: %+v reruns %v", st, e.gh.rerunJobs())
	}
	// Another restart finds the run re-run already.
	e.rec = e.newReconciler()
	if st := e.pass(); st.Rerun != 0 || len(e.gh.rerunJobs()) != 1 {
		t.Fatalf("second restart: %+v", st)
	}
}

func TestLostRunsOutsideTheHorizonOrWithoutLabelsAreSkipped(t *testing.T) {
	t.Run("horizon", func(t *testing.T) {
		e := newEnv(t)
		e.runnerCancels()
		e.clock.Add(hostLossHorizon + time.Minute)
		e.rec = e.newReconciler()
		ops := countOps(e, nil)
		if st := e.pass(); st.Rerun != 0 || ops[opGetJob] != 0 {
			t.Fatalf("status %+v ops %v", st, ops)
		}
	})
	for _, label := range []string{LabelRepo, LabelJob, LabelRunID, LabelRunAttempt, LabelRunner} {
		t.Run(label, func(t *testing.T) {
			e := newEnv(t)
			e.runnerCancels()
			e.lux.mu.Lock()
			for _, r := range e.lux.runs {
				delete(r.run.Labels, label)
			}
			e.lux.mu.Unlock()
			ops := countOps(e, nil)
			if st := e.pass(); st.Rerun != 0 || ops[opGetJob] != 0 {
				t.Fatalf("status %+v ops %v", st, ops)
			}
		})
	}
}

// Each pass lists only the ended Runs created since the oldest Run live in
// the previous one, not the whole horizon.
func TestLostRunListingStartsAtTheOldestLiveRun(t *testing.T) {
	e := newEnv(t)
	created := e.clock.Now()
	e.lose(1, preempt, 7)
	e.clock.Add(time.Hour)
	e.pass()
	e.pass()
	got := e.lux.sinceListCalls()
	horizon := created.Add(-hostLossHorizon - 2*time.Hour)
	if len(got) != 3 || !got[0].Equal(horizon) || !got[1].Equal(created.Add(-lostCursorSkew)) || !got[2].Equal(created.Add(time.Hour-lostCursorSkew)) {
		t.Fatalf("since %v", got)
	}
}

func TestLostRunListingFailureIsALuxError(t *testing.T) {
	e := newEnv(t)
	e.runnerCancels()
	e.lux.failSinceList = true
	if st := e.pass(); st.Error != ErrLux || st.Rerun != 0 {
		t.Fatalf("status %+v", st)
	}
	e.lux.failSinceList = false
	if st := e.pass(); st.Rerun != 1 {
		t.Fatalf("after recovery: %+v", st)
	}
}

func TestSupersededWorkflowRunIsNotRerun(t *testing.T) {
	main, prBranch := "main", "feat/x"
	ours := githubapp.WorkflowRun{ID: 70, WorkflowID: 3, HeadBranch: &main, Event: "push", RunNumber: 5}
	for _, tc := range []struct {
		name   string
		other  *githubapp.WorkflowRun
		ours   githubapp.WorkflowRun
		reruns int
	}{
		{"newer push", &githubapp.WorkflowRun{ID: 90, WorkflowID: 3, HeadBranch: &main, Event: "push", RunNumber: 6}, ours, 0},
		{"older run", &githubapp.WorkflowRun{ID: 60, WorkflowID: 3, HeadBranch: &main, Event: "push", RunNumber: 4}, ours, 1},
		{"other branch", &githubapp.WorkflowRun{ID: 90, WorkflowID: 3, HeadBranch: &prBranch, Event: "push", RunNumber: 6}, ours, 1},
		{"other event", &githubapp.WorkflowRun{ID: 90, WorkflowID: 3, HeadBranch: &main, Event: "pull_request", RunNumber: 6}, ours, 1},
		{"no head branch", nil, githubapp.WorkflowRun{ID: 70, WorkflowID: 3, Event: "push", RunNumber: 5}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.gh.describeRun(tc.ours)
			if tc.other != nil {
				e.gh.describeRun(*tc.other)
			}
			e.runnerCancels()
			ops := countOps(e, nil)
			st := e.pass()
			if len(e.gh.rerunJobs()) != tc.reruns || st.Error != "" {
				t.Fatalf("status %+v reruns %v", st, e.gh.rerunJobs())
			}
			if want := map[bool]int{true: 1, false: 0}[tc.ours.HeadBranch != nil]; ops[opLatestRun] != want {
				t.Fatalf("ops %v", ops)
			}
			e.pass()
			if len(e.gh.rerunJobs()) != tc.reruns || ops[opLatestRun] > 1 {
				t.Fatalf("second pass: reruns %v ops %v", e.gh.rerunJobs(), ops)
			}
		})
	}
}

// A host is lost, then within stuckJobGrace a newer push cancels the run
// (concurrency cancel-in-progress): the superseded run is not re-run.
func TestHostLossRerunSkipsARunCancelledBySupersession(t *testing.T) {
	main := "main"
	e := newEnv(t)
	e.gh.describeRun(githubapp.WorkflowRun{ID: 70, WorkflowID: 3, HeadBranch: &main, Event: "push", RunNumber: 5})
	e.loseHost(1, hostLoss)
	e.pass()
	e.gh.describeRun(githubapp.WorkflowRun{ID: 90, WorkflowID: 3, HeadBranch: &main, Event: "push", RunNumber: 6})
	e.gh.complete(7, "cancelled")
	e.clock.Add(2 * time.Minute)
	e.pass()
	e.pass()
	if len(e.gh.rerunJobs()) != 0 || len(e.gh.cancels()) != 0 {
		t.Fatalf("reruns %v cancels %v", e.gh.rerunJobs(), e.gh.cancels())
	}
}

// Jobs of one attempt lost together are re-run with one rerun-failed-jobs:
// a job re-run starts a new attempt that its siblings are not part of.
func TestLostSiblingsAreRerunTogether(t *testing.T) {
	e := newEnv(t)
	e.lose(1, preempt, 7, 8)
	e.gh.complete(7, "cancelled")
	e.gh.complete(8, "cancelled")
	ops := countOps(e, nil)
	st := e.pass()
	if st.Rerun != 2 || !slices.Equal(e.gh.rerunJobs(), []int64{7, 8}) || ops[opRerunFailed] != 1 || ops[opRerun] != 0 || ops[opGetRun] != 1 {
		t.Fatalf("status %+v reruns %v ops %v", st, e.gh.rerunJobs(), ops)
	}
	for _, job := range []int64{7, 8} {
		if j := e.gh.job(e.gh.rerunOf(job)); j == nil || j.status != "queued" || j.attempt != 2 {
			t.Fatalf("job %d: %+v", job, j)
		}
	}
}

func TestRerunRefusals(t *testing.T) {
	for _, tc := range []struct {
		err   *githubapp.APIError
		error string
	}{
		{&githubapp.APIError{StatusCode: http.StatusForbidden}, ""},
		{&githubapp.APIError{StatusCode: http.StatusConflict}, ""},
		{&githubapp.APIError{StatusCode: http.StatusUnprocessableEntity}, ""},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			e := newEnv(t)
			e.runnerCancels()
			ops := countOps(e, failing(opRerun, tc.err))
			if st := e.pass(); st.Rerun != 0 || ops[opRerun] != 1 || st.Error != tc.error {
				t.Fatalf("status %+v ops %v", st, ops)
			}
			e.pass()
			if ops[opRerun] != 1 || ops[opGetRun] != 1 {
				t.Fatalf("retried a refused re-run: ops %v", ops)
			}
		})
	}
}

func TestRerunDeniedKeepsTheJobAndRecordsPermission(t *testing.T) {
	e := newEnv(t)
	e.runnerCancels()
	ops := countOps(e, failing(opRerun, &githubapp.APIError{StatusCode: http.StatusForbidden, NotAccessible: true}))
	if st := e.pass(); st.Error != ErrPermission || st.Rerun != 0 || ops[opRerun] != 1 {
		t.Fatalf("status %+v ops %v", st, ops)
	}
	if st := e.pass(); st.Error != ErrPermission || ops[opRerun] != 2 {
		t.Fatalf("denied again: %+v ops %v", st, ops)
	}
	countOps(e, nil)
	if st := e.pass(); st.Rerun != 1 || st.Error != "" {
		t.Fatalf("after the grant: %+v", st)
	}
}

// A missing permission is not a failed try: the job is retried every pass
// until the horizon drops it.
func TestRerunDeniedPastTheHorizonIsDropped(t *testing.T) {
	e := newEnv(t)
	e.runnerCancels()
	ops := countOps(e, failing(opRerun, &githubapp.APIError{StatusCode: http.StatusForbidden, NotAccessible: true}))
	for range maxRerunTries + 2 {
		e.pass()
	}
	if ops[opRerun] != maxRerunTries+2 {
		t.Fatalf("a denial counted as a failed try: ops %v", ops)
	}
	e.clock.Add(hostLossHorizon + time.Minute)
	before := ops[opGetRun]
	if st := e.pass(); st.Error != "" || ops[opGetRun] != before {
		t.Fatalf("status %+v ops %v", st, ops)
	}
}

func TestLostJobIsDroppedAfterRepeatedFailures(t *testing.T) {
	for _, op := range []string{opGetJob, opGetRun, opRerun} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			e.runnerCancels()
			ops := countOps(e, failing(op, &githubapp.APIError{StatusCode: http.StatusInternalServerError}))
			for range maxRerunTries {
				if st := e.pass(); st.Error != ErrGitHub {
					t.Fatalf("status %+v", st)
				}
			}
			if st := e.pass(); st.Error != "" || ops[op] != maxRerunTries {
				t.Fatalf("kept retrying: %+v ops %v", st, ops)
			}
		})
	}
}

func TestRerunRateLimitAbortsThePass(t *testing.T) {
	for _, op := range []string{opGetJob, opGetRun, opLatestRun, opRerun} {
		t.Run(op, func(t *testing.T) {
			main := "main"
			e := newEnv(t)
			e.gh.describeRun(githubapp.WorkflowRun{ID: 70, WorkflowID: 3, HeadBranch: &main, Event: "push", RunNumber: 5})
			e.runnerCancels()
			countOps(e, failing(op, &githubapp.APIError{StatusCode: 429, RateLimited: true, RetryAt: e.clock.Now().Add(time.Minute)}))
			if st := e.pass(); st.Error != ErrRateLimited || st.RateLimitedUntil == nil || st.Rerun != 0 {
				t.Fatalf("status %+v", st)
			}
			countOps(e, nil)
			e.clock.Add(time.Minute)
			if st := e.pass(); st.Rerun != 1 {
				t.Fatalf("after the limit: %+v", st)
			}
		})
	}
}

func TestRerunOfAGoneWorkflowRunIsDropped(t *testing.T) {
	e := newEnv(t)
	e.runnerCancels()
	ops := countOps(e, failing(opGetRun, &githubapp.APIError{StatusCode: http.StatusNotFound}))
	e.pass()
	e.pass()
	if ops[opGetRun] != 1 || ops[opRerun] != 0 {
		t.Fatalf("ops %v", ops)
	}
}
