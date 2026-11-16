package reconcile

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux-gha/githubapp"
	"github.com/marcioapm/lux-gha/luxclient"
)

// searchRecorder records the lost-runner search's GitHub listings: every
// unfiltered-status runs page and the runs it returned, and the workflow
// runs whose jobs were listed.
type searchRecorder struct {
	GitHub
	mu       sync.Mutex
	runPages int
	listed   []int64
	jobRuns  []int64
}

func (g *searchRecorder) ListWorkflowRuns(ctx context.Context, repo githubapp.Repository, opts githubapp.WorkflowRunListOptions) (githubapp.WorkflowRunPage, error) {
	out, err := g.GitHub.ListWorkflowRuns(ctx, repo, opts)
	if opts.Status == "" {
		g.mu.Lock()
		g.runPages++
		for _, run := range out.Runs {
			g.listed = append(g.listed, run.ID)
		}
		g.mu.Unlock()
	}
	return out, err
}

func (g *searchRecorder) ListWorkflowRunJobs(ctx context.Context, repo githubapp.Repository, runID int64, opts githubapp.WorkflowJobListOptions) (githubapp.WorkflowJobPage, error) {
	g.mu.Lock()
	g.jobRuns = append(g.jobRuns, runID)
	g.mu.Unlock()
	return g.GitHub.ListWorkflowRunJobs(ctx, repo, runID, opts)
}

func (e *env) recordSearch() *searchRecorder {
	g := &searchRecorder{GitHub: e.gh}
	e.rec.gh = g
	return g
}

// completedRuns adds n workflow runs from first, each with one job that
// succeeded on a GitHub-hosted runner now.
func (e *env) completedRuns(first int64, n int) {
	for i := range int64(n) {
		e.gh.queueIn(widgets, first+i, 10_000+first+i, "completed", "ubuntu-latest")
		e.gh.editJob(10_000+first+i, func(j *fakeJob) { j.conclusion = "success" })
	}
}

// windowedLoss starts workflow run actualRun now with a job running on a
// GitHub-hosted runner (job actualRun+1000), and lead later provisions a Lux
// Run for job trigger of workflow run 70. That Run's runner takes job actual
// of actualRun instead, and its host is lost ten minutes later; actualRun
// then completes cancelled. It returns the Lux Run's id.
func (e *env) windowedLoss(actualRun, trigger, actual int64, lead time.Duration) string {
	e.t.Helper()
	other := actualRun + 1000
	e.gh.queueIn(widgets, actualRun, other, "queued", "ubuntu-latest")
	e.gh.pickElsewhere(widgets, other, 9000+other, "hosted-"+itoa(other))
	e.clock.Add(lead)
	e.gh.queueIn(widgets, 70, trigger, "queued", lux3...)
	e.pass()
	id := e.runID(itoa(trigger))
	e.gh.queueIn(widgets, actualRun, actual, "queued", lux3...)
	if err := e.gh.pick(runner(itoa(trigger), 0), actual); err != nil {
		e.t.Fatal(err)
	}
	e.gh.pickElsewhere(widgets, trigger, 9000+trigger, "hosted-"+itoa(trigger))
	e.gh.complete(trigger, "success")
	e.clock.Add(10 * time.Minute)
	e.lux.end(id, preempt)
	e.gh.complete(actual, "cancelled")
	e.gh.complete(other, "success")
	return id
}

// The first pilot repository's shape: the executing job's workflow run was
// created 40 minutes before the lost Run and 200 newer runs followed the
// loss. The search lists only the runs created by the window's end.
func TestLostSearchReachesTheJobOnABusyRepository(t *testing.T) {
	e := newEnv(t)
	e.gh.perPage = 100
	e.windowedLoss(80, 7, 8, 40*time.Minute)
	e.clock.Add(5 * time.Minute)
	e.completedRuns(1000, 200)
	g := e.recordSearch()
	ops := countOps(e, nil)
	st := e.pass()
	t.Logf("search: %d runs pages, jobs listed for runs %v; all GitHub calls %v", g.runPages, g.jobRuns, ops)
	if st.Error != "" || st.Rerun != 1 || !slices.Equal(e.gh.rerunJobs(), []int64{8}) {
		t.Fatalf("status %+v reruns %v", st, e.gh.rerunJobs())
	}
	if j := e.gh.job(e.gh.rerunOf(8)); j == nil || j.runID != 80 || j.attempt != 2 {
		t.Fatalf("new attempt %+v", j)
	}
	if g.runPages != 1 || !slices.Equal(g.jobRuns, []int64{70, 80}) {
		t.Fatalf("search pages %d, jobs listed for %v", g.runPages, g.jobRuns)
	}
	for range 3 {
		e.pass()
	}
	if !slices.Equal(e.gh.rerunJobs(), []int64{8}) {
		t.Fatalf("duplicate reruns: %v", e.gh.rerunJobs())
	}
}

