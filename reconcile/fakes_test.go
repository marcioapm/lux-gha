package reconcile

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
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
	runnerID   int64
	name       string // defaults to build-<id>
	workflow   string // workflow_name; empty sends null
	attempt    int64
}

// fakeRepo is one repository's Actions state.
type fakeRepo struct {
	id      int64
	jobs    map[int64]*fakeJob
	runners map[int64]*githubapp.Runner
	// runs holds what the runs listing reports of a workflow run besides its
	// id and status (name, branch, number, pull requests).
	runs map[int64]githubapp.WorkflowRun
	// unlisted runners exist but are missing from unfiltered listings, as a
	// lagging listing would leave them.
	unlisted map[string]bool
	// archived, disabled and uninstalled shape GET /installation/repositories;
	// an uninstalled repository answers 404 to the installation token, as
	// GitHub does once it leaves the installation.
	archived, disabled, uninstalled bool
	checkRuns                       []githubapp.CheckRun
	forceCancels                    []int64
}

// Fake GitHub operations, for fail.
const (
	opRuns        = "runs"
	opJobs        = "jobs"
	opRunners     = "runners"
	opJIT         = "jit"
	opDelete      = "delete"
	opInstall     = "installation"
	opListChecks  = "list-checks"
	opCreateCheck = "create-check"
	opCancel      = "force-cancel"
	opGetRun      = "get-run"
	opRerun       = "rerun"
)

// fakeGitHub models one installation's repositories: workflow jobs with
// labels and status, and self-hosted runners whose busy/online state the
// test drives. Workflow runs are derived from their jobs.
type fakeGitHub struct {
	mu        sync.Mutex
	clock     *fakeClock
	repos     map[string]*fakeRepo // by owner/name
	nextID    int64
	jit       []githubapp.JITConfigRequest
	jitSecret map[string]string // runner name -> issued JIT config
	deleted   []string
	calls     int
	// installCalls counts GET /installation/repositories pages; reads counts
	// calls per repository.
	installCalls int
	reads        map[string]int
	// rateLimitUntil answers every call 429 until this time.
	rateLimitUntil time.Time
	// fail, when set, may fail an operation on a repository and page (0 for
	// unpaged operations) after the rate-limit gate.
	fail func(op, repo string, page int) error
	// perPage bounds page sizes to exercise paging.
	perPage int
	// endlessListing makes installation repository pages claim a next one
	// up to page 150, past the reconciler's bound.
	endlessListing bool
	// listTwice names a repository the installation listing returns twice,
	// as offset paging does when the set changes between pages.
	listTwice string
	// beforeDelete runs once, under the lock, before the next runner deletion.
	beforeDelete func()
	checkOutputs []githubapp.CheckRunRequest
	reruns       []int64 // job ids re-run
}

const widgets = "acme/widgets"

func newFakeGitHub(clock *fakeClock) *fakeGitHub {
	f := &fakeGitHub{clock: clock, repos: map[string]*fakeRepo{}, nextID: 500, jitSecret: map[string]string{}, perPage: 2, reads: map[string]int{}}
	f.addRepo(widgets, 1001)
	return f
}

func (f *fakeGitHub) addRepo(full string, id int64) *fakeRepo {
	r := &fakeRepo{id: id, jobs: map[int64]*fakeJob{}, runners: map[int64]*githubapp.Runner{}, runs: map[int64]githubapp.WorkflowRun{}, unlisted: map[string]bool{}}
	f.repos[full] = r
	return r
}

func (f *fakeGitHub) gate(op string, repo githubapp.Repository, page int) (*fakeRepo, error) {
	f.calls++
	full := repo.Owner + "/" + repo.Name
	f.reads[full]++
	if f.clock.Now().Before(f.rateLimitUntil) {
		return nil, &githubapp.APIError{StatusCode: 429, RateLimited: true, RetryAt: f.rateLimitUntil}
	}
	if f.fail != nil {
		if err := f.fail(op, full, page); err != nil {
			return nil, err
		}
	}
	r, ok := f.repos[full]
	if !ok || r.uninstalled {
		return nil, &githubapp.APIError{StatusCode: 404}
	}
	return r, nil
}

func (f *fakeGitHub) GetRepository(_ context.Context, repo githubapp.Repository) (githubapp.RepositoryInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.gate("repo", repo, 0)
	if err != nil {
		return githubapp.RepositoryInfo{}, err
	}
	return githubapp.RepositoryInfo{ID: r.id, FullName: repo.Owner + "/" + repo.Name}, nil
}

