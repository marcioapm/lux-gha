package reconcile

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/marcioapm/lux-gha/config"
	"github.com/marcioapm/lux-gha/githubapp"
	"github.com/marcioapm/lux-gha/luxclient"
)

// CheckName is the name of the check run that explains a job whose Lux Run
// ended under it; its external_id is the Lux Run id.
const CheckName = "lux-gha"

// luxDefaultDiskGiB is Lux's default resources.disk, which a route without
// diskGiB gets (an operator can change it on luxd).
const luxDefaultDiskGiB = 20

// closeStopped handles in_progress jobs, listed this pass, whose runner is
// one of this instance's and whose Lux Run has ended: the runner died with
// the job and may never report it. It posts one failed check run per Run
// saying why Lux stopped it, and stuckJobGrace after the Run finished, if
// the job is still in_progress, force-cancels its workflow run, unless a
// job of that run holds a runner with a live Run or the run's jobs were not
// all listed. Reading a job's Run costs one unit of maxJobsPerPass; an
// ended Run is remembered, so a job costs it once. A 403 from either GitHub
// call is a missing App permission: logged once per pass, recorded as
// ErrPermission, and the pass goes on without it.
func (p *pass) closeStopped() error {
	r := p.r
	liveRunner := map[string]bool{}
	for _, run := range p.live {
		liveRunner[run.Labels[LabelRunner]] = true
	}
	observed := map[string]bool{}
	for _, v := range p.repos {
		for _, j := range v.stuck {
			name := *j.RunnerName
			if liveRunner[name] {
				continue
			}
			s, ok := r.stopped[name]
			if !ok {
				if p.budget <= 0 {
					p.truncated = true
					return nil
				}
				p.budget--
				run, err := p.endedRun(v, name)
				if err != nil {
					return err
				}
				if run == nil {
					continue
				}
				// A Run without finishedAt counts from when it was seen ended.
				s = stoppedRun{run: *run, finished: r.now()}
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
			if err := p.unstick(v, j, s, liveRunner); err != nil {
				return err
			}
		}
	}
	p.pruneStopped(observed)
	return nil
}

// stoppedRun is the ended Run of a runner whose job is still in_progress.
// A resumePolicy never Run never runs again, so it stays ended.
type stoppedRun struct {
	run      luxclient.Run
	finished time.Time
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
func (p *pass) explain(v *repoView, j githubapp.WorkflowJob, s stoppedRun) error {
	r, run := p.r, s.run
	name := *j.RunnerName
	if _, done := r.explained[name]; done || p.noChecks {
		return nil
	}
	log := r.log.With("repository", v.full, "job", j.ID, "runner", name, "run", run.ID)
	posted, err := p.checkPosted(v, j.HeadSHA, run.ID)
	if err == nil && !posted {
		_, err = r.gh.CreateCheckRun(p.ctx, v.ref, r.checkRun(j, s))
		if err == nil {
			p.st.CheckRuns++
			log.Info("posted a check run for a job whose Lux Run ended", "reason", run.StateReason)
		}
	}
	if p.rateLimited(err) {
		return errAbort
	}
	if p.denied(err, &p.noChecks, "Checks") {
		return nil
	}
	if err != nil {
		log.Error("posting the check run for a stopped job failed", "error", err)
		p.setError(ErrGitHub)
		return nil
	}
	r.explained[name] = r.now()
	return nil
}

// checkPosted pages the commit's lux-gha check runs for one whose
// external_id is runID. A listing past the page bound counts as posted:
// a duplicate would be worse than a missing explanation.
func (p *pass) checkPosted(v *repoView, sha, runID string) (bool, error) {
	for page := 1; page <= maxRunPages; page++ {
		cp, err := p.r.gh.ListCheckRunsForRef(p.ctx, v.ref, sha, githubapp.CheckRunListOptions{
			PageOptions: githubapp.PageOptions{Page: page, PerPage: pageSize}, CheckName: CheckName})
		if err != nil {
			return false, err
		}
		for _, c := range cp.CheckRuns {
			if c.ExternalID == runID {
				return true, nil
			}
		}
		if !cp.HasNext {
			return false, nil
		}
	}
	p.r.log.Warn("check run listing exceeds page bound; not posting", "repository", v.full, "sha", sha, "run", runID)
	return true, nil
}

// unstick force-cancels the job's workflow run once the job has stayed
// in_progress stuckJobGrace past its Run's end: the runner did not cancel
// it (SIGKILL, a lost host), so GitHub would wait out the job's timeout.
func (p *pass) unstick(v *repoView, j githubapp.WorkflowJob, s stoppedRun, liveRunner map[string]bool) error {
	r := p.r
	name := *j.RunnerName
	if _, done := r.cancelled[name]; done || p.noCancel {
		return nil
	}
	now := r.now()
	if now.Sub(s.finished) < r.cfg.Reconcile.StuckJobGrace.Duration {
		return nil
	}
	run := s.run
	log := r.log.With("repository", v.full, "workflowRun", j.RunID, "job", j.ID, "runner", name, "run", run.ID)
	if v.partial[j.RunID] {
		log.Info("not force-cancelling: the workflow run's jobs were not all listed this pass")
		return nil
	}
	for _, sibling := range v.runRunners[j.RunID] {
		if liveRunner[sibling] {
			log.Info("not force-cancelling: another job of the workflow run holds a runner with a live Run", "sibling", sibling)
			return nil
		}
	}
	err := r.gh.ForceCancelWorkflowRun(p.ctx, v.ref, j.RunID)
	if p.rateLimited(err) {
		return errAbort
	}
	if p.denied(err, &p.noCancel, "Actions") {
		return nil
	}
	var apiErr *githubapp.APIError
	switch {
	case err == nil:
		p.st.ForceCancelled++
		log.Warn("force-cancelled a workflow run whose job's Lux Run ended under it", "reason", run.StateReason,
			"stuckFor", now.Sub(s.finished).Round(time.Second))
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict:
		log.Info("workflow run already completed; nothing to force-cancel")
	default:
		log.Error("force-cancelling a workflow run failed", "error", err)
		p.setError(ErrGitHub)
		return nil
	}
	r.cancelled[name] = now
	return nil
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
	gone := func(name string) bool {
		return !observed[name] && slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(name, prefix) })
	}
	maps.DeleteFunc(p.r.stopped, func(name string, _ stoppedRun) bool { return gone(name) })
	maps.DeleteFunc(p.r.explained, func(name string, _ time.Time) bool { return gone(name) })
	maps.DeleteFunc(p.r.cancelled, func(name string, _ time.Time) bool { return gone(name) })
}

