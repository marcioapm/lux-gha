package reconcile

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/marcioapm/lux-gha/githubapp"
)

// lostJob is a job whose Lux Run's host was taken away, waiting for its
// workflow run to complete so that the job can be re-run.
type lostJob struct {
	repoID, jobID, runID, attempt int64
}

// hostLost reports whether Lux's stateReason says the Run's host was taken
// away (preemption, drain or loss) rather than the job causing the stop.
func hostLost(reason string) bool {
	return strings.Contains(reason, "preempt") || strings.Contains(reason, "drain") || strings.Contains(reason, "host lost")
}

// rerunEligible reports whether a job of run_attempt attempt stopped for
// reason is re-run: each re-run increments the workflow run's run_attempt,
// so the attempt bounds the re-runs per workflow run with no local state.
func (r *Reconciler) rerunEligible(reason string, attempt int64) bool {
	return hostLost(reason) && attempt >= 1 && attempt <= int64(r.cfg.Reconcile.Reruns())
}

// rerunLost re-runs each lost job of a repository read this pass once its
// workflow run has completed (failed, or cancelled by unstick). GitHub's
// run_attempt is the record: a run whose attempt moved past the job's was
// re-run already, by this bridge before a restart or by anyone else.
func (p *pass) rerunLost() error {
	r := p.r
	for _, v := range p.repos {
		for _, key := range sortedKeys(r.lost) {
			l := r.lost[key]
			if l.repoID != v.id || p.noRerun {
				continue
			}
			log := r.log.With("repository", v.full, "workflowRun", l.runID, "job", l.jobID, "attempt", l.attempt)
			done, err := p.rerun(v, l, log)
			if err != nil {
				return err
			}
			if done {
				delete(r.lost, key)
			}
		}
	}
	return nil
}

// rerun reports whether l is settled: re-run, re-run already, or gone.
func (p *pass) rerun(v *repoView, l *lostJob, log *slog.Logger) (bool, error) {
	run, err := p.r.gh.GetWorkflowRun(p.ctx, v.ref, l.runID)
	var apiErr *githubapp.APIError
	switch {
	case p.rateLimited(err):
		return false, errAbort
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound:
		log.Info("workflow run is gone; not re-running the job")
		return true, nil
	case err != nil:
		log.Error("reading a workflow run failed", "error", err)
		p.setError(ErrGitHub)
		return false, nil
	case run.RunAttempt != l.attempt:
		log.Info("workflow run already re-run", "runAttempt", run.RunAttempt)
		return true, nil
	case *run.Status != "completed":
		return false, nil
	case run.Conclusion == nil || (*run.Conclusion != "failure" && *run.Conclusion != "cancelled"):
		log.Info("workflow run did not fail; not re-running the job", "conclusion", run.Conclusion)
		return true, nil
	}
	err = p.r.gh.RerunJob(p.ctx, v.ref, l.jobID)
	if p.rateLimited(err) {
		return false, errAbort
	}
	switch {
	case err == nil:
		p.st.Rerun++
		log.Warn("re-ran a job whose Run's host was lost")
		return true, nil
	case errors.As(err, &apiErr) && (apiErr.Already || apiErr.StatusCode == http.StatusConflict || apiErr.StatusCode == http.StatusUnprocessableEntity):
		log.Info("GitHub refused the re-run as already running or re-run", "status", apiErr.StatusCode)
		return true, nil
	case p.denied(err, &p.noRerun, "Actions"):
		return false, nil
	}
	log.Error("re-running a job failed", "error", err)
	p.setError(ErrGitHub)
	return false, nil
}
