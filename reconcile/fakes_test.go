package reconcile

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/marcioapm/lux-gha/githubapp"
	"github.com/marcioapm/lux-gha/luxclient"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type fakeJob struct {
	id, runID  int64
	status     string // queued, waiting, in_progress, completed
	conclusion string
	labels     []string
	runner     string
}

// fakeGitHub models one installation's repositories: workflow jobs with
// labels and status, and self-hosted runners whose busy/online state the
// test drives. Runs are derived from their jobs.
type fakeGitHub struct {
	mu        sync.Mutex
	clock     *fakeClock
	repoIDs   map[string]int64
	jobs      map[int64]*fakeJob // all in repo "acme/widgets"
	runners   map[int64]*githubapp.Runner
	nextID    int64
	jit       []githubapp.JITConfigRequest
	jitSecret map[string]string // runner name -> issued JIT config
	deleted   []string
	calls     int
	// rateLimitUntil answers every call 429 until this time.
	rateLimitUntil time.Time
	// unlisted runners exist but are missing from unfiltered listings, as a
	// lagging or truncated listing would leave them.
	unlisted map[string]bool
	// block, when set, makes ListWorkflowRuns wait for a receive.
	block chan struct{}
	// perPage bounds page sizes to exercise paging.
	perPage int
	// failRunners answers runner listings 500.
	failRunners bool
}

func newFakeGitHub(clock *fakeClock) *fakeGitHub {
	return &fakeGitHub{clock: clock, repoIDs: map[string]int64{"acme/widgets": 1001}, jobs: map[int64]*fakeJob{},
		runners: map[int64]*githubapp.Runner{}, nextID: 500, jitSecret: map[string]string{}, unlisted: map[string]bool{}, perPage: 2}
}

func (f *fakeGitHub) gate() error {
	f.calls++
	if f.clock.Now().Before(f.rateLimitUntil) {
		return &githubapp.APIError{StatusCode: 429, RateLimited: true, RetryAt: f.rateLimitUntil}
	}
	return nil
}

func (f *fakeGitHub) GetRepository(_ context.Context, repo githubapp.Repository) (githubapp.RepositoryInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.gate(); err != nil {
		return githubapp.RepositoryInfo{}, err
	}
	full := repo.Owner + "/" + repo.Name
	id, ok := f.repoIDs[full]
	if !ok {
		return githubapp.RepositoryInfo{}, &githubapp.APIError{StatusCode: 404}
	}
	return githubapp.RepositoryInfo{ID: id, FullName: full}, nil
}

func (f *fakeGitHub) runStatus(runID int64) string {
	var queued, active, done int
	for _, j := range f.jobs {
		if j.runID != runID {
			continue
		}
		switch j.status {
		case "queued", "waiting":
			queued++
		case "in_progress":
			active++
		default:
			done++
		}
	}
	switch {
	case active > 0 || (queued > 0 && done > 0):
		return "in_progress"
	case queued > 0:
		return "queued"
	}
	return "completed"
}

func page[T any](items []T, opts githubapp.PageOptions, perPage int) ([]T, bool) {
	n := perPage
	if opts.PerPage > 0 && opts.PerPage < n {
		n = opts.PerPage
	}
	p := max(opts.Page, 1)
	lo := min((p-1)*n, len(items))
	hi := min(lo+n, len(items))
	return items[lo:hi], hi < len(items)
}

func (f *fakeGitHub) ListWorkflowRuns(_ context.Context, repo githubapp.Repository, opts githubapp.WorkflowRunListOptions) (githubapp.WorkflowRunPage, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.gate(); err != nil {
		return githubapp.WorkflowRunPage{}, err
	}
	var ids []int64
	for _, j := range f.jobs {
		if !slices.Contains(ids, j.runID) && f.runStatus(j.runID) == opts.Status {
			ids = append(ids, j.runID)
		}
	}
	slices.Sort(ids)
	var runs []githubapp.WorkflowRun
	for _, id := range ids {
		s := opts.Status
		runs = append(runs, githubapp.WorkflowRun{ID: id, Status: &s})
	}
	items, next := page(runs, opts.PageOptions, f.perPage)
	return githubapp.WorkflowRunPage{TotalCount: len(runs), Runs: items, HasNext: next}, nil
}

