package reconcile

import (
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/marcioapm/lux-gha/githubapp"
	"github.com/marcioapm/lux-gha/luxclient"
)

const (
	// hostLossHorizon: a job whose Run ended longer ago is neither
	// discovered nor kept waiting for a re-run.
	hostLossHorizon = 24 * time.Hour
	// maxRerunTries bounds the passes whose GitHub reads or re-run of a lost
	// job failed; a missing permission is not counted (see rerunLost).
	maxRerunTries = 5
	// lostCursorSkew allows for Lux's clock running behind the bridge's.
	lostCursorSkew = time.Minute
)

// endedStates are the Lux Run states that are not live; succeeded is left
// out, since such a Run ran its job to the end.
var endedStates = []string{"stopped", "failed", "terminated", "lost"}

// lostJob is a job whose Lux Run's host was lost, waiting for its workflow
// run to complete so that the job can be re-run.
type lostJob struct {
	repoID, jobID, runID, attempt int64
	// runner is the lost Run's runner name. verified: GitHub says the job
	// ran on it and ended failed or cancelled; a Run lost before its runner
	// took the job is never verified, as the job ran on a later gen.
	runner   string
	verified bool
	since    time.Time // the Run's end
	tries    int
}

// hostLost reports whether Lux's stateReason says the Run's host was taken
// away (preemption, drain or loss) rather than the job causing the stop. A
// Run terminated on request ("terminated; host lost" when its host went
// during the terminate) is not: its job had ended or was not wanted.
func hostLost(reason string) bool {
	if strings.HasPrefix(reason, "terminated") {
		return false
	}
	return strings.Contains(reason, "preempt") || strings.Contains(reason, "drain") || strings.Contains(reason, "host lost")
}

// rerunEligible reports whether a job of run_attempt attempt stopped for
// reason is re-run: each re-run increments the workflow run's run_attempt,
// so the attempt bounds the re-runs per workflow run with no local state.
func (r *Reconciler) rerunEligible(reason string, attempt int64) bool {
	return hostLost(reason) && r.attemptEligible(attempt)
}

func (r *Reconciler) attemptEligible(attempt int64) bool {
	return attempt >= 1 && attempt <= int64(r.cfg.Reconcile.Reruns())
}

// queueLost queues the job of ended Run runID that lost its host, once per
// Run: a Run already seen is not queued again after its job was settled.
func (r *Reconciler) queueLost(runID string, finished time.Time, l *lostJob) {
	if q := r.lost[l.key()]; q != nil {
		q.verified = q.verified || l.verified
		r.lostSeen[runID] = finished
		return
	}
	if _, seen := r.lostSeen[runID]; seen {
		return
	}
	r.lostSeen[runID] = finished
	l.since = finished
	r.lost[l.key()] = l
}

func (l *lostJob) key() string {
	return strconv.FormatInt(l.repoID, 10) + "/" + strconv.FormatInt(l.jobID, 10)
}

// discoverLost queues the jobs of this instance's Runs that lost their host
// and ended within hostLossHorizon, whether or not the bridge saw the job
// in_progress under an ended Run: a runner that cancels its job within the
// stop grace leaves nothing for closeStopped.
//
// In steady state a candidate is a Run live in the previous pass's
// discovery (r.lostLive) and absent from p.live now; each is read with one
// Get. Otherwise the ended Runs are listed newest first down to
// r.lostCursor, since Lux cannot filter by finishedAt: on the first pass
// after a start, after a failed read or an aborted pass, and after a pass
// with an unresolved submission or an unread repository, whose Run may
// commit and end between two passes without being live in either. A Run
// ending after this pass was live in it or is created after it, so the
// cursor is the oldest live Run's creation; the first pass after a start
// reaches back hostLossHorizon plus the longest route timeout.
func (p *pass) discoverLost() {
	r := p.r
	if r.cfg.Reconcile.Reruns() == 0 {
		return
	}
	now := r.now()
	var ok bool
	if p.lostPrev == nil {
		ok = p.listEnded(now)
	} else {
		ok = p.readEnded(p.lostPrev, now)
	}
	if !ok {
		return
	}
	next := now
	live := map[string]bool{}
	for _, run := range p.live {
		if run.CreatedAt.Before(next) {
			next = run.CreatedAt
		}
		if l, ok := lostFromLabels(run.Labels); ok && r.attemptEligible(l.attempt) {
			live[run.ID] = true
		}
	}
	r.lostCursor = next.Add(-lostCursorSkew)
	if p.unresolved == 0 && !p.unread {
		r.lostLive = live
	}
	maps.DeleteFunc(r.lostSeen, func(_ string, at time.Time) bool { return now.Sub(at) > hostLossHorizon })
}

