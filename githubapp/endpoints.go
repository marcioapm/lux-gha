package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Repository struct {
	InstallationID int64
	Owner, Name    string
}

type RunnerLabel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}
type Runner struct {
	ID            int64         `json:"id"`
	RunnerGroupID int64         `json:"runner_group_id"`
	Name          string        `json:"name"`
	OS            string        `json:"os"`
	Status        string        `json:"status"`
	Busy          bool          `json:"busy"`
	Labels        []RunnerLabel `json:"labels"`
	Ephemeral     bool          `json:"ephemeral"`
}

type JITConfigRequest struct {
	Name          string   `json:"name"`
	RunnerGroupID int64    `json:"runner_group_id"`
	Labels        []string `json:"labels"`
	WorkFolder    string   `json:"work_folder,omitempty"`
}

type JITConfig struct {
	Runner           Runner `json:"runner"`
	EncodedJITConfig string `json:"encoded_jit_config"`
}

func (j JITConfig) String() string {
	return fmt.Sprintf("JITConfig{RunnerID:%d EncodedJITConfig:<redacted>}", j.Runner.ID)
}
func (j JITConfig) GoString() string { return j.String() }

type WorkflowJob struct {
	ID            int64      `json:"id"`
	RunID         int64      `json:"run_id"`
	RunAttempt    int64      `json:"run_attempt"`
	HeadSHA       string     `json:"head_sha"`
	Status        string     `json:"status"`
	Conclusion    *string    `json:"conclusion"`
	Name          string     `json:"name"`
	Labels        []string   `json:"labels"`
	RunnerID      *int64     `json:"runner_id"`
	RunnerName    *string    `json:"runner_name"`
	RunnerGroupID *int64     `json:"runner_group_id"`
	WorkflowName  *string    `json:"workflow_name"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     time.Time  `json:"started_at"`
	CompletedAt   *time.Time `json:"completed_at"`
}

type WorkflowRun struct {
	ID         int64     `json:"id"`
	Name       *string   `json:"name"`
	HeadBranch *string   `json:"head_branch"`
	HeadSHA    string    `json:"head_sha"`
	RunNumber  int64     `json:"run_number"`
	RunAttempt int64     `json:"run_attempt"`
	Event      string    `json:"event"`
	Status     *string   `json:"status"`
	Conclusion *string   `json:"conclusion"`
	WorkflowID int64     `json:"workflow_id"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	// PullRequests lists the open pull requests whose head is this run's
	// head, whatever their base: it can hold a pull request into a fork, and
	// it is empty for a pull request from a fork.
	PullRequests   []PullRequestRef `json:"pull_requests"`
	HeadRepository *HeadRepository  `json:"head_repository"`
}

type PullRequestRef struct {
	Number int64           `json:"number"`
	Base   PullRequestBase `json:"base"`
}

type PullRequestBase struct {
	Repo struct {
		ID int64 `json:"id"`
	} `json:"repo"`
}