// A run created after the window's end, while the lost Run was long gone,
// is never listed, so it costs no job page either.
func TestLostSearchNeverListsRunsCreatedAfterTheWindow(t *testing.T) {
	e := newEnv(t)
	e.gh.perPage = 100
	e.windowedLoss(80, 7, 8, 40*time.Minute)
	e.clock.Add(2 * time.Minute)
	e.completedRuns(900, 1)
	g := e.recordSearch()
	if st := e.pass(); st.Error != "" || !slices.Equal(e.gh.rerunJobs(), []int64{8}) {
		t.Fatalf("status %+v reruns %v", st, e.gh.rerunJobs())
	}
	if slices.Contains(g.listed, 900) || slices.Contains(g.jobRuns, 900) {
		t.Fatalf("listed a run created after the window: runs %v jobs %v", g.listed, g.jobRuns)
	}
}

// 250 runs that completed after the executing job's run was created but
// before the lost Run was: more than the job-page budget, so listing their
// jobs would stop the search before it reached the job.
func TestLostSearchSkipsRunsThatFinishedBeforeTheWindow(t *testing.T) {
	e := newEnv(t)
	e.gh.perPage = 100
	e.gh.queueIn(widgets, 80, 1080, "queued", "ubuntu-latest")
	e.gh.pickElsewhere(widgets, 1080, 9080, "hosted-80")
	e.clock.Add(10 * time.Minute)
	e.completedRuns(1000, 250)
	e.clock.Add(30 * time.Minute)
	e.gh.queueIn(widgets, 70, 7, "queued", lux3...)
	e.pass()
	id := e.runID("7")
	e.gh.queueIn(widgets, 80, 8, "queued", lux3...)
	if err := e.gh.pick(runner("7", 0), 8); err != nil {
		t.Fatal(err)
	}
	e.gh.pickElsewhere(widgets, 7, 9007, "hosted-7")
	e.gh.complete(7, "success")
	e.clock.Add(10 * time.Minute)
	e.lux.end(id, preempt)
	e.gh.complete(8, "cancelled")
	e.gh.complete(1080, "success")
	g := e.recordSearch()
	st := e.pass()
	t.Logf("search: %d runs pages, %d jobs pages", g.runPages, len(g.jobRuns))
	if st.Error != "" || !slices.Equal(e.gh.rerunJobs(), []int64{8}) {
		t.Fatalf("status %+v reruns %v jobs listed for %d runs", st, e.gh.rerunJobs(), len(g.jobRuns))
	}
	if !slices.Equal(g.jobRuns, []int64{70, 80}) || g.runPages != 3 {
		t.Fatalf("search pages %d, jobs listed for %v", g.runPages, g.jobRuns)
	}
}

// The executing job's run was created seven hours before the lost Run,
// beyond the lookback, behind 450 older runs. The search stops listing at
// the first page whose last run is past the lookback instead of exhausting
// its page bound, and the job is not found: no re-run, no error.
func TestLostSearchStopsAtTheLookback(t *testing.T) {
	e := newEnv(t)
	e.gh.perPage = 100
	e.gh.queueIn(widgets, 80, 1080, "queued", "ubuntu-latest")
	e.gh.pickElsewhere(widgets, 1080, 9080, "hosted-80")
	e.completedRuns(2000, 1050)
	e.clock.Add(time.Minute)
	e.completedRuns(1000, 50)
	e.clock.Add(7 * time.Hour)
	e.gh.queueIn(widgets, 70, 7, "queued", lux3...)
	e.pass()
	id := e.runID("7")
	e.gh.queueIn(widgets, 80, 8, "queued", lux3...)
	if err := e.gh.pick(runner("7", 0), 8); err != nil {
		t.Fatal(err)
	}
	e.gh.pickElsewhere(widgets, 7, 9007, "hosted-7")
	e.gh.complete(7, "success")
	e.clock.Add(10 * time.Minute)
	e.lux.end(id, preempt)
	e.gh.complete(8, "cancelled")
	e.gh.complete(1080, "success")
	g := e.recordSearch()
	st := e.pass()
	t.Logf("search: %d runs pages, jobs listed for runs %v", g.runPages, g.jobRuns)
	if st.Error != "" || st.Rerun != 0 || len(e.gh.rerunJobs()) != 0 {
		t.Fatalf("status %+v reruns %v", st, e.gh.rerunJobs())
	}
	if g.runPages != 1 || !slices.Equal(g.jobRuns, []int64{70}) {
		t.Fatalf("search pages %d, jobs listed for %v", g.runPages, g.jobRuns)
	}
}