func (f *fakeGitHub) ListWorkflowRunJobs(_ context.Context, _ githubapp.Repository, runID int64, opts githubapp.WorkflowJobListOptions) (githubapp.WorkflowJobPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.gate(); err != nil {
		return githubapp.WorkflowJobPage{}, err
	}
	if opts.Filter != "latest" {
		return githubapp.WorkflowJobPage{}, errors.New("fake: reconciler must ask for the latest attempt")
	}
	var jobs []githubapp.WorkflowJob
	for _, id := range sortedKeys(f.jobs) {
		j := f.jobs[id]
		if j.runID == runID {
			jobs = append(jobs, githubapp.WorkflowJob{ID: j.id, RunID: j.runID, Status: j.status, Labels: j.labels})
		}
	}
	items, next := page(jobs, opts.PageOptions, f.perPage)
	return githubapp.WorkflowJobPage{TotalCount: len(jobs), Jobs: items, HasNext: next}, nil
}

func (f *fakeGitHub) ListRepoRunners(_ context.Context, _ githubapp.Repository, opts githubapp.RunnerListOptions) (githubapp.RunnerPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.gate(); err != nil {
		return githubapp.RunnerPage{}, err
	}
	if f.failRunners {
		return githubapp.RunnerPage{}, &githubapp.APIError{StatusCode: 500}
	}
	var all []githubapp.Runner
	for _, id := range sortedKeys(f.runners) {
		rn := *f.runners[id]
		if opts.Name != "" && rn.Name != opts.Name || opts.Name == "" && f.unlisted[rn.Name] {
			continue
		}
		all = append(all, rn)
	}
	items, next := page(all, opts.PageOptions, f.perPage)
	return githubapp.RunnerPage{TotalCount: len(all), Runners: items, HasNext: next}, nil
}

func (f *fakeGitHub) GenerateRepoJITConfig(_ context.Context, _ githubapp.Repository, req githubapp.JITConfigRequest) (githubapp.JITConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.gate(); err != nil {
		return githubapp.JITConfig{}, err
	}
	for _, rn := range f.runners {
		if rn.Name == req.Name {
			return githubapp.JITConfig{}, &githubapp.APIError{StatusCode: http.StatusConflict}
		}
	}
	f.jit = append(f.jit, req)
	f.nextID++
	rn := &githubapp.Runner{ID: f.nextID, Name: req.Name, Status: "offline", RunnerGroupID: req.RunnerGroupID, Ephemeral: true}
	for _, l := range req.Labels {
		rn.Labels = append(rn.Labels, githubapp.RunnerLabel{Name: l})
	}
	f.runners[rn.ID] = rn
	secret := fmt.Sprintf("SECRET-JIT-%d-%s", rn.ID, req.Name)
	f.jitSecret[req.Name] = secret
	return githubapp.JITConfig{Runner: *rn, EncodedJITConfig: secret}, nil
}

// DeleteRepoRunner answers 422 for a busy runner, as GitHub does.
func (f *fakeGitHub) DeleteRepoRunner(_ context.Context, _ githubapp.Repository, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.gate(); err != nil {
		return err
	}
	rn, ok := f.runners[id]
	if !ok {
		return &githubapp.APIError{StatusCode: 404}
	}
	if rn.Busy {
		return &githubapp.APIError{StatusCode: 422}
	}
	delete(f.runners, id)
	f.deleted = append(f.deleted, rn.Name)
	return nil
}

// Test controls.

func (f *fakeGitHub) queue(jobID int64, status string, labels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[jobID] = &fakeJob{id: jobID, runID: jobID * 10, status: status, labels: labels}
}

func (f *fakeGitHub) runnerByName(name string) *githubapp.Runner {
	for _, rn := range f.runners {
		if rn.Name == name {
			return rn
		}
	}
	return nil
}

func (f *fakeGitHub) setOnline(name string, online bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rn := f.runnerByName(name); rn != nil {
		rn.Status = map[bool]string{true: "online", false: "offline"}[online]
	}
}

// pick assigns a queued job to an online idle runner.
func (f *fakeGitHub) pick(runner string, jobID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rn, j := f.runnerByName(runner), f.jobs[jobID]
	if rn == nil || rn.Busy || rn.Status != "online" || j == nil || j.status != "queued" {
		return fmt.Errorf("fake: %s cannot pick job %d", runner, jobID)
	}
	rn.Busy, j.status, j.runner = true, "in_progress", runner
	return nil
}

