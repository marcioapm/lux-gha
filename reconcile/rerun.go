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
	// maxWorkflowLookback is how long before a lost Run's creation a
	// workflow run may have been created, or its latest attempt started
	// (run_started_at), and still hold the job the Run's runner took. A
	// re-run keeps the run's created_at: a re-run attempt of a run created
	// earlier is reached by run_started_at on a listed page, or as the
	// loss's own label workflow run; any other such attempt, and a run
	// longer than this, is not searched.
	maxWorkflowLookback = 6 * time.Hour
	// lostWindowSkew widens the lost Runs' window for the skew between
	// Lux's and GitHub's clocks.
	lostWindowSkew = time.Minute
	// maxSearchRunPages and maxSearchJobPages bound one lost-runner search:
	// workflow-run listing pages (pageSize runs each) and job listing pages
	// of the candidate runs, budgeted separately.
	maxSearchRunPages = 10
	maxSearchJobPages = 200
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
	// resolved: a direct job read confirmed the identity, so no search is
	// needed while the job runs on.
	runner   string
	verified bool
	resolved bool
	created  time.Time // the Run's creation, before its runner could take a job
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
		// Assignment observation does not establish failed completion, so
		// only the identity is taken over; q stays unverified.
		if l.verified && !q.verified {
			q.jobID, q.runID, q.attempt = l.jobID, l.runID, l.attempt
		}
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
	return strconv.FormatInt(l.repoID, 10) + "/" + l.runner
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
		ok = p.readEnded(now)
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
		if l := lostFromLabels(run.Labels); l != nil {
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

// readEnded reads each Run of p.lostPrev that is no longer live. On a failed
// read the next pass lists from the unchanged cursor, which covers every Run
// of p.lostPrev; Runs already queued are not queued again (lostSeen).
func (p *pass) readEnded(now time.Time) bool {
	r := p.r
	for _, run := range p.live {
		delete(p.lostPrev, run.ID)
	}
	for _, id := range sortedKeys(p.lostPrev) {
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
	if l := lostFromLabels(run.Labels); l != nil && hostLost(run.StateReason) {
		l.created = run.CreatedAt
		p.r.queueLost(run.ID, finished, l)
	}
}

// lostFromLabels reads a Run's job identity, or nil if any of it is missing.
func lostFromLabels(labels map[string]string) *lostJob {
	var ids [4]int64
	for i, k := range []string{LabelRepo, LabelJob, LabelRunID, LabelRunAttempt} {
		n, err := strconv.ParseInt(labels[k], 10, 64)
		if err != nil || n <= 0 {
			return nil
		}
		ids[i] = n
	}
	if labels[LabelRunner] == "" {
		return nil
	}
	return &lostJob{repoID: ids[0], jobID: ids[1], runID: ids[2], attempt: ids[3], runner: labels[LabelRunner]}
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
		var apiErr *githubapp.APIError
		missing := errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
		if !missing {
			if step, failed, abort := p.readFailed(err, log, "reading a lost job failed"); failed {
				return step, abort
			}
		}
		if missing || job.RunnerName == nil || *job.RunnerName != l.runner {
			search := p.searchLost(v)
			candidate, ok := search.jobs[l.runner]
			// A complete candidate workflow is independent of unrelated history,
			// but partial same-workflow results cannot establish the retry group.
			if p.rateLimited(search.err) {
				return stepWait, errAbort
			}
			if search.err != nil && (!ok || !search.complete[candidate.RunID]) {
				if step, failed, abort := p.readFailed(search.err, log, "resolving a lost runner failed"); failed {
					return step, abort
				}
			}
			if !ok {
				return stepRetry, nil
			}
			job, err = p.r.gh.GetWorkflowJob(p.ctx, v.ref, candidate.ID)
			if step, failed, abort := p.readFailed(err, log, "reading the executing job failed"); failed {
				return step, abort
			}
			if job.ID != candidate.ID || job.RunID != candidate.RunID || job.RunAttempt != candidate.RunAttempt ||
				job.RunnerName == nil || *job.RunnerName != l.runner {
				return stepDrop, nil
			}
		} else if job.ID != l.jobID || job.RunID != l.runID || job.RunAttempt != l.attempt {
			return stepDrop, nil
		}
		if !p.r.attemptEligible(job.RunAttempt) {
			return stepDrop, nil
		}
		l.jobID, l.runID, l.attempt, l.resolved = job.ID, job.RunID, job.RunAttempt, true
		switch {
		case job.Status != "completed":
			return stepWait, nil
		case !failedOrCancelled(job.Conclusion):
			log.Info("the lost job did not fail; not re-running it", "conclusion", job.Conclusion)
			return stepDrop, nil
		}
		l.verified = true
	}
	if v.active[l.runID] {
		return stepWait, nil
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
	// Live siblings may have been provisioned for another workflow.
	if len(p.liveRunner) > 0 {
		siblings := p.lostSiblingJobs(v, l.runID)
		if step, failed, abort := p.readFailed(siblings.err, log, "reading lost job siblings failed"); failed {
			return step, abort
		}
		for runner := range siblings.jobs {
			if p.liveRunner[runner] {
				return stepWait, nil
			}
		}
	}
	return stepDue, nil
}

func (p *pass) lostSiblingJobs(v *repoView, runID int64) *lostSearch {
	if p.lostSiblings == nil {
		p.lostSiblings = map[string]*lostSearch{}
	}
	key := strconv.FormatInt(v.id, 10) + "/" + strconv.FormatInt(runID, 10)
	if s := p.lostSiblings[key]; s != nil {
		return s
	}
	s := &lostSearch{jobs: map[string]githubapp.WorkflowJob{}}
	p.lostSiblings[key] = s
	for page := 1; page <= maxRunPages; page++ {
		jobs, err := p.r.gh.ListWorkflowRunJobs(p.ctx, v.ref, runID, githubapp.WorkflowJobListOptions{
			PageOptions: githubapp.PageOptions{Page: page, PerPage: pageSize}, Filter: "latest",
		})
		if err != nil {
			s.err = err
			return s
		}
		for _, job := range jobs.Jobs {
			if job.RunnerName != nil {
				s.jobs[*job.RunnerName] = job
			}
		}
		if !jobs.HasNext {
			return s
		}
	}
	s.err = errors.New("lost job sibling search exceeded its page bound")
	return s
}

type lostSearch struct {
	jobs     map[string]githubapp.WorkflowJob
	complete map[int64]bool
	err      error
}

// lostWindow is the lifetime of an unresolved loss's Run, widened by
// lostWindowSkew: only a workflow run overlapping it can hold the job the
// Run's runner took. runID is the workflow run of the Run's label job.
type lostWindow struct {
	start, end time.Time
	runID      int64
}

// lostWindows maps the runner of each unresolved loss of repository repoID
// to its window.
func (r *Reconciler) lostWindows(repoID int64) map[string]lostWindow {
	windows := map[string]lostWindow{}
	for _, lost := range r.lost {
		if lost.repoID != repoID || lost.verified || lost.resolved {
			continue
		}
		created := lost.created
		if created.IsZero() {
			// No Run outlives its route's timeout.
			created = lost.since.Add(-r.longestTimeout())
		}
		windows[lost.runner] = lostWindow{created.Add(-lostWindowSkew), lost.since.Add(lostWindowSkew), lost.runID}
	}
	return windows
}

// lostScan is one searchLost in progress: the losses not yet resolved, the
// job pages left and the workflow runs whose jobs were listed.
type lostScan struct {
	p       *pass
	v       *repoView
	s       *lostSearch
	targets map[string]lostWindow
	budget  int
	listed  map[int64]bool
}

// listJobs lists the latest jobs of workflow run runID, resolving the
// targets whose runner took one. It is false, with s.err set, when the
// search must end: a failed read or an exhausted job budget.
func (c *lostScan) listJobs(runID int64) bool {
	c.listed[runID] = true
	for jp := 1; jp <= maxRunPages && c.budget > 0; jp++ {
		c.budget--
		jobs, err := c.p.r.gh.ListWorkflowRunJobs(c.p.ctx, c.v.ref, runID, githubapp.WorkflowJobListOptions{
			PageOptions: githubapp.PageOptions{Page: jp, PerPage: pageSize}, Filter: "latest",
		})
		if err != nil {
			c.s.err = err
			return false
		}
		for _, job := range jobs.Jobs {
			if job.RunnerName != nil && job.RunID == runID && job.ID > 0 && job.RunAttempt > 0 {
				c.s.jobs[*job.RunnerName] = job
				delete(c.targets, *job.RunnerName)
			}
		}
		if !jobs.HasNext {
			c.s.complete[runID] = true
			return true
		}
	}
	c.s.err = errors.New("lost runner job search exceeded its page bound")
	return false
}

// searchLost shares a bounded repository search across every lost runner in
// the pass. Listings only nominate jobs; checkLost reads each nominee again.
//
// Runs are listed newest first with created at or before the latest window
// end, and listing stops at a page created wholly before the earliest window
// start minus maxWorkflowLookback. Only a run that may hold the job of a loss
// still unresolved (mayHoldLost) has its jobs listed; a loss is resolved once
// its runner is found, so the skipped set grows during the scan. A re-run
// keeps its workflow run's created_at, so the listing can miss a re-run of a
// run created before the lookback; the label job's own workflow run, which
// the runner most often took a sibling of, is then listed directly.
func (p *pass) searchLost(v *repoView) *lostSearch {
	if p.lostSearches == nil {
		p.lostSearches = map[int64]*lostSearch{}
	}
	if s := p.lostSearches[v.id]; s != nil {
		return s
	}
	s := &lostSearch{jobs: map[string]githubapp.WorkflowJob{}, complete: map[int64]bool{}}
	p.lostSearches[v.id] = s
	c := &lostScan{p: p, v: v, s: s, targets: p.r.lostWindows(v.id), budget: maxSearchJobPages, listed: map[int64]bool{}}
	if !c.scanWindows() {
		return s
	}
	for _, runner := range sortedKeys(c.targets) {
		w, ok := c.targets[runner]
		if !ok || c.listed[w.runID] {
			continue
		}
		if !c.listJobs(w.runID) {
			return s
		}
	}
	return s
}

// scanWindows lists the workflow runs that may hold a target's job and
// their jobs. It is false when the search must end.
func (c *lostScan) scanWindows() bool {
	var start, end time.Time
	for _, w := range c.targets {
		if start.IsZero() || w.start.Before(start) {
			start = w.start
		}
		if w.end.After(end) {
			end = w.end
		}
	}
	oldest := start.Add(-maxWorkflowLookback)
	for page := 1; page <= maxSearchRunPages; page++ {
		runs, err := c.p.r.gh.ListWorkflowRuns(c.p.ctx, c.v.ref, githubapp.WorkflowRunListOptions{
			PageOptions: githubapp.PageOptions{Page: page, PerPage: pageSize}, CreatedAtOrBefore: end,
		})
		if err != nil {
			c.s.err = err
			return false
		}
		pastLookback := len(runs.Runs) > 0
		for _, run := range runs.Runs {
			if !run.CreatedAt.Before(oldest) {
				pastLookback = false
			}
			if !mayHoldLost(run, c.targets) {
				continue
			}
			if !c.listJobs(run.ID) {
				return false
			}
			if len(c.targets) == 0 {
				return true
			}
		}
		if !runs.HasNext || pastLookback {
			return true
		}
	}
	c.s.err = errors.New("lost runner workflow search exceeded its page bound")
	return false
}

// mayHoldLost reports whether workflow run may have run a job on the runner
// of a loss in targets: created, or its latest attempt started, by the
// window's end and at most maxWorkflowLookback before its start, with a
// started job (not queued), and not completed before the window's start
// (updated_at is its completion).
func mayHoldLost(run githubapp.WorkflowRun, targets map[string]lostWindow) bool {
	status := ""
	if run.Status != nil {
		status = *run.Status
	}
	if status == "queued" {
		return false
	}
	for _, w := range targets {
		if !within(run.CreatedAt, w) && !within(run.RunStartedAt, w) {
			continue
		}
		if status == "completed" && run.UpdatedAt.Before(w.start) {
			continue
		}
		return true
	}
	return false
}

func within(at time.Time, w lostWindow) bool {
	return !at.IsZero() && !at.After(w.end) && !at.Before(w.start.Add(-maxWorkflowLookback))
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