// Two losses with different, overlapping windows, in different workflow
// runs, are resolved by one search in one pass that lists each candidate
// run once.
func TestLostSearchSharesOneWindowAcrossLosses(t *testing.T) {
	e := newEnv(t)
	e.gh.perPage = 100
	e.gh.queueIn(widgets, 80, 1080, "queued", "ubuntu-latest")
	e.gh.pickElsewhere(widgets, 1080, 9080, "hosted-80")
	e.clock.Add(40 * time.Minute)
	e.gh.queueIn(widgets, 70, 7, "queued", lux3...)
	e.pass()
	id7 := e.runID("7")
	e.gh.queueIn(widgets, 80, 8, "queued", lux3...)
	if err := e.gh.pick(runner("7", 0), 8); err != nil {
		t.Fatal(err)
	}
	e.gh.pickElsewhere(widgets, 7, 9007, "hosted-7")
	e.gh.complete(7, "success")
	e.clock.Add(10 * time.Minute)
	e.completedRuns(1000, 30)
	e.clock.Add(20 * time.Minute)
	e.gh.queueIn(widgets, 85, 1085, "queued", "ubuntu-latest")
	e.gh.pickElsewhere(widgets, 1085, 9085, "hosted-85")
	e.gh.queueIn(widgets, 75, 9, "queued", lux3...)
	e.pass()
	id9 := e.runID("9")
	e.gh.queueIn(widgets, 85, 10, "queued", lux3...)
	if err := e.gh.pick(runner("9", 0), 10); err != nil {
		t.Fatal(err)
	}
	e.gh.pickElsewhere(widgets, 9, 9009, "hosted-9")
	e.gh.complete(9, "success")
	e.clock.Add(5 * time.Minute)
	e.lux.end(id7, preempt)
	e.gh.complete(8, "cancelled")
	e.clock.Add(5 * time.Minute)
	e.lux.end(id9, preempt)
	e.gh.complete(10, "cancelled")
	e.gh.complete(1080, "success")
	e.gh.complete(1085, "success")
	e.clock.Add(5 * time.Minute)
	e.completedRuns(2000, 200)
	g := e.recordSearch()
	st := e.pass()
	t.Logf("search: %d runs pages, %d jobs pages", g.runPages, len(g.jobRuns))
	got := e.gh.rerunJobs()
	slices.Sort(got)
	if st.Error != "" || st.Rerun != 2 || !slices.Equal(got, []int64{8, 10}) {
		t.Fatalf("status %+v reruns %v", st, got)
	}
	// The window runs from job 7's Lux Run's creation to job 9's Run's end:
	// runs 70, 75, 80, 85 and the 30 that completed in between.
	if g.runPages != 1 || len(g.jobRuns) != 34 || slices.ContainsFunc(g.jobRuns, func(id int64) bool { return id >= 2000 }) {
		t.Fatalf("search pages %d, jobs listed for %d runs %v", g.runPages, len(g.jobRuns), g.jobRuns)
	}
	for _, id := range []int64{70, 75, 80, 85} {
		if n := countOf(g.jobRuns, id); n != 1 {
			t.Fatalf("run %d job-listed %d times: %v", id, n, g.jobRuns)
		}
	}
}

// 198 runs that completed during the lost Run need a job page each; with
// the Run's own trigger run and the executing job's run that is the whole
// job-page budget. Run listing pages are budgeted separately, so the search
// still reaches the job.
func TestLostSearchRunPagesDoNotSpendTheJobBudget(t *testing.T) {
	e := newEnv(t)
	e.gh.perPage = 100
	e.gh.queueIn(widgets, 80, 1080, "queued", "ubuntu-latest")
	e.gh.pickElsewhere(widgets, 1080, 9080, "hosted-80")
	e.clock.Add(40 * time.Minute)
	e.gh.queueIn(widgets, 70, 7, "queued", lux3...)
	e.pass()
	id := e.runID("7")
	e.gh.queueIn(widgets, 80, 8, "queued", lux3...)
	if err := e.gh.pick(runner("7", 0), 8); err != nil {
		t.Fatal(err)
	}
	e.gh.pickElsewhere(widgets, 7, 9007, "hosted-7")
	e.gh.complete(7, "success")
	e.clock.Add(5 * time.Minute)
	e.completedRuns(1000, maxSearchJobPages-2)
	e.clock.Add(5 * time.Minute)
	e.lux.end(id, preempt)
	e.gh.complete(8, "cancelled")
	e.gh.complete(1080, "success")
	g := e.recordSearch()
	st := e.pass()
	t.Logf("search: %d runs pages, %d jobs pages", g.runPages, len(g.jobRuns))
	if st.Error != "" || !slices.Equal(e.gh.rerunJobs(), []int64{8}) {
		t.Fatalf("status %+v reruns %v", st, e.gh.rerunJobs())
	}
	if g.runPages != 2 || len(g.jobRuns) != maxSearchJobPages {
		t.Fatalf("search pages %d, jobs pages %d", g.runPages, len(g.jobRuns))
	}
}