// complete finishes a job; a JIT runner that ran it deregisters.
func (f *fakeGitHub) complete(jobID int64, conclusion string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[jobID]
	j.status, j.conclusion = "completed", conclusion
	if rn := f.runnerByName(j.runner); rn != nil {
		delete(f.runners, rn.ID)
	}
}

func (f *fakeGitHub) runnerNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, rn := range f.runners {
		names = append(names, rn.Name)
	}
	slices.Sort(names)
	return names
}

func (f *fakeGitHub) jitCount() int  { f.mu.Lock(); defer f.mu.Unlock(); return len(f.jit) }
func (f *fakeGitHub) callCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }
func (f *fakeGitHub) deletedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.deleted)
}

type fakeRun struct {
	run  luxclient.Run
	spec luxclient.RunSpec
	key  string
}

// fakeLux keeps Runs with their labels and full submitted spec. Create
// honours Idempotency-Key per tenant as Lux does.
type fakeLux struct {
	mu         sync.Mutex
	clock      *fakeClock
	runs       []*fakeRun
	creates    int
	terminated []string // by the bridge
	failCreate int      // fail the next n Creates before they reach state
	noEcho     bool
	onCreate   func(luxclient.Run)
}

func (l *fakeLux) Create(_ context.Context, key string, spec luxclient.RunSpec) (*luxclient.Run, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	for _, r := range l.runs {
		if r.key == key {
			run := r.run
			l.mu.Unlock()
			return &run, nil
		}
	}
	if l.failCreate > 0 {
		l.failCreate--
		l.mu.Unlock()
		return nil, &luxclient.APIError{Op: "create run", Status: 503}
	}
	l.creates++
	r := &fakeRun{key: key, spec: spec, run: luxclient.Run{
		ID: fmt.Sprintf("run_%d", len(l.runs)+1), Labels: maps.Clone(spec.Labels), State: "submitted", CreatedAt: l.clock.Now(),
		Spec: luxclient.RunSpecEcho{ResumePolicy: luxclient.ResumePolicyNever}}}
	if l.noEcho {
		r.run.Spec.ResumePolicy = ""
	}
	l.runs = append(l.runs, r)
	run, hook := r.run, l.onCreate
	l.mu.Unlock()
	if l.noEcho {
		return &run, luxclient.ErrResumePolicyNotEchoed
	}
	if hook != nil {
		hook(run)
	}
	return &run, nil
}

func (l *fakeLux) Terminate(_ context.Context, id string) (*luxclient.Run, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.runs {
		if r.run.ID == id {
			if slices.Contains(LiveStates, r.run.State) {
				r.run.State = "terminated"
				l.terminated = append(l.terminated, id)
			}
			run := r.run
			return &run, nil
		}
	}
	return nil, &luxclient.APIError{Op: "terminate run", Status: 404}
}

func (l *fakeLux) List(_ context.Context, labels map[string]string, states ...string) ([]luxclient.Run, error) {
	l.mu.Lock()
	var out []luxclient.Run
	for i := len(l.runs) - 1; i >= 0; i-- {
		r := l.runs[i]
		if len(states) > 0 && !slices.Contains(states, r.run.State) {
			continue
		}
		match := true
		for k, v := range labels {
			if r.run.Labels[k] != v {
				match = false
			}
		}
		if match {
			out = append(out, r.run)
		}
	}
	l.mu.Unlock()
	return out, nil
}

// end makes a Run finish on its own, as a never Run does: terminated.
func (l *fakeLux) end(id, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.runs {
		if r.run.ID == id {
			r.run.State, r.run.StateReason = "terminated", reason
		}
	}
}

func (l *fakeLux) setState(id, state string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.runs {
		if r.run.ID == id {
			r.run.State = state
		}
	}
}

func (l *fakeLux) all() []fakeRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]fakeRun, len(l.runs))
	for i, r := range l.runs {
		out[i] = *r
	}
	return out
}

func (l *fakeLux) live() []fakeRun {
	var out []fakeRun
	for _, r := range l.all() {
		if slices.Contains(LiveStates, r.run.State) {
			out = append(out, r)
		}
	}
	return out
}

func (l *fakeLux) liveForJob(job string) []fakeRun {
	var out []fakeRun
	for _, r := range l.live() {
		if r.run.Labels[LabelJob] == job {
			out = append(out, r)
		}
	}
	return out
}

func (l *fakeLux) terminatedIDs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.terminated)
}

func (l *fakeLux) createCount() int { l.mu.Lock(); defer l.mu.Unlock(); return l.creates }