func (r *fakeRepo) runStatus(runID int64) string {
	var queued, active, done int
	for _, j := range r.jobs {
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
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.gate(opRuns, repo, opts.Page)
	if err != nil {
		return githubapp.WorkflowRunPage{}, err
	}
	var ids []int64
	for _, j := range r.jobs {
		if !slices.Contains(ids, j.runID) && r.runStatus(j.runID) == opts.Status {
			ids = append(ids, j.runID)
		}
	}
	slices.Sort(ids)
	var runs []githubapp.WorkflowRun
	for _, id := range ids {
		s := opts.Status
		run := r.runs[id]
		run.ID, run.Status, run.HeadSHA = id, &s, fakeSHA(id)
		runs = append(runs, run)
	}
	items, next := page(runs, opts.PageOptions, f.perPage)
	return githubapp.WorkflowRunPage{TotalCount: len(runs), Runs: items, HasNext: next}, nil
}

func (f *fakeGitHub) ListWorkflowRunJobs(_ context.Context, repo githubapp.Repository, runID int64, opts githubapp.WorkflowJobListOptions) (githubapp.WorkflowJobPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.gate(opJobs, repo, opts.Page)
	if err != nil {
		return githubapp.WorkflowJobPage{}, err
	}
	if opts.Filter != "latest" {
		return githubapp.WorkflowJobPage{}, errors.New("fake: reconciler must ask for the latest attempt")
	}
	var jobs []githubapp.WorkflowJob
	for _, id := range sortedKeys(r.jobs) {
		j := r.jobs[id]
		if j.runID != runID {
			continue
		}
		out := githubapp.WorkflowJob{ID: j.id, RunID: j.runID, Status: j.status, Labels: j.labels,
			Name: cmp.Or(j.name, fmt.Sprintf("build-%d", j.id)), HeadSHA: fakeSHA(j.runID), RunAttempt: attemptOf(j)}
		if j.workflow != "" {
			wf := j.workflow
			out.WorkflowName = &wf
		}
		if j.conclusion != "" {
			c := j.conclusion
			out.Conclusion = &c
		}
		// Assignment stays on the job after the JIT runner deregisters.
		if j.runner != "" {
			name, rid := j.runner, j.runnerID
			out.RunnerName, out.RunnerID = &name, &rid
		}
		jobs = append(jobs, out)
	}
	items, next := page(jobs, opts.PageOptions, f.perPage)
	return githubapp.WorkflowJobPage{TotalCount: len(jobs), Jobs: items, HasNext: next}, nil
}

func (f *fakeGitHub) ListRepoRunners(_ context.Context, repo githubapp.Repository, opts githubapp.RunnerListOptions) (githubapp.RunnerPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.gate(opRunners, repo, opts.Page)
	if err != nil {
		return githubapp.RunnerPage{}, err
	}
	var all []githubapp.Runner
	for _, id := range sortedKeys(r.runners) {
		rn := *r.runners[id]
		if opts.Name != "" && rn.Name != opts.Name || opts.Name == "" && r.unlisted[rn.Name] {
			continue
		}
		all = append(all, rn)
	}
	items, next := page(all, opts.PageOptions, f.perPage)
	return githubapp.RunnerPage{TotalCount: len(all), Runners: items, HasNext: next}, nil
}

func (f *fakeGitHub) GenerateRepoJITConfig(_ context.Context, repo githubapp.Repository, req githubapp.JITConfigRequest) (githubapp.JITConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.gate(opJIT, repo, 0)
	if err != nil {
		return githubapp.JITConfig{}, err
	}
	for _, rn := range r.runners {
		if rn.Name == req.Name {
			return githubapp.JITConfig{}, &githubapp.APIError{StatusCode: http.StatusConflict}
		}
	}
	f.jit = append(f.jit, req)
	f.nextID++
	rn := &githubapp.Runner{ID: f.nextID, Name: req.Name, Status: "offline", RunnerGroupID: req.RunnerGroupID, Ephemeral: true}
	for _, l := range req.Labels {
		rn.Labels = append(rn.Labels, githubapp.RunnerLabel{Name: l, Type: "custom"})
	}
	// GitHub also reports its default labels for a self-hosted runner.
	for _, l := range []string{"self-hosted", "Linux", "ARM64"} {
		if !slices.ContainsFunc(req.Labels, func(s string) bool { return strings.EqualFold(s, l) }) {
			rn.Labels = append(rn.Labels, githubapp.RunnerLabel{Name: l, Type: "read-only"})
		}
	}
	r.runners[rn.ID] = rn
	secret := fmt.Sprintf("SECRET-JIT-%d-%s", rn.ID, req.Name)
	f.jitSecret[req.Name] = secret
	return githubapp.JITConfig{Runner: *rn, EncodedJITConfig: secret}, nil
}

// ListInstallationRepositories pages the installed repositories in name
// order. fail sees the operation with an empty repository name.
func (f *fakeGitHub) ListInstallationRepositories(_ context.Context, installationID int64, opts githubapp.PageOptions) (githubapp.InstallationRepositoryPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.installCalls++
	if f.clock.Now().Before(f.rateLimitUntil) {
		return githubapp.InstallationRepositoryPage{}, &githubapp.APIError{StatusCode: 429, RateLimited: true, RetryAt: f.rateLimitUntil}
	}
	if f.fail != nil {
		if err := f.fail(opInstall, "", opts.Page); err != nil {
			return githubapp.InstallationRepositoryPage{}, err
		}
	}
	if installationID != 42 || opts.PerPage != 100 {
		return githubapp.InstallationRepositoryPage{}, &githubapp.APIError{StatusCode: 400}
	}
	var all []githubapp.InstallationRepository
	for _, full := range sortedKeys(f.repos) {
		r := f.repos[full]
		if !r.uninstalled {
			all = append(all, githubapp.InstallationRepository{ID: r.id, FullName: full, Archived: r.archived, Disabled: r.disabled})
			if f.listTwice == full {
				all = append(all, all[len(all)-1])
			}
		}
	}
	items, next := page(all, opts, f.perPage)
	return githubapp.InstallationRepositoryPage{TotalCount: len(all), Repositories: items, HasNext: next || f.endlessListing && opts.Page < 150}, nil
}

// DeleteRepoRunner answers 422 for a busy runner, as GitHub does ("Runner
// ... is still running a job"; see the README).
func (f *fakeGitHub) DeleteRepoRunner(_ context.Context, repo githubapp.Repository, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.gate(opDelete, repo, 0)
	if err != nil {
		return err
	}
	if f.beforeDelete != nil {
		f.beforeDelete()
		f.beforeDelete = nil
	}
	rn, ok := r.runners[id]
	if !ok {
		return &githubapp.APIError{StatusCode: 404}
	}
	if rn.Busy {
		return &githubapp.APIError{StatusCode: 422}
	}
	delete(r.runners, id)
	f.deleted = append(f.deleted, rn.Name)
	return nil
}

// Test controls.

// fakeSHA is the head commit of a workflow run: one commit per run.
func fakeSHA(runID int64) string { return fmt.Sprintf("%040x", runID) }

// CreateCheckRun stores a check run on its commit, as GitHub does.
func (f *fakeGitHub) CreateCheckRun(_ context.Context, repo githubapp.Repository, req githubapp.CheckRunRequest) (githubapp.CheckRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.gate(opCreateCheck, repo, 0)
	if err != nil {
		return githubapp.CheckRun{}, err
	}
	if req.Status != "completed" || req.Conclusion == "" || req.HeadSHA == "" {
		return githubapp.CheckRun{}, &githubapp.APIError{StatusCode: 422}
	}
	f.nextID++
	c := req.Conclusion
	run := githubapp.CheckRun{ID: f.nextID, Name: req.Name, HeadSHA: req.HeadSHA, ExternalID: req.ExternalID, Status: req.Status, Conclusion: &c}
	r.checkRuns = append(r.checkRuns, run)
	f.checkOutputs = append(f.checkOutputs, req)
	return run, nil
}

func (f *fakeGitHub) ListCheckRunsForRef(_ context.Context, repo githubapp.Repository, ref string, opts githubapp.CheckRunListOptions) (githubapp.CheckRunPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.gate(opListChecks, repo, opts.Page)
	if err != nil {
		return githubapp.CheckRunPage{}, err
	}
	var all []githubapp.CheckRun
	for _, c := range r.checkRuns {
		if c.HeadSHA == ref && (opts.CheckName == "" || c.Name == opts.CheckName) {
			all = append(all, c)
		}
	}
	items, next := page(all, opts.PageOptions, f.perPage)
	return githubapp.CheckRunPage{TotalCount: len(all), CheckRuns: items, HasNext: next}, nil
}

// ForceCancelWorkflowRun cancels every unfinished job of the run; a run
// with none answers 409, as GitHub does for a completed run.
func (f *fakeGitHub) ForceCancelWorkflowRun(_ context.Context, repo githubapp.Repository, runID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.gate(opCancel, repo, 0)
	if err != nil {
		return err
	}
	if r.runStatus(runID) == "completed" {
		return &githubapp.APIError{StatusCode: http.StatusConflict}
	}
	r.forceCancels = append(r.forceCancels, runID)
	for _, j := range r.jobs {
		if j.runID == runID && j.status != "completed" {
			j.status, j.conclusion = "completed", "cancelled"
		}
	}
	return nil
}

// GetWorkflowRun reports the run's latest attempt (its jobs' highest) and
// the status derived from its jobs.
func (f *fakeGitHub) GetWorkflowRun(_ context.Context, repo githubapp.Repository, runID int64) (githubapp.WorkflowRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.gate(opGetRun, repo, 0)
	if err != nil {
		return githubapp.WorkflowRun{}, err
	}
	run := githubapp.WorkflowRun{ID: runID}
	for _, j := range r.jobs {
		if j.runID == runID {
			run.RunAttempt = max(run.RunAttempt, attemptOf(j))
		}
	}
	if run.RunAttempt == 0 {
		return githubapp.WorkflowRun{}, &githubapp.APIError{StatusCode: http.StatusNotFound}
	}
	s := r.runStatus(runID)
	run.Status = &s
	// The worst conclusion of its jobs, as GitHub concludes a run.
	if s == "completed" {
		c := "success"
		for _, j := range r.jobs {
			if j.runID == runID && (j.conclusion == "failure" || j.conclusion == "cancelled" && c != "failure") {
				c = j.conclusion
			}
		}
		run.Conclusion = &c
	}
	return run, nil
}

// RerunJob starts the run's next attempt with the job queued again; GitHub
// refuses a run that is not completed with 403 "already running".
func (f *fakeGitHub) RerunJob(_ context.Context, repo githubapp.Repository, jobID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.gate(opRerun, repo, 0)
	if err != nil {
		return err
	}
	j := r.jobs[jobID]
	if j == nil {
		return &githubapp.APIError{StatusCode: http.StatusNotFound}
	}
	if r.runStatus(j.runID) != "completed" {
		return &githubapp.APIError{StatusCode: http.StatusForbidden, Already: true}
	}
	f.reruns = append(f.reruns, jobID)
	for _, o := range r.jobs {
		if o.runID == j.runID {
			o.attempt = attemptOf(o) + 1
		}
	}
	j.status, j.conclusion, j.runner, j.runnerID = "queued", "", "", 0
	return nil
}

func attemptOf(j *fakeJob) int64 { return max(j.attempt, 1) }

func (f *fakeGitHub) rerunJobs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reruns)
}