type HeadRepository struct {
	ID    int64 `json:"id"`
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// Page and PerPage default to GitHub's 1 and 30 when zero; PerPage is capped by GitHub at 100.
type PageOptions struct{ Page, PerPage int }
type RunnerListOptions struct {
	PageOptions
	Name string
}
type WorkflowRunListOptions struct {
	PageOptions
	Status string
}

// Attempt selects the per-attempt endpoint; Filter ("latest" or "all") applies only without Attempt.
type WorkflowJobListOptions struct {
	PageOptions
	Attempt int64
	Filter  string
}

type RunnerPage struct {
	TotalCount int      `json:"total_count"`
	Runners    []Runner `json:"runners"`
	HasNext    bool     `json:"-"`
}
type WorkflowRunPage struct {
	TotalCount int           `json:"total_count"`
	Runs       []WorkflowRun `json:"workflow_runs"`
	HasNext    bool          `json:"-"`
}
type WorkflowJobPage struct {
	TotalCount int           `json:"total_count"`
	Jobs       []WorkflowJob `json:"jobs"`
	HasNext    bool          `json:"-"`
}

func (c *Client) GenerateRepoJITConfig(ctx context.Context, repo Repository, req JITConfigRequest) (JITConfig, error) {
	var out JITConfig
	if req.Name == "" || req.RunnerGroupID <= 0 || len(req.Labels) < 1 || len(req.Labels) > 100 {
		return out, errors.New("githubapp: JIT config requires name, runner group and 1-100 labels")
	}
	for _, label := range req.Labels {
		if strings.TrimSpace(label) == "" {
			return out, errors.New("githubapp: JIT labels must be non-empty")
		}
	}
	path, err := repo.path("/actions/runners/generate-jitconfig")
	if err != nil {
		return out, err
	}
	// Not retried: a lost 201 has already registered a runner that reconciliation must delete.
	_, err = c.installationRequest(ctx, repo.InstallationID, http.MethodPost, path, req, http.StatusCreated, &out)
	if err == nil && (out.Runner.ID <= 0 || out.EncodedJITConfig == "") {
		return JITConfig{}, errors.New("githubapp: invalid JIT config response")
	}
	return out, err
}

// RepositoryInfo is the subset of GET /repos/{owner}/{repo} the bridge reads.
type RepositoryInfo struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

// GetRepository resolves the numeric repository ID, which unlike owner/name
// survives renames and transfers.
func (c *Client) GetRepository(ctx context.Context, repo Repository) (RepositoryInfo, error) {
	var out RepositoryInfo
	path, err := repo.path("")
	if err != nil {
		return out, err
	}
	_, err = c.installationRequest(ctx, repo.InstallationID, http.MethodGet, path, nil, http.StatusOK, &out)
	if err == nil && out.ID <= 0 {
		return RepositoryInfo{}, errors.New("githubapp: invalid repository response")
	}
	return out, err
}

func (c *Client) GetWorkflowJob(ctx context.Context, repo Repository, jobID int64) (WorkflowJob, error) {
	var out WorkflowJob
	if jobID <= 0 {
		return out, errors.New("githubapp: job ID must be positive")
	}
	path, err := repo.path(fmt.Sprintf("/actions/jobs/%d", jobID))
	if err != nil {
		return out, err
	}
	_, err = c.installationRequest(ctx, repo.InstallationID, http.MethodGet, path, nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) ListRepoRunners(ctx context.Context, repo Repository, opts RunnerListOptions) (RunnerPage, error) {
	var out RunnerPage
	q, err := opts.PageOptions.query()
	if err != nil {
		return out, err
	}
	if opts.Name != "" {
		q.Set("name", opts.Name)
	}
	out.HasNext, err = c.list(ctx, repo, "/actions/runners", q, &out)
	return out, err
}

// DeleteRepoRunner reports a missing runner as an *APIError (GitHub documents only 204 and 422).
func (c *Client) DeleteRepoRunner(ctx context.Context, repo Repository, runnerID int64) error {
	if runnerID <= 0 {
		return errors.New("githubapp: runner ID must be positive")
	}
	path, err := repo.path(fmt.Sprintf("/actions/runners/%d", runnerID))
	if err != nil {
		return err
	}
	_, err = c.installationRequest(ctx, repo.InstallationID, http.MethodDelete, path, nil, http.StatusNoContent, nil)
	return err
}

func (c *Client) ListWorkflowRuns(ctx context.Context, repo Repository, opts WorkflowRunListOptions) (WorkflowRunPage, error) {
	var out WorkflowRunPage
	q, err := opts.PageOptions.query()
	if err != nil {
		return out, err
	}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	out.HasNext, err = c.list(ctx, repo, "/actions/runs", q, &out)
	return out, err
}

func (c *Client) ListWorkflowRunJobs(ctx context.Context, repo Repository, runID int64, opts WorkflowJobListOptions) (WorkflowJobPage, error) {
	var out WorkflowJobPage
	if runID <= 0 || opts.Attempt < 0 {
		return out, errors.New("githubapp: run ID and attempt must be positive")
	}
	if opts.Filter != "" && opts.Filter != "latest" && opts.Filter != "all" {
		return out, errors.New("githubapp: job filter must be latest or all")
	}
	if opts.Attempt > 0 && opts.Filter != "" {
		return out, errors.New("githubapp: job filter is not supported with attempt")
	}
	q, err := opts.PageOptions.query()
	if err != nil {
		return out, err
	}
	suffix := fmt.Sprintf("/actions/runs/%d/jobs", runID)
	if opts.Attempt > 0 {
		suffix = fmt.Sprintf("/actions/runs/%d/attempts/%d/jobs", runID, opts.Attempt)
	} else if opts.Filter != "" {
		q.Set("filter", opts.Filter)
	}
	out.HasNext, err = c.list(ctx, repo, suffix, q, &out)
	return out, err
}

func (c *Client) list(ctx context.Context, repo Repository, suffix string, q url.Values, out any) (bool, error) {
	path, err := repo.path(suffix)
	if err != nil {
		return false, err
	}
	return c.listPath(ctx, repo.InstallationID, path, q, out)
}

func (c *Client) listPath(ctx context.Context, installationID int64, path string, q url.Values, out any) (bool, error) {
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	header, err := c.installationRequest(ctx, installationID, http.MethodGet, path, nil, http.StatusOK, out)
	if err != nil {
		return false, err
	}
	for _, link := range strings.Split(header.Get("Link"), ",") {
		if strings.Contains(link, `rel="next"`) {
			return true, nil
		}
	}
	return false, nil
}

// InstallationRepository is the subset of a repository in
// GET /installation/repositories the bridge reads.
type InstallationRepository struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	Archived      bool   `json:"archived"`
	Disabled      bool   `json:"disabled"`
	DefaultBranch string `json:"default_branch"`
}

type InstallationRepositoryPage struct {
	TotalCount   int                      `json:"total_count"`
	Repositories []InstallationRepository `json:"repositories"`
	HasNext      bool                     `json:"-"`
}

// ListInstallationRepositories lists one page of the repositories the
// installation can access, with the installation's own token.
func (c *Client) ListInstallationRepositories(ctx context.Context, installationID int64, opts PageOptions) (InstallationRepositoryPage, error) {
	var out InstallationRepositoryPage
	if installationID <= 0 {
		return out, errors.New("githubapp: installation ID must be positive")
	}
	q, err := opts.query()
	if err != nil {
		return out, err
	}
	out.HasNext, err = c.listPath(ctx, installationID, "/installation/repositories", q, &out)
	if err != nil {
		return InstallationRepositoryPage{}, err
	}
	for _, r := range out.Repositories {
		if r.ID <= 0 || r.FullName == "" {
			return InstallationRepositoryPage{}, errors.New("githubapp: invalid installation repository response")
		}
	}
	return out, nil
}

// TagCommit returns the commit SHA tag name points at, peeling an annotated
// tag (GET /repos/{o}/{r}/commits/refs/tags/{name} as application/vnd.github.sha).
// found is false when no such tag exists.
func (c *Client) TagCommit(ctx context.Context, repo Repository, name string) (sha string, found bool, err error) {
	suffix, err := refSuffix("/commits/refs/tags/", name)
	if err != nil {
		return "", false, err
	}
	path, err := repo.path(suffix)
	if err != nil {
		return "", false, err
	}
	var out shaResponse
	_, err = c.installationRequest(ctx, repo.InstallationID, http.MethodGet, path, nil, http.StatusOK, &out)
	var apiErr *APIError
	// GitHub answers 422 "No commit found" for a ref that does not exist.
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusUnprocessableEntity) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !shaPattern(out.sha) {
		return "", false, errors.New("githubapp: invalid commit SHA response")
	}
	return out.sha, true, nil
}