// listEnded lists the ended Runs created since r.lostCursor. Past the
// client's page limit the window's candidates are skipped with a WARN and
// the cursor still advances, so the next pass does not fail the same way.
func (p *pass) listEnded(now time.Time) bool {
	r := p.r
	since := r.lostCursor
	if since.IsZero() {
		since = now.Add(-hostLossHorizon - r.longestTimeout())
	}
	runs, err := r.lux.ListCreatedSince(p.ctx, map[string]string{LabelManaged: r.cfg.Instance}, since, endedStates...)
	switch {
	case errors.Is(err, luxclient.ErrTooManyPages):
		r.log.Warn("too many ended Lux Runs to look for lost hosts; skipping them", "since", since)
		return true
	case err != nil:
		r.log.Error("listing ended Lux Runs for lost hosts failed", "error", err)
		p.setError(ErrLux)
		return false
	}
	for _, run := range runs {
		p.considerEnded(run, now)
	}
	return true
}

// readEnded reads each Run of prev that is no longer live. On a failed read
// the next pass lists from the unchanged cursor, which covers every Run of
// prev; Runs already queued are not queued again (lostSeen).
func (p *pass) readEnded(prev map[string]bool, now time.Time) bool {
	r := p.r
	for _, run := range p.live {
		delete(prev, run.ID)
	}
	for _, id := range sortedKeys(prev) {
		run, err := r.lux.Get(p.ctx, id)
		if err != nil {
			r.log.Error("reading an ended Lux Run for a lost host failed", "run", id, "error", err)
			p.setError(ErrLux)
			return false
		}
		p.considerEnded(*run, now)
	}
	return true
}

func (p *pass) considerEnded(run luxclient.Run, now time.Time) {
	finished := now
	if run.FinishedAt != nil {
		finished = *run.FinishedAt
	}
	if now.Sub(finished) > hostLossHorizon {
		return
	}
	if l, ok := lostFromLabels(run.Labels); ok && p.r.rerunEligible(run.StateReason, l.attempt) {
		p.r.queueLost(run.ID, finished, l)
	}
}

// lostFromLabels reads a Run's job identity; a Run missing any of it is
// skipped.
func lostFromLabels(labels map[string]string) (*lostJob, bool) {
	var ids [4]int64
	for i, k := range []string{LabelRepo, LabelJob, LabelRunID, LabelRunAttempt} {
		n, err := strconv.ParseInt(labels[k], 10, 64)
		if err != nil || n <= 0 {
			return nil, false
		}
		ids[i] = n
	}
	if labels[LabelRunner] == "" {
		return nil, false
	}
	return &lostJob{repoID: ids[0], jobID: ids[1], runID: ids[2], attempt: ids[3], runner: labels[LabelRunner]}, true
}

func (r *Reconciler) longestTimeout() time.Duration {
	var d time.Duration
	for _, route := range r.cfg.Routes {
		d = max(d, route.Timeout.Duration)
	}
	return d
}

// rerunStep is what a pass does with a lost job.
type rerunStep int

const (
	stepWait  rerunStep = iota // the workflow run is not completed
	stepDrop                   // settled: re-run already, gone, or not re-run
	stepRetry                  // a GitHub read failed
	stepDue                    // re-run it now
)