// checks returns acme/widgets' check runs and the requests that made them.
func (f *fakeGitHub) checks() ([]githubapp.CheckRun, []githubapp.CheckRunRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.repos[widgets].checkRuns), slices.Clone(f.checkOutputs)
}

func (f *fakeGitHub) cancels() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.repos[widgets].forceCancels)
}

func (f *fakeGitHub) repo(full string) *fakeRepo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repos[full]
}

// queue adds a job to acme/widgets in a workflow run of its own.
func (f *fakeGitHub) queue(jobID int64, status string, labels ...string) {
	f.queueIn(widgets, jobID*10, jobID, status, labels...)
}

func (f *fakeGitHub) queueIn(repo string, runID, jobID int64, status string, labels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos[repo].jobs[jobID] = &fakeJob{id: jobID, runID: runID, status: status, labels: labels}
}

// addRunner registers a runner directly, as another tool or bridge would.
func (f *fakeGitHub) addRunner(repo string, id int64, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos[repo].runners[id] = &githubapp.Runner{ID: id, Name: name, Status: "online"}
}

func (f *fakeGitHub) find(name string) (*fakeRepo, *githubapp.Runner) {
	for _, r := range f.repos {
		for _, rn := range r.runners {
			if rn.Name == name {
				return r, rn
			}
		}
	}
	return nil, nil
}