func (r *Reconciler) checkRun(j githubapp.WorkflowJob, s stoppedRun) githubapp.CheckRunRequest {
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
	fmt.Fprintf(&b, "| Reason | %s |\n", cmp.Or(run.StateReason, run.State))
	if run.ExitCode != nil {
		fmt.Fprintf(&b, "| Exit code | %d |\n", *run.ExitCode)
	}
	fmt.Fprintf(&b, "| Finished | %s |\n", finished.UTC().Format(time.RFC3339))
	if hint := r.hint(reason, route); hint != "" {
		fmt.Fprintf(&b, "\n%s\n", hint)
	}
	summary := b.String()
	if len(summary) > 65535 {
		summary = summary[:65535]
	}
	return githubapp.CheckRunRequest{
		Name: CheckName, HeadSHA: j.HeadSHA, ExternalID: run.ID, Status: "completed", Conclusion: "failure",
		CompletedAt: finished,
		Output: githubapp.CheckRunOutput{
			Title:   truncate(`Lux stopped job "`+j.Name+`": `+reason, 255),
			Summary: summary,
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

// largerDisk returns the route with the smallest disk larger than route's.
func (r *Reconciler) largerDisk(route *config.Route) *config.Route {
	var best *config.Route
	for i := range r.cfg.Routes {
		c := &r.cfg.Routes[i]
		if diskGiB(c) > diskGiB(route) && (best == nil || diskGiB(c) < diskGiB(best)) {
			best = c
		}
	}
	return best
}

func diskGiB(route *config.Route) int64 {
	return cmp.Or(route.DiskGiB, luxDefaultDiskGiB)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