// pastLoss records a host loss at the clock's time, which must not be after
// at: a Lux Run for job trigger of workflow run triggerRun is created at at,
// its runner takes job actual of workflow run actualRun instead, and it is
// preempted after life. The trigger job runs on a GitHub-hosted runner; the
// actual job ends cancelled, and so does actualRun. No pass runs, as when
// the bridge was down.
func (e *env) pastLoss(at time.Time, triggerRun, trigger, actualRun, actual int64, life time.Duration) {
	e.t.Helper()
	e.clock.Add(at.Sub(e.clock.Now()))
	name := runner(itoa(trigger), 0)
	id := e.lux.seed("api.github.com/t1/1001/"+itoa(trigger)+"/0", luxclient.RunSpec{Labels: map[string]string{
		LabelManaged: "t1", LabelRepo: "1001", LabelRoute: "default", LabelJob: itoa(trigger), LabelGen: "0", LabelRunner: name,
		LabelRunID: itoa(triggerRun), LabelRunAttempt: "1"}})
	e.clock.Add(life)
	e.lux.end(id, preempt)
	e.gh.queueIn(widgets, triggerRun, trigger, "queued", lux3...)
	e.gh.pickElsewhere(widgets, trigger, 9_000_000+trigger, "hosted-"+itoa(trigger))
	e.gh.complete(trigger, "success")
	e.gh.queueIn(widgets, actualRun, actual, "queued", lux3...)
	e.gh.pickElsewhere(widgets, actual, 8_000_000+actual, name)
	e.gh.complete(actual, "cancelled")
	e.gh.setRunTimes(widgets, triggerRun, at.Add(-time.Minute), at.Add(2*time.Minute))
	e.gh.setRunTimes(widgets, actualRun, at.Add(-5*time.Minute), at.Add(life))
}

// pastRuns adds n workflow runs from first, created every apart from from,
// each with one job that ran for life and succeeded on a GitHub-hosted
// runner.
func (e *env) pastRuns(first int64, n int, from time.Time, every, life time.Duration) {
	for i := range int64(n) {
		e.gh.queueIn(widgets, first+i, 1_000_000+first+i, "completed", "ubuntu-latest")
		e.gh.editJob(1_000_000+first+i, func(j *fakeJob) { j.conclusion = "success" })
		created := from.Add(time.Duration(i) * every)
		e.gh.setRunTimes(widgets, first+i, created, created.Add(life))
	}
}

// Two losses five hours apart, rediscovered together after a restart, with
// 300 runs created and completed between them: those runs overlap neither
// loss's Run, so none of them is job-listed, and the job budget is left for
// the runs that can hold the jobs.
func TestLostSearchSkipsRunsBetweenDisjointLosses(t *testing.T) {
	e := newEnv(t)
	e.gh.perPage = 100
	t0 := e.clock.Now().Add(time.Hour)
	e.pastLoss(t0, 70, 7, 80, 8, 20*time.Minute)
	e.pastRuns(100_000, 300, t0.Add(30*time.Minute), 48*time.Second, 10*time.Minute)
	e.pastLoss(t0.Add(5*time.Hour), 75, 9, 85, 10, 20*time.Minute)
	e.clock.Add(5 * time.Minute)
	g := e.recordSearch()
	ops := countOps(e, nil)
	st := e.pass()
	t.Logf("search: %d runs pages, jobs listed for runs %v; all GitHub calls %v", g.runPages, g.jobRuns, ops)
	got := e.gh.rerunJobs()
	slices.Sort(got)
	if st.Error != "" || st.Rerun != 2 || !slices.Equal(got, []int64{8, 10}) {
		t.Fatalf("status %+v reruns %v", st, got)
	}
	if !slices.Equal(g.jobRuns, []int64{75, 85, 70, 80}) {
		t.Fatalf("jobs listed for %v", g.jobRuns)
	}
}

// A restart rediscovers 20 losses spread over 20 hours of a repository
// creating 50 workflow runs an hour, each running 15 minutes: about 1,040
// runs, past the 1,000-run listing cap. One search cannot list the jobs of
// every loss's candidates within its job budget, so each pass resolves the
// newest losses it can and retries the rest; a resolved loss leaves the
// window, so later listings end earlier and reach the oldest. Every loss is
// re-run within maxRerunTries passes and none is given up on.
func TestLostSearchRecoversLossesSpreadOverARestart(t *testing.T) {
	e := newEnv(t)
	e.gh.perPage = 100
	t0 := e.clock.Now().Add(30 * time.Minute)
	e.pastRuns(100_000, 1000, t0.Add(-15*time.Minute), 72*time.Second, 15*time.Minute)
	var want []int64
	for i := range int64(20) {
		at := t0.Add(time.Duration(i) * time.Hour)
		e.pastLoss(at, 200+i, 30_000+i, 400+i, 50_000+i, 20*time.Minute)
		want = append(want, 50_000+i)
	}
	e.clock.Add(5 * time.Minute)
	g := e.recordSearch()
	ops := countOps(e, nil)
	passes := 0
	for passes < maxRerunTries && len(e.gh.rerunJobs()) < len(want) {
		e.pass()
		passes++
		t.Logf("pass %d: re-ran %d jobs, %d search runs pages, %d jobs pages so far", passes, len(e.gh.rerunJobs()), g.runPages, len(g.jobRuns))
		e.clock.Add(time.Minute)
	}
	t.Logf("all GitHub calls %v", ops)
	got := e.gh.rerunJobs()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("after %d passes re-ran %v, want %v", passes, got, want)
	}
	if len(e.rec.lost) != 0 {
		t.Fatalf("losses left: %d", len(e.rec.lost))
	}
}

