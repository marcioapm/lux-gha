package reconcile

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/marcioapm/lux-gha/config"
	"github.com/marcioapm/lux-gha/githubapp"
	"github.com/marcioapm/lux-gha/luxclient"
)

// CheckName is the name of lux-gha's check runs; external_id is the Lux Run
// id, or job-<id> for a job Lux refused to create.
const CheckName = "lux-gha"

// luxDefaultDiskGiB is Lux's default resources.disk, which a route without
// diskGiB gets (an operator can change it on luxd).
const luxDefaultDiskGiB = 20

// closeStopped runs last in a pass, on the in_progress jobs listed this
// pass whose runner is this instance's and whose Lux Run has ended: it
// posts a check run saying why, and force-cancels a job still stuck
// stuckJobGrace after the Run finished.
func (p *pass) closeStopped() error {
	r := p.r
	observed := map[string]bool{}
	for _, v := range p.repos {
		for _, j := range v.stuck {
			name := *j.RunnerName
			if p.liveRunner[name] {
				continue
			}
			s, ok := r.stopped[name]
			if !ok {
				// Cached jobs later in the listing still need no read.
				if p.luxReads <= 0 {
					p.truncated = true
					continue
				}
				p.luxReads--
				run, err := p.endedRun(v, name)
				if err != nil {
					return err
				}
				if run == nil {
					continue
				}
				// A Run without finishedAt counts from when it was seen ended.
				s = &stoppedRun{run: *run, finished: r.now()}
				if run.FinishedAt != nil {
					s.finished = *run.FinishedAt
				}
				r.stopped[name] = s
			}
			observed[name] = true
			// A Run that exited 0 by itself ran its job to the end; GitHub's
			// job listing lags the runner's report.
			if strings.HasPrefix(s.run.StateReason, "succeeded") {
				continue
			}
			if err := p.explain(v, j, s); err != nil {
				return err
			}
			if err := p.unstick(v, j, s); err != nil {
				return err
			}
		}
	}
	p.pruneStopped(observed)
	return nil
}

// stoppedRun is the ended Run of a runner whose job is still in_progress,
// and what was done about it. A resumePolicy never Run stays ended.
type stoppedRun struct {
	run                  luxclient.Run
	finished             time.Time
	explained, cancelled bool
}

// endedRun reads the Runs labelled with the runner's name, in any state:
// one per runner name, so the listing is bounded. It returns nil when one
// is live (the runner may yet report) or there is none of this repository.
func (p *pass) endedRun(v *repoView, name string) (*luxclient.Run, error) {
	r := p.r
	runs, err := r.lux.List(p.ctx, map[string]string{LabelManaged: r.cfg.Instance, LabelRunner: name})
	if err != nil {
		r.log.Error("listing the Runs of a busy runner failed", "repository", v.full, "runner", name, "error", err)
		p.setError(ErrLux)
		return nil, errAbort
	}
	repoID := strconv.FormatInt(v.id, 10)
	var ended *luxclient.Run
	for i := range runs {
		if runs[i].Labels[LabelRepo] != repoID {
			continue
		}
		if slices.Contains(LiveStates, runs[i].State) {
			return nil, nil
		}
		// Newest first; one per runner name unless a key was reused.
		if ended == nil {
			ended = &runs[i]
		}
	}
	return ended, nil
}

// explain posts the job's check run once per Run. Whether one was posted is
// read from GitHub (a lux-gha check run on the job's commit whose
// external_id is the Run id), so a restart does not post it again.
func (p *pass) explain(v *repoView, j githubapp.WorkflowJob, s *stoppedRun) error {
	r, run := p.r, s.run
	if s.explained || p.noChecks {
		return nil
	}
	log := r.log.With("repository", v.full, "job", j.ID, "runner", *j.RunnerName, "run", run.ID, "reason", run.StateReason)
	done, err := p.postCheck(v, r.checkRun(j, s), log)
	s.explained = done
	return err
}

// postCheck posts req unless a lux-gha check run with its external_id is
// already on its commit. It reports whether that is settled; Checks denied
// or a GitHub error leaves it for a later pass, and a rate limit aborts.
func (p *pass) postCheck(v *repoView, req githubapp.CheckRunRequest, log *slog.Logger) (bool, error) {
	posted, err := p.checkPosted(v, req.HeadSHA, req.ExternalID)
	if err == nil && !posted {
		_, err = p.r.gh.CreateCheckRun(p.ctx, v.ref, req)
		if err == nil {
			p.st.CheckRuns++
			log.Info("posted a check run", "title", req.Output.Title)
		}
	}
	if p.rateLimited(err) {
		return false, errAbort
	}
	if p.denied(err, &p.noChecks, "Checks") {
		return false, nil
	}
	if err != nil {
		log.Error("posting a check run failed", "error", err)
		p.setError(ErrGitHub)
		return false, nil
	}
	return true, nil
}

