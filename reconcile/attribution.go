package reconcile

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marcioapm/lux-gha/githubapp"
)

// assignment is a candidate for the job a runner of this instance executes:
// a job a pass or a search listed as in_progress on it, or a signed
// in_progress delivery naming it. The registry listener confirms it with a
// job read before use.
type assignment struct {
	repository string // owner/name
	jobID      int64
	// fromPass: a pass's listing put it there, and the next pass's listing
	// replaces it. Others (deliveries, searches) are kept until a pass
	// contradicts them, the runner's Run ends, or assignmentTTL passes.
	fromPass bool
	at       time.Time
}

const (
	// assignmentTTL bounds how long a candidate is offered. A JIT runner's
	// job is confirmed within its first seconds, so an older one is stale.
	assignmentTTL = 10 * time.Minute
	// completedTTL bounds how long a completed delivery is remembered; the
	// registry listener keeps its own refusal for the runner after reading it.
	completedTTL = 2 * time.Hour
	// searchGap spaces a repository's fallback searches.
	searchGap = 5 * time.Second
	// searchPageBudget bounds the GitHub pages one search reads, runs and
	// jobs listings together; the runs listing is newest first.
	searchPageBudget = 2 * maxRunPages
)

// attribution publishes, for the registry's runner listener, what passes,
// searches and deliveries learn of runner-to-job assignment (README "Runner
// attribution") and of repository names. Passes, webhook goroutines and
// listener goroutines share it.
type attribution struct {
	mu         sync.Mutex
	byRunner   map[string]assignment
	names      map[int64]string     // repo id: owner/name
	completed  map[string]time.Time // runner name: when a completed delivery named it
	searchedAt map[int64]time.Time  // repo id: last search start
	searching  map[int64]*searchFlight
}

type searchFlight struct {
	done chan struct{}
	err  error
}

func newAttribution() *attribution {
	return &attribution{byRunner: map[string]assignment{}, names: map[int64]string{}, completed: map[string]time.Time{},
		searchedAt: map[int64]time.Time{}, searching: map[int64]*searchFlight{}}
}

// observe publishes a repository read by a pass that started at start. The
// pass's own assignments for the repository are replaced by the in_progress
// jobs its listing put on this instance's runners. Any other assignment is
// kept unless the listing, taken after it was recorded, showed its job
// completed or in_progress on another runner, or put the runner on another
// job. A job still listed queued is GitHub's lag, not a contradiction.
func (a *attribution) observe(v *repoView, start time.Time) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.names[v.id] = v.full
	for name, as := range a.byRunner {
		if !strings.EqualFold(as.repository, v.full) {
			continue
		}
		on, running := v.inProgressOn[as.jobID]
		contradicted := !as.at.After(start) && (v.completed[as.jobID] || running && on != name)
		if as.fromPass || contradicted || start.Sub(as.at) > assignmentTTL {
			delete(a.byRunner, name)
		}
	}
	for _, j := range v.stuck {
		a.byRunner[*j.RunnerName] = assignment{repository: v.full, jobID: j.ID, fromPass: true, at: start}
	}
}

// forgetEnded drops the assignments, recorded before a Lux listing taken
// at start, of runners with no live Run in it.
func (a *attribution) forgetEnded(live map[string]bool, start time.Time) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for name, as := range a.byRunner {
		if !live[name] && as.at.Before(start) {
			delete(a.byRunner, name)
		}
	}
}

func (a *attribution) observeName(id int64, full string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.names[id] = full
	a.mu.Unlock()
}

// noteInProgress records a signed in_progress delivery whose runner is one
// of this instance's: it arrives seconds after pickup, long before the next
// pass would list the job.
func (r *Reconciler) noteInProgress(repository string, runner string, jobID int64) {
	if r.att == nil || !strings.HasPrefix(runner, r.instancePrefix()) {
		return
	}
	r.att.mu.Lock()
	r.att.byRunner[runner] = assignment{repository: repository, jobID: jobID, at: r.now()}
	r.att.mu.Unlock()
}

// noteCompleted records a signed completed delivery naming one of this
// instance's runners. A JIT runner executes one job, so the runner is done.
// The delivery must come from the runner's own repository: another
// repository of the installation can register a runner of any name and
// have GitHub sign a delivery for it.
func (r *Reconciler) noteCompleted(repository, runner string) {
	if r.att == nil {
		return
	}
	repoID, ok := r.ParseRunner(runner)
	if !ok {
		return
	}
	now := r.now()
	r.att.mu.Lock()
	defer r.att.mu.Unlock()
	if full, known := r.att.names[repoID]; !known || !strings.EqualFold(full, repository) {
		return
	}
	for name, at := range r.att.completed {
		if now.Sub(at) > completedTTL {
			delete(r.att.completed, name)
		}
	}
	r.att.completed[runner] = now
	delete(r.att.byRunner, runner)
}

// RunnerCompleted reports whether a signed completed delivery named runner.
func (r *Reconciler) RunnerCompleted(runner string) bool {
	if r.att == nil {
		return false
	}
	r.att.mu.Lock()
	defer r.att.mu.Unlock()
	_, ok := r.att.completed[runner]
	return ok
}

// CandidateJob returns the repository and id of the last job a pass, search
// or delivery put on runner within assignmentTTL. It is a candidate only:
// GitHub's job record decides.
func (r *Reconciler) CandidateJob(runner string) (repository string, jobID int64, ok bool) {
	if r.att == nil {
		return "", 0, false
	}
	r.att.mu.Lock()
	defer r.att.mu.Unlock()
	as, ok := r.att.byRunner[runner]
	if !ok || r.now().Sub(as.at) > assignmentTTL {
		return "", 0, false
	}
	return as.repository, as.jobID, true
}