// oldReRun queues jobs in workflow run 60 at the clock's time, which fail on
// GitHub-hosted runners, waits age and re-runs them by hand: GitHub keeps
// run 60's created_at and moves its run_started_at to now. It returns the
// re-run jobs' ids.
func (e *env) oldReRun(age time.Duration, jobs ...int64) []int64 {
	for _, j := range jobs {
		e.gh.queueIn(widgets, 60, j, "queued", lux3...)
		e.gh.pickElsewhere(widgets, j, 9000+j, "hosted-"+itoa(j))
		e.gh.complete(j, "failure")
	}
	e.clock.Add(age)
	return e.gh.rerunByHand(60)
}

// Workflow run 60 was created eight hours before the loss and re-run 12
// minutes before it. A Lux Run provisioned for job 7 of a new workflow run
// 70 has its runner take run 60's attempt-2 job; the host is lost. Run 60's
// created_at is past the lookback, but its run_started_at is within the
// Run's window, so the search lists its jobs and re-runs the job once.
func TestLostSearchFindsTheJobOfAReRunOfAnOldWorkflowRun(t *testing.T) {
	e := newEnv(t)
	e.gh.perPage = 100
	j2 := e.oldReRun(8*time.Hour-12*time.Minute, 61)[0]
	e.clock.Add(2 * time.Minute)
	e.gh.queueIn(widgets, 70, 7, "queued", lux3...)
	e.pass()
	id := e.runID("7")
	if err := e.gh.pick(runner("7", 0), j2); err != nil {
		t.Fatal(err)
	}
	e.gh.pickElsewhere(widgets, 7, 9007, "hosted-7")
	e.gh.complete(7, "success")
	e.clock.Add(8 * time.Minute)
	e.lux.end(id, preempt)
	e.gh.complete(j2, "cancelled")
	e.clock.Add(2 * time.Minute)
	g := e.recordSearch()
	ops := countOps(e, nil)
	st := e.pass()
	t.Logf("search: %d runs pages, jobs listed for runs %v; all GitHub calls %v", g.runPages, g.jobRuns, ops)
	if st.Error != "" || st.Rerun != 1 || !slices.Equal(e.gh.rerunJobs(), []int64{j2}) {
		t.Fatalf("status %+v reruns %v", st, e.gh.rerunJobs())
	}
	// Run 60 by the search, after run 70, then by the sibling guard.
	if g.runPages != 1 || !slices.Equal(g.jobRuns, []int64{70, 60, 60}) {
		t.Fatalf("search pages %d, jobs listed for %v", g.runPages, g.jobRuns)
	}
	if j := e.gh.job(e.gh.rerunOf(j2)); j == nil || j.runID != 60 || j.attempt != 3 {
		t.Fatalf("new attempt %+v", j)
	}
	for range 3 {
		e.clock.Add(time.Minute)
		e.pass()
	}
	if !slices.Equal(e.gh.rerunJobs(), []int64{j2}) {
		t.Fatalf("reruns %v", e.gh.rerunJobs())
	}
}

// labelReRunLoss creates workflow run 60 with two jobs that fail, then
// 250 completed runs an hour later, then recent completed runs created
// three hours after run 60; eight hours after run 60 it re-runs both jobs
// by hand. A Lux Run is provisioned for the first re-run job (the label
// job); its runner takes the second (the sibling), and its host is lost.
// It returns the sibling's id.
func (e *env) labelReRunLoss(recent int) int64 {
	e.t.Helper()
	e.gh.perPage = 100
	for _, j := range []int64{61, 62} {
		e.gh.queueIn(widgets, 60, j, "queued", lux3...)
		e.gh.pickElsewhere(widgets, j, 9000+j, "hosted-"+itoa(j))
		e.gh.complete(j, "failure")
	}
	e.clock.Add(time.Hour)
	e.completedRuns(1000, 250)
	e.clock.Add(2 * time.Hour)
	e.completedRuns(2000, recent)
	e.clock.Add(5*time.Hour - 12*time.Minute)
	jobs := e.gh.rerunByHand(60)
	label, sibling := jobs[0], jobs[1]
	e.clock.Add(2 * time.Minute)
	e.pass()
	id := e.runID(itoa(label))
	if err := e.gh.pick(runner(itoa(label), 0), sibling); err != nil {
		e.t.Fatal(err)
	}
	e.gh.pickElsewhere(widgets, label, 9000+label, "hosted-label")
	e.gh.complete(label, "success")
	e.clock.Add(8 * time.Minute)
	e.lux.end(id, preempt)
	e.gh.complete(sibling, "cancelled")
	e.clock.Add(2 * time.Minute)
	return sibling
}

