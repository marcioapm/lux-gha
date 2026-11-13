package reconcile

import (
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/marcioapm/lux-gha/githubapp"
)

const (
	queuedAllowance   = 1
	queuedJobLifetime = 24 * time.Hour
)

type heldJob struct {
	runID int64
	at    time.Time
}

type heldJobs struct {
	mu   sync.Mutex
	jobs map[string]map[int64]heldJob
}

func (r *Reconciler) rememberQueuedJob(fullName string, runID, jobID int64) {
	if runID <= 0 {
		return
	}
	now := r.now()
	key := strings.ToLower(fullName)
	q := &r.queuedJobs
	q.mu.Lock()
	defer q.mu.Unlock()
	for repo := range q.jobs {
		q.dropExpired(repo, now)
	}
	if q.jobs[key] == nil {
		q.jobs[key] = map[int64]heldJob{}
	}
	q.jobs[key][jobID] = heldJob{runID: runID, at: now}
}

func (q *heldJobs) dropExpired(key string, now time.Time) {
	maps.DeleteFunc(q.jobs[key], func(_ int64, d heldJob) bool { return now.Sub(d.at) >= queuedJobLifetime })
	if len(q.jobs[key]) == 0 {
		delete(q.jobs, key)
	}
}

func (r *Reconciler) forgetQueuedJobs(fullName string, done func(jobID int64, d heldJob) bool) {
	key := strings.ToLower(fullName)
	q := &r.queuedJobs
	q.mu.Lock()
	defer q.mu.Unlock()
	maps.DeleteFunc(q.jobs[key], done)
	if len(q.jobs[key]) == 0 {
		delete(q.jobs, key)
	}
}

func (r *Reconciler) unlistedQueuedJobRuns(fullName string, listed map[int64]bool) []int64 {
	key := strings.ToLower(fullName)
	q := &r.queuedJobs
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dropExpired(key, r.now())
	var runs []int64
	for job, d := range q.jobs[key] {
		if !listed[job] && !slices.Contains(runs, d.runID) {
			runs = append(runs, d.runID)
		}
	}
	slices.Sort(runs)
	return runs
}

func (p *pass) readHeldDemand(v *repoView) error {
	listed := maps.Clone(v.seen)
	if err := p.scanPendingRunsOnce(v); err != nil {
		return err
	}
	if err := p.readQueuedJobRuns(v); err != nil {
		return err
	}
	now, lag := p.r.now(), p.r.cfg.Reconcile.HotWindow.Duration
	p.r.forgetQueuedJobs(v.full, func(job int64, d heldJob) bool {
		_, running := v.inProgressOn[job]
		_, demand := v.demand[job]
		runReadWhole := v.active[d.runID] && !v.partial[d.runID]
		return listed[job] || running || v.completed[job] || runReadWhole && !demand && now.Sub(d.at) >= lag
	})
	return nil
}

func (p *pass) scanPendingRunsOnce(v *repoView) error {
	if p.r.scannedPending[v.id] {
		return nil
	}
	p.budget = p.statusBudget[queuedAllowance]
	if p.budget <= 0 {
		p.truncated = true
		return nil
	}
	pending := map[int64]bool{}
	for page := 1; ; page++ {
		if page > maxRunPages || p.budget <= 0 {
			p.truncated = true
			p.r.log.Warn("pending workflow runs exceed this pass's allowance; a job they hold is served once its release is delivered", "repository", v.full)
			break
		}
		runs, err := p.r.gh.ListWorkflowRuns(p.ctx, v.ref, githubapp.WorkflowRunListOptions{PageOptions: githubapp.PageOptions{Page: page, PerPage: pageSize}, Status: "pending"})
		if err != nil {
			return err
		}
		for _, run := range runs.Runs {
			pending[run.ID] = true
			if err := p.readHeldRun(v, run); err != nil {
				return err
			}
		}
		if !runs.HasNext {
			break
		}
	}
	for job := range v.demand {
		if runID := v.meta[job].runID; pending[runID] {
			p.r.rememberQueuedJob(v.full, runID, job)
		}
	}
	p.r.scannedPending[v.id] = true
	return nil
}

func (p *pass) readQueuedJobRuns(v *repoView) error {
	p.budget = p.statusBudget[queuedAllowance]
	for _, runID := range p.r.unlistedQueuedJobRuns(v.full, v.seen) {
		if p.budget <= 0 {
			p.truncated = true
			return nil
		}
		run, err := p.r.gh.GetWorkflowRun(p.ctx, v.ref, runID)
		var apiErr *githubapp.APIError
		gone := errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
		ended := err == nil && run.Status != nil && *run.Status == "completed"
		if err != nil || ended {
			p.budget--
			p.statusBudget[queuedAllowance] = p.budget
		}
		if gone || ended {
			p.r.forgetQueuedJobs(v.full, func(_ int64, d heldJob) bool { return d.runID == runID })
			continue
		}
		if err != nil {
			return err
		}
		if err := p.readHeldRun(v, run); err != nil {
			return err
		}
	}
	return nil
}

func (p *pass) readHeldRun(v *repoView, run githubapp.WorkflowRun) error {
	if p.budget <= 0 {
		p.truncated = true
		v.partial[run.ID] = true
		return nil
	}
	err := p.readRunJobs(v, run)
	p.budget = max(p.budget-1, 0)
	p.statusBudget[queuedAllowance] = p.budget
	return err
}