func (f *fakeGitHub) runnerByName(name string) *githubapp.Runner {
	_, rn := f.find(name)
	return rn
}

func (f *fakeGitHub) job(jobID int64) *fakeJob {
	for _, r := range f.repos {
		if j := r.jobs[jobID]; j != nil {
			return j
		}
	}
	return nil
}

// editJob changes a job under the lock.
func (f *fakeGitHub) editJob(jobID int64, edit func(*fakeJob)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit(f.job(jobID))
}

// describeRun sets what acme/widgets' runs listing reports of a workflow run.
func (f *fakeGitHub) describeRun(run githubapp.WorkflowRun) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos[widgets].runs[run.ID] = run
}

func (f *fakeGitHub) setOnline(name string, online bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rn := f.runnerByName(name); rn != nil {
		rn.Status = map[bool]string{true: "online", false: "offline"}[online]
	}
}

// pick assigns a queued job to an online idle runner of the same repository
// whose labels cover every label the job requests.
func (f *fakeGitHub) pick(runner string, jobID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, rn := f.find(runner)
	if rn == nil || rn.Busy || rn.Status != "online" || r.jobs[jobID] == nil || r.jobs[jobID].status != "queued" {
		return fmt.Errorf("fake: %s cannot pick job %d", runner, jobID)
	}
	j := r.jobs[jobID]
	for _, want := range j.labels {
		if !slices.ContainsFunc(rn.Labels, func(l githubapp.RunnerLabel) bool { return strings.EqualFold(l.Name, want) }) {
			return fmt.Errorf("fake: %s lacks label %q for job %d", runner, want, jobID)
		}
	}
	rn.Busy, j.status, j.runner, j.runnerID = true, "in_progress", runner, rn.ID
	return nil
}