// The lost Run was provisioned for a job of the re-run of workflow run 60,
// created eight hours earlier, and its runner took that job's sibling in
// the same attempt. 250 runs created seven hours before the loss end the
// listing at its first page, before it reaches run 60; the search then
// lists the label job's own workflow run and finds the sibling.
func TestLostSearchListsTheLabelWorkflowRunOfAnOldReRun(t *testing.T) {
	e := newEnv(t)
	sibling := e.labelReRunLoss(0)
	g := e.recordSearch()
	ops := countOps(e, nil)
	st := e.pass()
	t.Logf("search: %d runs pages, jobs listed for runs %v; all GitHub calls %v", g.runPages, g.jobRuns, ops)
	if st.Error != "" || st.Rerun != 1 || !slices.Equal(e.gh.rerunJobs(), []int64{sibling}) {
		t.Fatalf("status %+v reruns %v", st, e.gh.rerunJobs())
	}
	// The search lists run 60 once; the sibling guard lists it again, as
	// the label job's Run is live.
	if g.runPages != 1 || !slices.Equal(g.jobRuns, []int64{60, 60}) {
		t.Fatalf("search pages %d, jobs listed for %v", g.runPages, g.jobRuns)
	}
	for range 3 {
		e.clock.Add(time.Minute)
		e.pass()
	}
	if !slices.Equal(e.gh.rerunJobs(), []int64{sibling}) {
		t.Fatalf("reruns %v", e.gh.rerunJobs())
	}
}

// As above, with 920 runs inside the lookback that completed before the
// lost Run: the listing's 10th and last allowed page holds the last 20 of
// them and 80 runs past the lookback. Its last run being past the lookback
// ends the listing within its page bound, so the search is complete and
// goes on to the label job's workflow run.
func TestLostSearchEndsTheListingOnAPageThatCrossesTheLookback(t *testing.T) {
	e := newEnv(t)
	sibling := e.labelReRunLoss(920)
	g := e.recordSearch()
	st := e.pass()
	t.Logf("search: %d runs pages, jobs listed for runs %v", g.runPages, g.jobRuns)
	if st.Error != "" || st.Rerun != 1 || !slices.Equal(e.gh.rerunJobs(), []int64{sibling}) {
		t.Fatalf("status %+v reruns %v", st, e.gh.rerunJobs())
	}
	if g.runPages != maxSearchRunPages {
		t.Fatalf("search pages %d, want %d", g.runPages, maxSearchRunPages)
	}
}

// Lux reports the lost Run with no createdAt, so its window starts the
// longest route timeout (2h) before its end. 250 runs that completed three
// hours before that start sit between the window and the executing job's
// run; they are skipped without a job page, and the job is re-run in the
// first pass that sees the loss.
func TestLostSearchWindowsARunWithoutCreatedAtByTheRouteTimeout(t *testing.T) {
	e := newEnv(t)
	e.gh.perPage = 100
	e.gh.queueIn(widgets, 80, 1080, "queued", "ubuntu-latest")
	e.gh.pickElsewhere(widgets, 1080, 9080, "hosted-80")
	e.clock.Add(10 * time.Minute)
	e.completedRuns(1000, 250)
	e.clock.Add(3 * time.Hour)
	e.gh.queueIn(widgets, 70, 7, "queued", lux3...)
	e.pass()
	id := e.runID("7")
	e.gh.queueIn(widgets, 80, 8, "queued", lux3...)
	if err := e.gh.pick(runner("7", 0), 8); err != nil {
		t.Fatal(err)
	}
	e.gh.pickElsewhere(widgets, 7, 9007, "hosted-7")
	e.gh.complete(7, "success")
	e.clock.Add(10 * time.Minute)
	e.lux.end(id, preempt)
	e.gh.complete(8, "cancelled")
	e.gh.complete(1080, "success")
	e.lux.getNoCreated = true
	g := e.recordSearch()
	st := e.pass()
	t.Logf("search: %d runs pages, jobs listed for runs %v", g.runPages, g.jobRuns)
	if !slices.Contains(e.lux.getCalls(), id) {
		t.Fatalf("the loss was not read with Get: %v", e.lux.getCalls())
	}
	if st.Error != "" || !slices.Equal(e.gh.rerunJobs(), []int64{8}) {
		t.Fatalf("status %+v reruns %v", st, e.gh.rerunJobs())
	}
	if !slices.Equal(g.jobRuns, []int64{70, 80}) {
		t.Fatalf("jobs listed for %v", g.jobRuns)
	}
}