// rerunLost re-runs the lost jobs of repositories read this pass once their
// workflow run has completed failed or cancelled. GitHub's run_attempt is the
// record: a run whose attempt moved past the job's was re-run already, by
// this bridge before a restart or by anyone else. Jobs of one workflow run
// due together are re-run with one request, since a job re-run starts a new
// attempt and its siblings could no longer be re-run from theirs.
func (p *pass) rerunLost() error {
	r := p.r
	p.discoverLost()
	repos := map[int64]*repoView{}
	for _, v := range p.repos {
		repos[v.id] = v
	}
	// A sibling's Run still live (stopping, uploading its log) may end lost
	// in a later pass; re-running now would start a new attempt it is not
	// part of, so the workflow run waits for all of its Runs.
	runLive := map[int64]bool{}
	for _, run := range p.live {
		if id, err := strconv.ParseInt(run.Labels[LabelRunID], 10, 64); err == nil {
			runLive[id] = true
		}
	}
	now := r.now()
	due := map[int64][]string{}
	for _, key := range sortedKeys(r.lost) {
		l := r.lost[key]
		if now.Sub(l.since) > hostLossHorizon {
			r.log.Warn("not re-running a job lost longer ago than the horizon", "repoID", l.repoID, "job", l.jobID, "lostAt", l.since)
			delete(r.lost, key)
			continue
		}
		v := repos[l.repoID]
		if v == nil {
			continue
		}
		log := r.log.With("repository", v.full, "workflowRun", l.runID, "job", l.jobID, "attempt", l.attempt)
		// A run listed active is not completed; one with a live Run may
		// still lose a sibling.
		if v.active[l.runID] || runLive[l.runID] {
			continue
		}
		step, err := p.checkLost(v, l, log)
		if err != nil {
			return err
		}
		switch step {
		case stepDrop:
			delete(r.lost, key)
		case stepRetry:
			p.retryLost(key, log)
		case stepDue:
			due[l.runID] = append(due[l.runID], key)
		}
	}
	for _, runID := range sortedKeys(due) {
		if err := p.rerunRun(runID, due[runID], repos); err != nil || p.noRerun {
			return err
		}
	}
	return nil
}

func (p *pass) retryLost(key string, log *slog.Logger) {
	l := p.r.lost[key]
	l.tries++
	if l.tries >= maxRerunTries {
		log.Warn("not re-running a job: GitHub failed too often", "tries", l.tries)
		delete(p.r.lost, key)
	}
}

// checkLost verifies, once, that the job ran on the lost Run's runner, then
// reads its workflow run (once per pass per run).
func (p *pass) checkLost(v *repoView, l *lostJob, log *slog.Logger) (rerunStep, error) {
	if !l.verified {
		job, err := p.r.gh.GetWorkflowJob(p.ctx, v.ref, l.jobID)
		if step, failed, abort := p.readFailed(err, log, "reading a lost job failed"); failed {
			return step, abort
		}
		switch {
		case job.RunnerName == nil || *job.RunnerName != l.runner || job.RunAttempt != l.attempt:
			log.Info("the lost Run did not hold the job; not re-running it", "runner", l.runner)
			return stepDrop, nil
		case job.Status != "completed":
			return stepWait, nil
		case !failedOrCancelled(job.Conclusion):
			log.Info("the lost job did not fail; not re-running it", "conclusion", job.Conclusion)
			return stepDrop, nil
		}
		l.verified = true
	}
	run, ok := p.workflowRuns[l.runID]
	if !ok {
		var err error
		run, err = p.r.gh.GetWorkflowRun(p.ctx, v.ref, l.runID)
		if step, failed, abort := p.readFailed(err, log, "reading a workflow run failed"); failed {
			return step, abort
		}
		p.workflowRuns[l.runID] = run
	}
	switch {
	case run.RunAttempt != l.attempt:
		log.Info("workflow run already re-run", "runAttempt", run.RunAttempt)
		return stepDrop, nil
	case *run.Status != "completed":
		return stepWait, nil
	case !failedOrCancelled(run.Conclusion):
		log.Info("workflow run did not fail; not re-running the job", "conclusion", run.Conclusion)
		return stepDrop, nil
	}
	return stepDue, nil
}