// checkPosted pages the commit's lux-gha check runs for one whose
// external_id is externalID. A listing past the page bound counts as posted:
// a duplicate would be worse than a missing explanation.
func (p *pass) checkPosted(v *repoView, sha, externalID string) (bool, error) {
	for page := 1; page <= maxRunPages; page++ {
		cp, err := p.r.gh.ListCheckRunsForRef(p.ctx, v.ref, sha, githubapp.CheckRunListOptions{
			PageOptions: githubapp.PageOptions{Page: page, PerPage: pageSize}, CheckName: CheckName})
		if err != nil {
			return false, err
		}
		for _, c := range cp.CheckRuns {
			if c.ExternalID == externalID {
				return true, nil
			}
		}
		if !cp.HasNext {
			return false, nil
		}
	}
	p.r.log.Warn("check run listing exceeds page bound; not posting", "repository", v.full, "sha", sha, "externalID", externalID)
	return true, nil
}

// unstick force-cancels the job's workflow run once the job has stayed
// in_progress stuckJobGrace past its Run's end: the runner did not cancel
// it (SIGKILL, a lost host), so GitHub would wait out the job's timeout.
func (p *pass) unstick(v *repoView, j githubapp.WorkflowJob, s *stoppedRun) error {
	r := p.r
	if s.cancelled || p.noCancel {
		return nil
	}
	now := r.now()
	if now.Sub(s.finished) < r.cfg.Reconcile.StuckJobGrace.Duration {
		return nil
	}
	run := s.run
	log := r.log.With("repository", v.full, "workflowRun", j.RunID, "job", j.ID, "runner", *j.RunnerName, "run", run.ID,
		"reason", run.StateReason, "stuckFor", now.Sub(s.finished).Round(time.Second))
	done, err := p.cancelRun(v, j.RunID, log)
	s.cancelled = done
	return err
}

// cancelRun force-cancels workflow run runID and reports whether that is
// settled (cancelled, or already completed). It is held back while the pass
// did not list all of the run's jobs, or one of them is running on a runner
// of this instance with a live Run. A queued sibling has no runner yet, so its
// idle Run does not hold the cancel back; it is cancelled before it starts and
// its Run is reclaimed. Actions denied or a GitHub error leaves it for a later
// pass, and a rate limit aborts.
func (p *pass) cancelRun(v *repoView, runID int64, log *slog.Logger) (bool, error) {
	if v.partial[runID] {
		log.Info("not force-cancelling: the workflow run's jobs were not all listed this pass")
		return false, nil
	}
	for _, sibling := range v.runRunners[runID] {
		if p.liveRunner[sibling] {
			log.Info("not force-cancelling: another job of the workflow run holds a runner with a live Run", "sibling", sibling)
			return false, nil
		}
	}
	err := p.r.gh.ForceCancelWorkflowRun(p.ctx, v.ref, runID)
	if p.rateLimited(err) {
		return false, errAbort
	}
	if p.denied(err, &p.noCancel, "Actions") {
		return false, nil
	}
	var apiErr *githubapp.APIError
	switch {
	case err == nil:
		p.st.ForceCancelled++
		log.Warn("force-cancelled the workflow run")
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict:
		log.Info("workflow run already completed; nothing to force-cancel")
	default:
		log.Error("force-cancelling a workflow run failed", "error", err)
		p.setError(ErrGitHub)
		return false, nil
	}
	return true, nil
}

// denied reports whether err is a 403 that is not a rate limit: the App
// lacks permission (Checks or Actions write). The first one in a pass is
// logged and recorded; *flag then skips further calls needing it this pass.
func (p *pass) denied(err error, flag *bool, permission string) bool {
	var apiErr *githubapp.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || apiErr.RateLimited {
		return false
	}
	if !*flag {
		p.r.log.Warn("GitHub refused a call for stopped jobs: grant the App this write permission", "permission", permission)
	}
	*flag = true
	p.setError(ErrPermission)
	return true
}