// Loss A's runner took job 8, which GitHub still shows in_progress after
// the host loss: a pass finds it and waits. Loss B follows within the stuck
// job grace. B's search has only B's window; A's identity is known, so the
// 30 runs that completed during A's Run before B's started cost no job page.
func TestLostSearchLeavesOutALossWhoseJobIsFoundButRunning(t *testing.T) {
	e := newEnv(t)
	e.gh.perPage = 100
	e.gh.queueIn(widgets, 70, 7, "queued", lux3...)
	e.pass()
	idA := e.runID("7")
	e.gh.queueIn(widgets, 80, 8, "queued", lux3...)
	if err := e.gh.pick(runner("7", 0), 8); err != nil {
		t.Fatal(err)
	}
	e.gh.pickElsewhere(widgets, 7, 9007, "hosted-7")
	e.gh.complete(7, "success")
	e.clock.Add(10 * time.Minute)
	e.completedRuns(1000, 30)
	e.clock.Add(20 * time.Minute)
	e.gh.queueIn(widgets, 75, 9, "queued", lux3...)
	e.pass()
	idB := e.runID("9")
	e.gh.queueIn(widgets, 85, 10, "queued", lux3...)
	if err := e.gh.pick(runner("9", 0), 10); err != nil {
		t.Fatal(err)
	}
	e.gh.pickElsewhere(widgets, 9, 9009, "hosted-9")
	e.gh.complete(9, "success")
	// closeStopped reads A's runner as live, so A is queued unverified from
	// the ended Run and resolved by a search.
	e.lux.runnerListState = "running"
	e.clock.Add(5 * time.Minute)
	e.lux.end(idA, preempt)
	e.clock.Add(30 * time.Second)
	if st := e.pass(); st.Error != "" || st.Rerun != 0 {
		t.Fatalf("loss A pass: %+v", st)
	}
	e.clock.Add(30 * time.Second)
	e.lux.end(idB, preempt)
	e.gh.complete(10, "cancelled")
	e.clock.Add(30 * time.Second)
	g := e.recordSearch()
	if l := e.rec.lost["1001/"+runner("7", 0)]; l == nil || l.verified || l.jobID != 8 {
		t.Fatalf("loss A %+v, want unverified with job 8", l)
	}
	st := e.pass()
	t.Logf("search: %d runs pages, jobs listed for runs %v", g.runPages, g.jobRuns)
	if st.Error != "" || !slices.Equal(e.gh.rerunJobs(), []int64{10}) {
		t.Fatalf("status %+v reruns %v", st, e.gh.rerunJobs())
	}
	// Run 80 is listed by the pass's in_progress job listing, not the
	// search, which stops at run 85.
	if !slices.Equal(g.jobRuns, []int64{80, 85}) {
		t.Fatalf("jobs listed for %v", g.jobRuns)
	}
	if j := e.gh.job(8); j.status != "in_progress" {
		t.Fatalf("job 8 %+v, want in_progress", j)
	}
}

func countOf(ids []int64, id int64) int {
	n := 0
	for _, x := range ids {
		if x == id {
			n++
		}
	}
	return n
}

// As SkipsRunsBetweenDisjointLosses, but the newer Run's runner took no job,
// so its window stays open for the whole scan; the gap runs still cost no
// job page.
func TestLostSearchSkipsRunsBetweenDisjointUnresolvedLosses(t *testing.T) {
	e := newEnv(t)
	e.gh.perPage = 100
	t0 := e.clock.Now().Add(time.Hour)
	e.pastLoss(t0, 70, 7, 80, 8, 20*time.Minute)
	e.pastRuns(100_000, 300, t0.Add(30*time.Minute), 48*time.Second, 10*time.Minute)
	at := t0.Add(5 * time.Hour)
	e.clock.Add(at.Sub(e.clock.Now()))
	id := e.lux.seed("api.github.com/t1/1001/9/0", luxclient.RunSpec{Labels: map[string]string{
		LabelManaged: "t1", LabelRepo: "1001", LabelRoute: "default", LabelJob: "9", LabelGen: "0",
		LabelRunner: runner("9", 0), LabelRunID: "75", LabelRunAttempt: "1"}})
	e.clock.Add(20 * time.Minute)
	e.lux.end(id, preempt)
	e.gh.queueIn(widgets, 75, 9, "queued", lux3...)
	e.gh.pickElsewhere(widgets, 9, 9_000_009, "hosted-9")
	e.gh.complete(9, "success")
	e.gh.setRunTimes(widgets, 75, at.Add(-time.Minute), at.Add(2*time.Minute))
	e.clock.Add(5 * time.Minute)
	g := e.recordSearch()
	st := e.pass()
	if st.Error != "" || !slices.Equal(e.gh.rerunJobs(), []int64{8}) {
		t.Fatalf("status %+v reruns %v", st, e.gh.rerunJobs())
	}
	if !slices.Equal(g.jobRuns, []int64{75, 70, 80}) {
		t.Fatalf("jobs listed for %v", g.jobRuns)
	}
}

