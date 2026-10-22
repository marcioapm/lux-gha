package reconcile

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/marcioapm/lux-gha/githubapp"
)

// Assignment is a candidate for the job a runner of this instance executes:
// a job a pass listed as in_progress on it, or a signed in_progress delivery
// naming it. The registry listener confirms it with a job read before use.
type Assignment struct {
	Repository string // owner/name
	JobID      int64
	RunID      int64
}

// attribution publishes, for the registry's runner listener, what passes and
// deliveries learn of runner-to-job assignment (README "Runner attribution")
// and of repository names. Passes and webhook goroutines write it; listener
// goroutines read it.
type attribution struct {
	mu       sync.Mutex
	byRunner map[string]Assignment
	names    map[int64]string // repo id: owner/name
}

func newAttribution() *attribution {
	return &attribution{byRunner: map[string]Assignment{}, names: map[int64]string{}}
}

// observe replaces the assignments a repository read this pass holds with
// the in_progress jobs its listing put on this instance's runners. A runner
// no longer listed is forgotten: its job completed, or GitHub listed it
// elsewhere.
func (a *attribution) observe(v *repoView) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.names[v.id] = v.full
	for name, as := range a.byRunner {
		if strings.EqualFold(as.Repository, v.full) {
			delete(a.byRunner, name)
		}
	}
	for _, j := range v.stuck {
		a.byRunner[*j.RunnerName] = Assignment{Repository: v.full, JobID: j.ID, RunID: j.RunID}
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
func (r *Reconciler) noteInProgress(repository string, runner string, jobID, runID int64) {
	if r.att == nil || !strings.HasPrefix(runner, "lux-gha-"+r.cfg.Instance+"-") {
		return
	}
	r.att.mu.Lock()
	r.att.byRunner[runner] = Assignment{Repository: repository, JobID: jobID, RunID: runID}
	r.att.mu.Unlock()
}

// RunnerAssignment returns the last job a pass or delivery put on runner.
// It is a candidate only: GitHub's job record decides.
func (r *Reconciler) RunnerAssignment(runner string) (Assignment, bool) {
	if r.att == nil {
		return Assignment{}, false
	}
	r.att.mu.Lock()
	defer r.att.mu.Unlock()
	as, ok := r.att.byRunner[runner]
	return as, ok
}

// CandidateJob is RunnerAssignment in the registry listener's terms.
func (r *Reconciler) CandidateJob(runner string) (string, int64, bool) {
	as, ok := r.RunnerAssignment(runner)
	return as.Repository, as.JobID, ok
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
	rest, found := strings.CutPrefix(name, "lux-gha-"+r.cfg.Instance+"-")
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

// FindRunnerJob lists the repository's in_progress workflow runs and their
// latest-attempt jobs, the listing a pass reads, for the job GitHub shows on
// runner. It is the fallback when neither a pass nor a delivery has named
// one yet; pages are bounded as in a pass.
func (r *Reconciler) FindRunnerJob(ctx context.Context, repo githubapp.Repository, runner string) (githubapp.WorkflowJob, bool, error) {
	for page := 1; page <= maxRunPages; page++ {
		runs, err := r.gh.ListWorkflowRuns(ctx, repo, githubapp.WorkflowRunListOptions{PageOptions: githubapp.PageOptions{Page: page, PerPage: pageSize}, Status: "in_progress"})
		if err != nil {
			return githubapp.WorkflowJob{}, false, err
		}
		for _, run := range runs.Runs {
			for jp := 1; jp <= maxRunPages; jp++ {
				jobs, err := r.gh.ListWorkflowRunJobs(ctx, repo, run.ID, githubapp.WorkflowJobListOptions{PageOptions: githubapp.PageOptions{Page: jp, PerPage: pageSize}, Filter: "latest"})
				if err != nil {
					return githubapp.WorkflowJob{}, false, err
				}
				for _, j := range jobs.Jobs {
					if j.Status == "in_progress" && j.RunnerName != nil && *j.RunnerName == runner {
						return j, true, nil
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
	return githubapp.WorkflowJob{}, false, nil
}