// readFailed classifies a failed GitHub read; failed is false for a nil err.
// A 404 drops the job, a rate limit aborts the pass, anything else is retried.
func (p *pass) readFailed(err error, log *slog.Logger, msg string) (step rerunStep, failed bool, abort error) {
	var apiErr *githubapp.APIError
	switch {
	case err == nil:
		return 0, false, nil
	case p.rateLimited(err):
		return stepWait, true, errAbort
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound:
		log.Info(msg+": gone; not re-running the job", "error", err)
		return stepDrop, true, nil
	}
	log.Error(msg, "error", err)
	p.setError(ErrGitHub)
	return stepRetry, true, nil
}

func failedOrCancelled(c *string) bool {
	return c != nil && (*c == "failure" || *c == "cancelled")
}

// supersedable: only a push or pull_request run is replaced by a newer run of
// its branch; a newer workflow_dispatch or schedule run may differ in inputs
// or be unrelated. The listing is newest first, so any other id is newer.
func supersedable(run githubapp.WorkflowRun) bool {
	return run.HeadBranch != nil && *run.HeadBranch != "" && run.WorkflowID > 0 &&
		(run.Event == "push" || run.Event == "pull_request")
}

// sameHead: pull_request runs of forks share branch names (main, patch-1),
// so a newer run supersedes ours only from the same head repository.
func sameHead(latest, run githubapp.WorkflowRun) bool {
	if run.Event != "pull_request" {
		return true
	}
	return latest.HeadRepository != nil && run.HeadRepository != nil && latest.HeadRepository.ID == run.HeadRepository.ID
}

// rerunRun re-runs the due lost jobs of workflow run runID, unless a newer
// push or pull_request run of the same workflow, branch, event (and, for a
// pull request, head repository) superseded it. One job is re-run with
// jobs/{id}/rerun, which also re-runs its dependents but not its siblings;
// several with rerun-failed-jobs, which re-runs every failed or cancelled job
// of the run.
func (p *pass) rerunRun(runID int64, keys []string, repos map[int64]*repoView) error {
	r := p.r
	first := r.lost[keys[0]]
	v, run := repos[first.repoID], p.workflowRuns[runID]
	log := r.log.With("repository", v.full, "workflowRun", runID, "attempt", first.attempt, "jobs", len(keys))
	drop := func() {
		for _, k := range keys {
			delete(r.lost, k)
		}
	}
	retry := func() {
		for _, k := range keys {
			p.retryLost(k, log)
		}
	}
	if supersedable(run) {
		latest, ok, err := r.gh.LatestWorkflowRun(p.ctx, v.ref, run.WorkflowID, *run.HeadBranch, run.Event)
		switch {
		case p.rateLimited(err):
			return errAbort
		case err != nil:
			log.Error("reading the workflow's latest run failed", "error", err)
			p.setError(ErrGitHub)
			retry()
			return nil
		case ok && latest.ID != runID && sameHead(latest, run):
			log.Info("workflow run was superseded by a newer run; not re-running it", "newerRun", latest.ID)
			drop()
			return nil
		}
	}
	var err error
	if len(keys) == 1 {
		err = r.gh.RerunJob(p.ctx, v.ref, first.jobID)
	} else {
		err = r.gh.RerunFailedJobs(p.ctx, v.ref, runID)
	}
	var apiErr *githubapp.APIError
	switch {
	case p.rateLimited(err):
		return errAbort
	case err == nil:
		p.st.Rerun += len(keys)
		log.Warn("re-ran jobs whose Run's host was lost")
		drop()
	case errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusConflict || apiErr.StatusCode == http.StatusUnprocessableEntity):
		log.Info("GitHub refused the re-run as conflicting", "status", apiErr.StatusCode)
		drop()
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden && apiErr.NotAccessible:
		// Kept, untried: a permission grant needs no new attempt count.
		p.denied(err, &p.noRerun, "Actions")
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden:
		log.Warn("GitHub refused the re-run; not re-running the jobs", "status", apiErr.StatusCode)
		drop()
	default:
		log.Error("re-running jobs failed", "error", err)
		p.setError(ErrGitHub)
		retry()
	}
	return nil
}