// pickElsewhere assigns a queued job to a busy runner outside this bridge.
func (f *fakeGitHub) pickElsewhere(repo string, jobID, runnerID int64, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[repo]
	r.runners[runnerID] = &githubapp.Runner{ID: runnerID, Name: name, Status: "online", Busy: true}
	j := r.jobs[jobID]
	j.status, j.runner, j.runnerID = "in_progress", name, runnerID
}

// complete finishes a job; a JIT runner that ran it deregisters.
func (f *fakeGitHub) complete(jobID int64, conclusion string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.job(jobID)
	j.status, j.conclusion = "completed", conclusion
	if r, rn := f.find(j.runner); rn != nil {
		delete(r.runners, rn.ID)
	}
}

func (f *fakeGitHub) runnerNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, r := range f.repos {
		for _, rn := range r.runners {
			names = append(names, rn.Name)
		}
	}
	slices.Sort(names)
	return names
}

func (f *fakeGitHub) jitCount() int  { f.mu.Lock(); defer f.mu.Unlock(); return len(f.jit) }
func (f *fakeGitHub) callCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// takeReads returns the per-repository call counts since the last call.
func (f *fakeGitHub) takeReads() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.reads
	f.reads = map[string]int{}
	return out
}
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
// honours Idempotency-Key per tenant as Lux does: a repeated key returns the
// first Run unchanged, whatever the new spec.
type fakeLux struct {
	mu            sync.Mutex
	clock         *fakeClock
	runs          []*fakeRun
	creates       int
	terminated    []string // by the bridge
	failCreate    int      // fail the next n Creates before they reach state
	failTerminate int      // fail the next n Terminates
	noEcho        bool
	// failCreateCode, when set, makes failCreate's failures a 422 with this
	// code instead of a 503.
	failCreateCode string
	onCreate       func(luxclient.Run)
	// lateCommit, when >= 0, makes the next Create answer a transport error
	// while its Run stays pending; the pending Run commits at the
	// (lateCommit+1)th List after that, or when its key is submitted again.
	lateCommit   int
	pending      *fakeRun
	pendingLists int
	byRunner     int
	historyJobs  []string
	// failRunnerList fails the next n Lists whose selector names a runner;
	// runnerListState, when set, is the state those Lists report every Run
	// in, as a Run that went live after the pass's live listing would be.
	failRunnerList  int
	runnerListState string
	// sizes, when set, are each shaped pool's size names, as luxd resolves
	// resources.size at submit: a pool absent from it is unshaped.
	sizes   map[string][]string
	refused int
}

func newFakeLux(clock *fakeClock) *fakeLux { return &fakeLux{clock: clock, lateCommit: -1} }