// RepositoryName returns owner/name of the repository with numeric id as
// the last refresh or read knew it.
func (r *Reconciler) RepositoryName(id int64) (string, bool) {
	if r.att == nil {
		return "", false
	}
	r.att.mu.Lock()
	defer r.att.mu.Unlock()
	full, ok := r.att.names[id]
	return full, ok
}

// ParseRunner returns the repository id of one of this instance's runner names.
func (r *Reconciler) ParseRunner(name string) (repoID int64, ok bool) {
	rest, found := strings.CutPrefix(name, r.instancePrefix())
	if !found {
		return 0, false
	}
	id, _, _ := strings.Cut(rest, "-")
	repoID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || repoID <= 0 {
		return 0, false
	}
	// The canonical spelling only, as for every other managed runner name.
	if _, _, ok := r.parseRunnerName(repoID, name); !ok {
		return 0, false
	}
	return repoID, true
}

// LiveRun reports whether a live Lux Run of this instance carries the
// runner's name and its repository. Lux lists one Run per runner name.
func (r *Reconciler) LiveRun(ctx context.Context, runner string, repoID int64) (bool, error) {
	runs, err := r.lux.List(ctx, map[string]string{LabelManaged: r.cfg.Instance, LabelRunner: runner}, LiveStates...)
	if err != nil {
		return false, err
	}
	for _, run := range runs {
		if run.Labels[LabelRepo] == strconv.FormatInt(repoID, 10) {
			return true, nil
		}
	}
	return false, nil
}

// RateLimitedUntil is when GitHub's last rate limit on the installation,
// seen by a pass or by the registry listener, ends.
func (r *Reconciler) RateLimitedUntil() time.Time {
	r.limitMu.Lock()
	defer r.limitMu.Unlock()
	return r.rateLimitedUntil
}

// NoteRateLimit records err if it is a GitHub rate limit, so passes and the
// registry listener both stop calling GitHub until it ends.
func (r *Reconciler) NoteRateLimit(err error) {
	var apiErr *githubapp.APIError
	if !errors.As(err, &apiErr) || !apiErr.RateLimited {
		return
	}
	r.limitMu.Lock()
	defer r.limitMu.Unlock()
	if apiErr.RetryAt.After(r.rateLimitedUntil) {
		r.rateLimitedUntil = apiErr.RetryAt
	}
}

// SearchRunnerJobs is the fallback when neither a pass nor a delivery has
// named a runner's job yet. It lists the repository's in_progress workflow
// runs and their latest-attempt jobs, newest first and within
// searchPageBudget pages, and publishes every in_progress job on one of
// this instance's runners as a candidate. A repository is searched at most
// once per searchGap; concurrent callers share one search and its error.
// before runs ahead of the first GitHub call, and its error is returned
// without a search.
func (r *Reconciler) SearchRunnerJobs(ctx context.Context, repo githubapp.Repository, repoID int64, before func() error) error {
	a := r.att
	if a == nil {
		return nil
	}
	now := r.now()
	a.mu.Lock()
	if f, ok := a.searching[repoID]; ok {
		a.mu.Unlock()
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if now.Sub(a.searchedAt[repoID]) < searchGap {
		a.mu.Unlock()
		return nil
	}
	if err := before(); err != nil {
		a.mu.Unlock()
		return err
	}
	f := &searchFlight{done: make(chan struct{})}
	a.searching[repoID], a.searchedAt[repoID] = f, now
	a.mu.Unlock()

	jobs, err := r.searchInProgress(ctx, repo, repoID)
	r.NoteRateLimit(err)
	a.mu.Lock()
	for _, j := range jobs {
		a.byRunner[*j.RunnerName] = assignment{repository: repo.Owner + "/" + repo.Name, jobID: j.ID, at: now}
	}
	f.err = err
	delete(a.searching, repoID)
	a.mu.Unlock()
	close(f.done)
	return err
}

// searchInProgress returns the in_progress jobs on this instance's runners
// that the first searchPageBudget pages show, found before any error.
func (r *Reconciler) searchInProgress(ctx context.Context, repo githubapp.Repository, repoID int64) ([]githubapp.WorkflowJob, error) {
	var found []githubapp.WorkflowJob
	budget := searchPageBudget
	for page := 1; page <= maxRunPages && budget > 0; page++ {
		budget--
		runs, err := r.gh.ListWorkflowRuns(ctx, repo, githubapp.WorkflowRunListOptions{PageOptions: githubapp.PageOptions{Page: page, PerPage: pageSize}, Status: "in_progress"})
		if err != nil {
			return found, err
		}
		for _, run := range runs.Runs {
			for jp := 1; jp <= maxRunPages; jp++ {
				if budget <= 0 {
					return found, nil
				}
				budget--
				jobs, err := r.gh.ListWorkflowRunJobs(ctx, repo, run.ID, githubapp.WorkflowJobListOptions{PageOptions: githubapp.PageOptions{Page: jp, PerPage: pageSize}, Filter: "latest"})
				if err != nil {
					return found, err
				}
				for _, j := range jobs.Jobs {
					if j.Status == "in_progress" && j.RunnerName != nil && r.managedRunner(repoID, *j.RunnerName) {
						found = append(found, j)
					}
				}
				if !jobs.HasNext {
					break
				}
			}
		}
		if !runs.HasNext {
			break
		}
	}
	return found, nil
}