// pruneStopped forgets the runners of read repositories that no longer
// hold a stopped in_progress job. It runs only after a complete
// closeStopped: a truncated or aborted one did not see every job.
func (p *pass) pruneStopped(observed map[string]bool) {
	if p.truncated {
		return
	}
	var prefixes []string
	for _, v := range p.repos {
		prefixes = append(prefixes, p.r.runnerPrefix(v.id))
	}
	maps.DeleteFunc(p.r.stopped, func(name string, _ *stoppedRun) bool {
		return !observed[name] && slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(name, prefix) })
	})
}

func (r *Reconciler) checkRun(j githubapp.WorkflowJob, s *stoppedRun) githubapp.CheckRunRequest {
	run, finished := s.run, s.finished
	// A resumePolicy never Run's reason ends in "; resumePolicy never",
	// which says nothing about the stop.
	reason := strings.TrimSuffix(cmp.Or(run.StateReason, run.State), "; resumePolicy "+luxclient.ResumePolicyNever)
	var route *config.Route
	for i := range r.cfg.Routes {
		if r.cfg.Routes[i].Name == run.Labels[LabelRoute] {
			route = &r.cfg.Routes[i]
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Lux ended the Run that held this job's runner, so the runner could not finish the job.\n\n")
	fmt.Fprintf(&b, "| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Route | `%s` |\n", run.Labels[LabelRoute])
	fmt.Fprintf(&b, "| Lux Run | `%s` |\n", run.ID)
	fmt.Fprintf(&b, "| State | %s |\n", run.State)
	fmt.Fprintf(&b, "| Reason | %s |\n", reason)
	if run.ExitCode != nil {
		fmt.Fprintf(&b, "| Exit code | %d |\n", *run.ExitCode)
	}
	fmt.Fprintf(&b, "| Finished | %s |\n", finished.UTC().Format(time.RFC3339))
	if hint := r.hint(reason, route); hint != "" {
		fmt.Fprintf(&b, "\n%s\n", hint)
	}
	return githubapp.CheckRunRequest{
		Name: CheckName, HeadSHA: j.HeadSHA, ExternalID: run.ID, Status: "completed", Conclusion: "failure",
		CompletedAt: finished,
		Output: githubapp.CheckRunOutput{
			Title: cutRunes(`Lux stopped job "`+j.Name+`": `+reason, githubapp.MaxCheckTitle),
			// The reason is Lux's free text, so the summary has no fixed bound.
			Summary: cutRunes(b.String(), githubapp.MaxCheckSummary),
		},
	}
}

// hint says what to change for the stop reasons a job can cause or avoid;
// reason is Lux's stateReason without the resumePolicy suffix.
func (r *Reconciler) hint(reason string, route *config.Route) string {
	switch {
	case strings.HasPrefix(reason, "disk limit exceeded"):
		if route == nil {
			return "The job wrote more than its disk limit: reduce disk use."
		}
		if route.Size != "" {
			return fmt.Sprintf("The job wrote more than size `%s`'s disk: ask for a larger size in `runs-on` or reduce disk use.", route.Size)
		}
		if next := r.largerDisk(route); next != nil {
			return fmt.Sprintf("The job wrote more than the route's %d GiB disk: use a larger-disk profile such as `%s` (runs-on: `%s`, %d GiB) or reduce disk use.",
				diskGiB(route), next.Name, strings.Join(next.Labels, ", "), diskGiB(next))
		}
		return fmt.Sprintf("The job wrote more than the route's %d GiB disk, the largest configured: reduce disk use.", diskGiB(route))
	case strings.HasPrefix(reason, "timeout"):
		if route == nil {
			return "The Run reached its Lux timeout."
		}
		return fmt.Sprintf("The Run reached the route's Lux timeout (%s): shorten the job, or raise the route's timeout.", route.Timeout.Duration)
	case strings.Contains(reason, "preempt"), strings.Contains(reason, "drain"), strings.Contains(reason, "host lost"):
		return "The Run's host was taken away (preemption, drain or loss); re-run the job."
	case strings.HasPrefix(reason, "terminated"):
		return "The Run was terminated through the Lux API."
	}
	return ""
}

// largerDisk returns the raw route with the smallest disk larger than
// route's; a size route's disk is its pool's, not in the config.
func (r *Reconciler) largerDisk(route *config.Route) *config.Route {
	var best *config.Route
	for i := range r.cfg.Routes {
		c := &r.cfg.Routes[i]
		if c.Size == "" && diskGiB(c) > diskGiB(route) && (best == nil || diskGiB(c) < diskGiB(best)) {
			best = c
		}
	}
	return best
}

func diskGiB(route *config.Route) int64 {
	return cmp.Or(route.DiskGiB, luxDefaultDiskGiB)
}

// cutRunes returns at most n bytes of s, cut on a rune boundary.
func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