func (l *fakeLux) newRun(key string, spec luxclient.RunSpec) *fakeRun {
	l.creates++
	return &fakeRun{key: key, spec: spec, run: luxclient.Run{
		ID: fmt.Sprintf("run_%d", l.creates), Labels: maps.Clone(spec.Labels), State: "submitted", CreatedAt: l.clock.Now(),
		Spec: luxclient.RunSpecEcho{ResumePolicy: luxclient.ResumePolicyNever}}}
}

// commitPending makes the pending Run visible; the caller holds mu and runs
// the returned hook after unlocking.
func (l *fakeLux) commitPending() func() {
	r, hook := l.pending, l.onCreate
	l.runs = append(l.runs, r)
	l.pending = nil
	if hook == nil {
		return func() {}
	}
	return func() { hook(r.run) }
}

func (l *fakeLux) Create(_ context.Context, key string, spec luxclient.RunSpec) (*luxclient.Run, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.pending != nil && l.pending.key == key {
		hook := l.commitPending()
		l.mu.Unlock()
		hook()
		l.mu.Lock()
	}
	for _, r := range l.runs {
		if r.key == key {
			run := r.run
			l.mu.Unlock()
			return &run, nil
		}
	}
	if size := spec.Resources.Size; size != "" && l.sizes != nil {
		ladder, shaped := l.sizes[spec.Pool]
		if !slices.Contains(ladder, size) {
			code := "unknown_size"
			if !shaped {
				code = "size_unsupported"
			}
			l.refused++
			l.mu.Unlock()
			return nil, &luxclient.APIError{Op: "create run", Status: http.StatusUnprocessableEntity, Code: code}
		}
	}
	if l.failCreate > 0 {
		l.failCreate--
		l.mu.Unlock()
		if l.failCreateCode != "" {
			return nil, &luxclient.APIError{Op: "create run", Status: http.StatusUnprocessableEntity, Code: l.failCreateCode}
		}
		return nil, &luxclient.APIError{Op: "create run", Status: 503}
	}
	r := l.newRun(key, spec)
	if l.lateCommit >= 0 {
		l.pending, l.pendingLists, l.lateCommit = r, l.lateCommit, -1
		l.mu.Unlock()
		return nil, errors.New("fake: create run: connection reset")
	}
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
	if l.failTerminate > 0 {
		l.failTerminate--
		return nil, &luxclient.APIError{Op: "terminate run", Status: 503}
	}
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
	if l.pending != nil {
		if l.pendingLists == 0 {
			hook := l.commitPending()
			l.mu.Unlock()
			hook()
			l.mu.Lock()
		} else {
			l.pendingLists--
		}
	}
	var out []luxclient.Run
	if job, ok := labels[LabelJob]; ok && len(states) == 0 {
		l.historyJobs = append(l.historyJobs, job)
	}
	_, byRunner := labels[LabelRunner]
	if byRunner {
		l.byRunner++
		if l.failRunnerList > 0 {
			l.failRunnerList--
			l.mu.Unlock()
			return nil, &luxclient.APIError{Op: "list runs", Status: 503}
		}
	}
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
			run := r.run
			if byRunner && l.runnerListState != "" {
				run.State = l.runnerListState
			}
			out = append(out, run)
		}
	}
	l.mu.Unlock()
	return out, nil
}

// seed stores a Run under key as if another client had submitted it.
func (l *fakeLux) seed(key string, spec luxclient.RunSpec) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.newRun(key, spec)
	r.run.State = "running"
	l.runs = append(l.runs, r)
	return r.run.ID
}

// end makes a Run finish on its own, as a never Run does: terminated.
func (l *fakeLux) end(id, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.runs {
		if r.run.ID == id {
			at := l.clock.Now()
			r.run.State, r.run.StateReason, r.run.FinishedAt = "terminated", reason, &at
		}
	}
}

// endNoFinish is end with no finishedAt reported.
func (l *fakeLux) endNoFinish(id, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.runs {
		if r.run.ID == id {
			r.run.State, r.run.StateReason, r.run.FinishedAt = "terminated", reason, nil
		}
	}
}

// runnerLists counts List calls whose selector names a runner.
func (l *fakeLux) runnerLists() int { l.mu.Lock(); defer l.mu.Unlock(); return l.byRunner }

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

func (l *fakeLux) createCount() int  { l.mu.Lock(); defer l.mu.Unlock(); return l.creates }
func (l *fakeLux) refusedCount() int { l.mu.Lock(); defer l.mu.Unlock(); return l.refused }