// BranchExists reports whether refs/heads/<name> exists
// (GET /repos/{o}/{r}/git/ref/heads/{name}, an exact match).
func (c *Client) BranchExists(ctx context.Context, repo Repository, name string) (bool, error) {
	suffix, err := refSuffix("/git/ref/heads/", name)
	if err != nil {
		return false, err
	}
	path, err := repo.path(suffix)
	if err != nil {
		return false, err
	}
	var out struct {
		Ref string `json:"ref"`
	}
	_, err = c.installationRequest(ctx, repo.InstallationID, http.MethodGet, path, nil, http.StatusOK, &out)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return out.Ref == "refs/heads/"+name, nil
}

// shaResponse, as request's out, asks for application/vnd.github.sha and
// receives the plain-text SHA.
type shaResponse struct{ sha string }

func shaPattern(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// refSuffix appends a ref name to prefix, escaping each '/'-separated
// segment. Names git refuses (empty segments, "." or "..") are rejected.
func refSuffix(prefix, name string) (string, error) {
	segs := strings.Split(name, "/")
	for i, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", errors.New("githubapp: invalid ref name")
		}
		segs[i] = url.PathEscape(s)
	}
	return prefix + strings.Join(segs, "/"), nil
}

func (o PageOptions) query() (url.Values, error) {
	if o.Page < 0 || o.PerPage < 0 || o.PerPage > 100 {
		return nil, errors.New("githubapp: invalid pagination")
	}
	q := url.Values{}
	if o.Page > 0 {
		q.Set("page", strconv.Itoa(o.Page))
	}
	if o.PerPage > 0 {
		q.Set("per_page", strconv.Itoa(o.PerPage))
	}
	return q, nil
}

func (r Repository) path(suffix string) (string, error) {
	if r.InstallationID <= 0 || !validSegment(r.Owner) || !validSegment(r.Name) {
		return "", errors.New("githubapp: invalid repository")
	}
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name) + suffix, nil
}

func validSegment(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 100 {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}
