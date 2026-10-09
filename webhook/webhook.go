// Package webhook verifies GitHub webhook deliveries and forwards
// workflow_job events for allowed repositories to a callback.
//
// The callback runs inside the HTTP request and must be a fast signal: GitHub
// abandons deliveries that take longer than 10 seconds. A callback error
// returns 503, but GitHub does not redeliver failed or missed deliveries
// automatically; recovery is the reconciliation loop's job, not the handler's.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// MaxBodyBytes bounds the unauthenticated read before HMAC verification.
// workflow_job and ping payloads are a few KiB; larger events are not consumed.
const MaxBodyBytes = 1 << 20

// CallbackTimeout leaves headroom inside GitHub's 10-second delivery budget.
var CallbackTimeout = 8 * time.Second

// WorkflowJobEvent is the subset of the workflow_job payload the bridge reads.
// RunnerID and RunnerName describe whichever runner GitHub assigned to this job,
// which need not be a runner this bridge provisioned for it.
type WorkflowJobEvent struct {
	DeliveryID     string
	Action         string // queued, waiting, in_progress or completed
	InstallationID int64
	Repository     string // owner/name
	JobID          int64
	RunID          int64
	RunAttempt     int64
	Status         string
	Conclusion     string // set on completed; cancellation is "cancelled"
	Labels         []string
	RunnerID       int64
	RunnerName     string
}

type Handler struct {
	Secret         []byte
	InstallationID int64
	AllowRepo      func(fullName string) bool
	OnWorkflowJob  func(context.Context, WorkflowJobEvent) error
	Log            *slog.Logger
}

var knownActions = map[string]bool{"queued": true, "waiting": true, "in_progress": true, "completed": true}

func New(secret []byte, installationID int64, allowRepo func(string) bool, onJob func(context.Context, WorkflowJobEvent) error, log *slog.Logger) (*Handler, error) {
	if len(secret) == 0 || installationID <= 0 || allowRepo == nil || onJob == nil {
		return nil, errors.New("webhook: secret, installation id, repository filter and callback are required")
	}
	return &Handler{Secret: secret, InstallationID: installationID, AllowRepo: allowRepo, OnWorkflowJob: onJob, Log: log}, nil
}

type payload struct {
	Action       string `json:"action"`
	Installation *struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	WorkflowJob *struct {
		ID         int64    `json:"id"`
		RunID      int64    `json:"run_id"`
		RunAttempt int64    `json:"run_attempt"`
		Status     string   `json:"status"`
		Conclusion *string  `json:"conclusion"`
		Labels     []string `json:"labels"`
		RunnerID   *int64   `json:"runner_id"`
		RunnerName *string  `json:"runner_name"`
	} `json:"workflow_job"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusRequestEntityTooLarge)
		return
	}
	if !validSignature(h.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	delivery := r.Header.Get("X-GitHub-Delivery")
	log := h.logger().With("delivery", delivery, "event", event)
	if event == "ping" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if event != "workflow_job" {
		log.Debug("ignored event")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}
	if p.Installation == nil || p.Repository == nil || p.WorkflowJob == nil || p.WorkflowJob.ID <= 0 {
		http.Error(w, "incomplete workflow_job payload", http.StatusBadRequest)
		return
	}
	if p.Installation.ID != h.InstallationID || !h.AllowRepo(p.Repository.FullName) {
		log.Warn("ignored workflow_job from unconfigured source", "installation", p.Installation.ID, "repository", p.Repository.FullName)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !knownActions[p.Action] {
		log.Info("ignored unknown workflow_job action", "action", p.Action)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	j := p.WorkflowJob
	ev := WorkflowJobEvent{
		DeliveryID:     delivery,
		Action:         p.Action,
		InstallationID: p.Installation.ID,
		Repository:     p.Repository.FullName,
		JobID:          j.ID,
		RunID:          j.RunID,
		RunAttempt:     j.RunAttempt,
		Status:         j.Status,
		Labels:         j.Labels,
	}
	if j.Conclusion != nil {
		ev.Conclusion = *j.Conclusion
	}
	if j.RunnerID != nil {
		ev.RunnerID = *j.RunnerID
	}
	if j.RunnerName != nil {
		ev.RunnerName = *j.RunnerName
	}
	ctx, cancel := context.WithTimeout(r.Context(), CallbackTimeout)
	defer cancel()
	if err := h.OnWorkflowJob(ctx, ev); err != nil {
		// The callback error is logged by type only so wrapped secrets never reach logs or GitHub.
		log.Error("workflow_job callback failed", "job", ev.JobID, "action", ev.Action, "error_type", errorType(err))
		http.Error(w, "temporarily unable to process delivery", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) logger() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func validSignature(secret, body []byte, header string) bool {
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok || len(secret) == 0 {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// SafeError lets a callback expose a fixed, secret-free category in logs.
type SafeError interface{ SafeKind() string }

func errorType(err error) string {
	var s SafeError
	if errors.As(err, &s) {
		return s.SafeKind()
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "context"
	}
	return "unclassified"
}
