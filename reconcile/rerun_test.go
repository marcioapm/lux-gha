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

const hostLoss = "host lost: missed heartbeats; resumePolicy never"

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

// loseHost starts job 7 at run_attempt attempt and ends its Run for reason.
func (e *env) loseHost(attempt int64, reason string) string {
	e.t.Helper()
	id := e.startJob(7)
	e.gh.editJob(7, func(j *fakeJob) { j.attempt = attempt })
	e.lux.end(id, reason)
	return id
}

func TestHostLossRerunWaitsForTheWorkflowRunToComplete(t *testing.T) {
	e := newEnv(t)
	e.loseHost(1, hostLoss)
	ops := countOps(e, nil)
	st := e.pass()
	if st.Rerun != 0 || ops[opRerun] != 0 || ops[opGetRun] != 1 || st.Error != "" {
		t.Fatalf("re-ran an in_progress workflow run: %+v ops %v", st, ops)
	}
	_, reqs := e.gh.checks()
	if len(reqs) != 1 || !strings.HasSuffix(reqs[0].Output.Summary,
		"\nThe Run's host was taken away (preemption, drain or loss); lux-gha re-runs the job automatically (attempt 2 of 3).\n") {
		t.Fatalf("check runs %+v", reqs)
	}
	// unstick force-cancels it; the run is then completed and cancelled.
	e.clock.Add(2 * time.Minute)
	st = e.pass()
	if st.ForceCancelled != 1 || st.Rerun != 1 || !slices.Equal(e.gh.rerunJobs(), []int64{7}) || st.Error != "" {
		t.Fatalf("status %+v reruns %v", st, e.gh.rerunJobs())
	}
	if j := e.gh.job(7); j.status != "queued" || j.attempt != 2 {
		t.Fatalf("job %+v", j)
	}
	// The new attempt gets a Run of its own.
	if st := e.pass(); st.Provisioned != 1 || len(e.lux.liveForJob("7")) != 1 {
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

func TestHostLossRerunAfterTheRunnerReportsFailure(t *testing.T) {
	for conclusion, reruns := range map[string]int{"failure": 1, "success": 0} {
		t.Run(conclusion, func(t *testing.T) {
			e := newEnv(t)
			e.loseHost(1, "preempt: not resumed (resumePolicy never)")
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
			if len(e.gh.cancels()) != 1 || len(e.gh.rerunJobs()) != 0 || ops[opGetRun] != 0 {
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
		{nil, 3, 0, "re-run the job. Automatic re-runs are used up (3 attempts)."},
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
			e.pass()
			e.pass()
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
	if st := e.pass(); st.Rerun != 1 {
		t.Fatalf("first bridge: %+v", st)
	}
	// The new attempt ran elsewhere and completed: the run is completed again.
	e.gh.editJob(7, func(j *fakeJob) { j.status, j.conclusion = "completed", "success" })
	ops := countOps(e, nil)
	if st := other.Pass(t.Context()); st.Rerun != 0 || ops[opRerun] != 0 || ops[opGetRun] != 1 || st.Error != "" {
		t.Fatalf("second bridge: %+v ops %v", st, ops)
	}
	if st := other.Pass(t.Context()); ops[opGetRun] != 1 {
		t.Fatalf("second bridge kept the job: %+v ops %v", st, ops)
	}
}

func TestRerunRefusalsAreSettled(t *testing.T) {
	for _, err := range []*githubapp.APIError{
		{StatusCode: http.StatusForbidden, Already: true},
		{StatusCode: http.StatusConflict},
		{StatusCode: http.StatusUnprocessableEntity},
	} {
		t.Run(err.Error(), func(t *testing.T) {
			e := newEnv(t)
			e.loseHost(1, hostLoss)
			ops := countOps(e, func(op string) error {
				if op == opRerun {
					return err
				}
				return nil
			})
			e.clock.Add(2 * time.Minute)
			if st := e.pass(); st.Rerun != 0 || ops[opRerun] != 1 || st.Error != "" {
				t.Fatalf("status %+v ops %v", st, ops)
			}
			e.pass()
			if ops[opRerun] != 1 || ops[opGetRun] != 1 {
				t.Fatalf("retried a settled re-run: ops %v", ops)
			}
		})
	}
}

func TestRerunDeniedKeepsTheJobAndRecordsPermission(t *testing.T) {
	e := newEnv(t)
	e.loseHost(1, hostLoss)
	ops := countOps(e, func(op string) error {
		if op == opRerun {
			return &githubapp.APIError{StatusCode: http.StatusForbidden}
		}
		return nil
	})
	e.clock.Add(2 * time.Minute)
	if st := e.pass(); st.Error != ErrPermission || st.ForceCancelled != 1 || st.CheckRuns != 1 || st.Rerun != 0 || ops[opRerun] != 1 {
		t.Fatalf("status %+v ops %v", st, ops)
	}
	countOps(e, nil)
	if st := e.pass(); st.Rerun != 1 || st.Error != "" {
		t.Fatalf("after the grant: %+v", st)
	}
}

func TestRerunRateLimitAbortsThePass(t *testing.T) {
	for _, op := range []string{opGetRun, opRerun} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			e.loseHost(1, hostLoss)
			countOps(e, func(got string) error {
				if got == op {
					return &githubapp.APIError{StatusCode: 429, RateLimited: true, RetryAt: e.clock.Now().Add(time.Minute)}
				}
				return nil
			})
			e.clock.Add(2 * time.Minute)
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
	e.loseHost(1, hostLoss)
	e.pass()
	ops := countOps(e, func(op string) error {
		if op == opGetRun {
			return &githubapp.APIError{StatusCode: http.StatusNotFound}
		}
		return nil
	})
	e.pass()
	e.pass()
	if ops[opGetRun] != 1 || ops[opRerun] != 0 {
		t.Fatalf("ops %v", ops)
	}
}
