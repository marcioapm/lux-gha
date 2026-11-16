package reconcile

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux-gha/githubapp"
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
// the first page wholly past the lookback instead of exhausting its page
// bound, and the job is not found: no re-run, no error.
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
	if g.runPages != 2 || !slices.Equal(g.jobRuns, []int64{70}) {
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

func countOf(ids []int64, id int64) int {
	n := 0
	for _, x := range ids {
		if x == id {
			n++
		}
	}
	return n
}
