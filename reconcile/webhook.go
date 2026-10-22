package reconcile

import (
	"context"

	"github.com/marcioapm/lux-gha/webhook"
)

// Webhook returns the signed webhook handler. Deliveries are only signals:
// each pass re-reads job state from GitHub. Only a queued job a route serves
// wakes a pass; the tick serves the rest, which keeps passes from following
// every delivery. With discovery, a job from a repository outside the set
// and a change to the installation's repositories request a refresh.
func (r *Reconciler) Webhook(secret []byte) (*webhook.Handler, error) {
	queuedMatching := func(ev webhook.WorkflowJobEvent) bool {
		_, err := r.cfg.MatchRoute(ev.Labels)
		return ev.Action == "queued" && err == nil
	}
	h, err := webhook.New(secret, r.cfg.GitHub.InstallationID, r.AllowsRepository, func(_ context.Context, ev webhook.WorkflowJobEvent) error {
		r.log.Debug("workflow_job received", "delivery", ev.DeliveryID, "action", ev.Action, "repository", ev.Repository, "job", ev.JobID)
		if ev.Action == "in_progress" && ev.RunnerName != "" {
			r.noteInProgress(ev.Repository, ev.RunnerName, ev.JobID, ev.RunID)
		}
		if queuedMatching(ev) {
			r.NoteQueued(ev.Repository, ev.JobID)
			r.Wake()
		}
		return nil
	}, r.log)
	if err != nil || r.disc == nil {
		return h, err
	}
	h.OnUnlistedRepository = func(_ context.Context, ev webhook.WorkflowJobEvent) error {
		// Marked hot, the repository is read as soon as a refresh lists it.
		if queuedMatching(ev) {
			r.NoteQueued(ev.Repository, ev.JobID)
		}
		r.noteUnlisted(ev.Repository)
		r.RequestRepositoryRefresh()
		return nil
	}
	h.OnInstallation = func(_ context.Context, ev webhook.InstallationEvent) error {
		r.log.Info("installation changed; refreshing its repositories", "delivery", ev.DeliveryID, "event", ev.Event, "action", ev.Action)
		r.RequestRepositoryRefresh()
		return nil
	}
	return h, nil
}
