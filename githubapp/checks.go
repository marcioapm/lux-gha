package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// CheckRunOutput is a check run's output, within MaxCheckTitle and
// MaxCheckSummary.
type CheckRunOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// CheckRunRequest creates a completed check run on HeadSHA.
type CheckRunRequest struct {
	Name        string         `json:"name"`
	HeadSHA     string         `json:"head_sha"`
	ExternalID  string         `json:"external_id,omitempty"`
	Status      string         `json:"status"`
	Conclusion  string         `json:"conclusion"`
	CompletedAt time.Time      `json:"completed_at"`
	Output      CheckRunOutput `json:"output"`
}

type CheckRun struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	HeadSHA    string  `json:"head_sha"`
	ExternalID string  `json:"external_id"`
	Status     string  `json:"status"`
	Conclusion *string `json:"conclusion"`
}

type CheckRunListOptions struct {
	PageOptions
	// CheckName filters by name (check_name); empty lists every check run.
	CheckName string
}

type CheckRunPage struct {
	TotalCount int        `json:"total_count"`
	CheckRuns  []CheckRun `json:"check_runs"`
	HasNext    bool       `json:"-"`
}

// MaxCheckSummary is GitHub's limit on a check run's output summary, in
// characters; MaxCheckTitle is this package's limit on its title, in bytes.
const (
	MaxCheckSummary = 65535
	MaxCheckTitle   = 255
)

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// CreateCheckRun is POST /repos/{owner}/{repo}/check-runs (Checks: write).
// Only completed check runs are created, so the request carries a
// conclusion and completion time.
func (c *Client) CreateCheckRun(ctx context.Context, repo Repository, req CheckRunRequest) (CheckRun, error) {
	var out CheckRun
	if req.Name == "" || !shaRe.MatchString(req.HeadSHA) || req.Status != "completed" || req.Conclusion == "" || req.CompletedAt.IsZero() {
		return out, errors.New("githubapp: check run requires name, head SHA, completed status, conclusion and completion time")
	}
	if req.Output.Title == "" || len(req.Output.Title) > MaxCheckTitle || len(req.Output.Summary) > MaxCheckSummary {
		return out, errors.New("githubapp: check run output requires a title within 255 bytes and a summary within 65535 characters")
	}
	path, err := repo.path("/check-runs")
	if err != nil {
		return out, err
	}
	_, err = c.installationRequest(ctx, repo.InstallationID, http.MethodPost, path, req, http.StatusCreated, &out)
	if err == nil && out.ID <= 0 {
		return CheckRun{}, errors.New("githubapp: invalid check run response")
	}
	return out, err
}

// ListCheckRunsForRef is GET /repos/{owner}/{repo}/commits/{ref}/check-runs
// with filter=all: the default, latest, keeps one check run per name, which
// would hide all but one of several with the same name on a commit.
func (c *Client) ListCheckRunsForRef(ctx context.Context, repo Repository, ref string, opts CheckRunListOptions) (CheckRunPage, error) {
	var out CheckRunPage
	if !shaRe.MatchString(ref) {
		return out, errors.New("githubapp: check runs are listed by commit SHA")
	}
	q, err := opts.PageOptions.query()
	if err != nil {
		return out, err
	}
	if opts.CheckName != "" {
		q.Set("check_name", opts.CheckName)
	}
	q.Set("filter", "all")
	out.HasNext, err = c.list(ctx, repo, "/commits/"+url.PathEscape(ref)+"/check-runs", q, &out)
	return out, err
}

// ForceCancelWorkflowRun is POST /repos/{owner}/{repo}/actions/runs/{id}/force-cancel
// (Actions: write): it cancels the run without waiting for its runners, so
// every unfinished job of the run ends cancelled. GitHub answers 409 for a
// run that is not in progress; that is returned as an *APIError.
func (c *Client) ForceCancelWorkflowRun(ctx context.Context, repo Repository, runID int64) error {
	if runID <= 0 {
		return errors.New("githubapp: run ID must be positive")
	}
	path, err := repo.path(fmt.Sprintf("/actions/runs/%d/force-cancel", runID))
	if err != nil {
		return err
	}
	_, err = c.installationRequest(ctx, repo.InstallationID, http.MethodPost, path, nil, http.StatusAccepted, nil)
	return err
}