// A resolved loss whose job then reads as gone leaves the search with no
// target: it lists nothing rather than every workflow run unfiltered.
func TestLostSearchWithNoTargetListsNothing(t *testing.T) {
	e := newEnv(t)
	e.gh.perPage = 100
	e.gh.queueIn(widgets, 70, 7, "queued", lux3...)
	e.pass()
	idA := e.runID("7")
	e.gh.queueIn(widgets, 80, 8, "queued", lux3...)
	if err := e.gh.pick(runner("7", 0), 8); err != nil {
		t.Fatal(err)
	}
	e.gh.pickElsewhere(widgets, 7, 9007, "hosted-7")
	e.gh.complete(7, "success")
	e.lux.runnerListState = "running"
	e.clock.Add(5 * time.Minute)
	e.lux.end(idA, preempt)
	e.clock.Add(30 * time.Second)
	if st := e.pass(); st.Error != "" || st.Rerun != 0 {
		t.Fatalf("loss A pass: %+v", st)
	}
	if l := e.rec.lost["1001/"+runner("7", 0)]; l == nil || l.verified || l.jobID != 8 {
		t.Fatalf("loss A %+v, want resolved, unverified, job 8", l)
	}
	e.completedRuns(100_000, 1100)
	e.gh.fail = func(op, _ string, _ int) error {
		if op == opGetJob {
			return &githubapp.APIError{StatusCode: http.StatusNotFound}
		}
		return nil
	}
	e.clock.Add(30 * time.Second)
	g := e.recordSearch()
	st := e.pass()
	if st.Error != "" || g.runPages != 0 || len(e.gh.rerunJobs()) != 0 {
		t.Fatalf("status %+v, %d search run pages, reruns %v", st, g.runPages, e.gh.rerunJobs())
	}
}

// When the run listing hits its page cap, each loss's own label workflow
// run is still listed, so a runner that took a sibling there is found. The
// label run is a re-run of a workflow run created 8 h earlier, out of the
// listing's reach, and 1,100 runs that finished before the loss fill the
// listing past its cap.
func TestLostSearchListsTheLabelRunWhenTheListingIsTruncated(t *testing.T) {
	e := newEnv(t)
	e.gh.perPage = 100
	at := e.clock.Now().Add(6 * time.Hour)
	e.pastRuns(100_000, 1100, at.Add(-5*time.Hour), time.Second, time.Second)
	e.pastLoss(at, 70, 7, 70, 8, 20*time.Minute)
	e.gh.setRunTimes(widgets, 70, at.Add(-8*time.Hour), at.Add(20*time.Minute))
	e.clock.Add(5 * time.Minute)
	g := e.recordSearch()
	st := e.pass()
	t.Logf("search: %d runs pages, jobs listed for runs %v", g.runPages, g.jobRuns)
	if g.runPages != maxSearchRunPages {
		t.Fatalf("listing not truncated: %d run pages", g.runPages)
	}
	// Found in the pass whose listing was truncated, not a later one.
	if st.Rerun != 1 || !slices.Equal(e.gh.rerunJobs(), []int64{8}) {
		t.Fatalf("status %+v reruns %v, want [8] in this pass", st, e.gh.rerunJobs())
	}
}

// A failed run listing ends the search: the label workflow run is not
// listed after it, and a rate limit is reported as such.
func TestLostSearchEndsOnAListingError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"server error", &githubapp.APIError{StatusCode: http.StatusInternalServerError}},
		{"rate limited", &githubapp.APIError{StatusCode: http.StatusTooManyRequests, RateLimited: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.gh.perPage = 100
			at := e.clock.Now().Add(6 * time.Hour)
			e.pastRuns(100_000, 1100, at.Add(-5*time.Hour), time.Second, time.Second)
			e.pastLoss(at, 70, 7, 70, 8, 20*time.Minute)
			e.gh.setRunTimes(widgets, 70, at.Add(-8*time.Hour), at.Add(20*time.Minute))
			e.clock.Add(5 * time.Minute)
			if rl, ok := tc.err.(*githubapp.APIError); ok && rl.RateLimited {
				rl.RetryAt = e.clock.Now().Add(time.Minute)
			}
			e.gh.fail = func(op, _ string, page int) error {
				if op == opRuns && page == 2 {
					return tc.err
				}
				return nil
			}
			g := e.recordSearch()
			st := e.pass()
			if slices.Contains(g.jobRuns, 70) || st.Rerun != 0 || len(e.gh.rerunJobs()) != 0 {
				t.Fatalf("search went on after the listing failed: jobs listed for %v, status %+v", g.jobRuns, st)
			}
			if rl := tc.err.(*githubapp.APIError); rl.RateLimited && (st.Error != ErrRateLimited || st.RateLimitedUntil == nil) {
				t.Fatalf("rate limit not reported: %+v", st)
			}
		})
	}
}
