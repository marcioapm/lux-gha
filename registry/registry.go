// Package registry serves the runner listener: the two routes through which a
// Run's registry hook and docker credential helper obtain the in-region
// registry paths and short-lived AWS credentials for the job its runner is
// executing. Nothing a request carries is trusted beyond the runner name its
// identity token proves; the job, the workflow run and its trust level are
// read from Lux and GitHub.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"

	"github.com/marcioapm/lux-gha/config"
	"github.com/marcioapm/lux-gha/githubapp"
	"github.com/marcioapm/lux-gha/internal/identity"
)

const (
	RegistryPath    = "/runner/v1/registry"
	CredentialsPath = "/runner/v1/aws-credentials"
)

// Runners is the reconciler's view of this instance's runners.
type Runners interface {
	// ParseRunner returns the repository id of one of this instance's runner names.
	ParseRunner(name string) (repoID int64, ok bool)
	// LiveRun reports whether a live Lux Run of this instance carries the runner.
	LiveRun(ctx context.Context, runner string, repoID int64) (bool, error)
	// CandidateJob returns the job a pass or a signed delivery last put on
	// the runner; it is confirmed against GitHub before use.
	CandidateJob(runner string) (repository string, jobID int64, ok bool)
	// RunnerCompleted reports whether a signed workflow_job completed
	// delivery named the runner.
	RunnerCompleted(runner string) bool
	// RepositoryName returns owner/name for a repository id.
	RepositoryName(id int64) (string, bool)
	// SearchRunnerJobs lists the repository's in_progress runs, bounded,
	// and publishes every in_progress job on a runner of this instance as
	// that runner's candidate. A repository is searched at most once per
	// gap, and concurrent callers share one search. before runs just
	// before a search's first GitHub call; its error ends the search.
	SearchRunnerJobs(ctx context.Context, repo githubapp.Repository, repoID int64, before func() error) error
	// RateLimitedUntil is when GitHub's rate limit on the installation ends.
	RateLimitedUntil() time.Time
	// NoteRateLimit records err if it is a GitHub rate limit.
	NoteRateLimit(err error)
}

type GitHub interface {
	GetRepository(context.Context, githubapp.Repository) (githubapp.RepositoryInfo, error)
	GetWorkflowJob(context.Context, githubapp.Repository, int64) (githubapp.WorkflowJob, error)
	GetWorkflowRun(context.Context, githubapp.Repository, int64) (githubapp.WorkflowRun, error)
	TagCommit(ctx context.Context, repo githubapp.Repository, name string) (sha string, found bool, err error)
	BranchContains(ctx context.Context, repo githubapp.Repository, name, sha string) (bool, error)
}

type STS interface {
	AssumeRole(ctx context.Context, in *sts.AssumeRoleInput, optFns ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
}

type Options struct {
	Config         *config.Registry
	InstallationID int64
	IdentityKey    []byte
	Runners        Runners
	GitHub         GitHub
	STS            STS
	Now            func() time.Time
	Log            *slog.Logger
}

// Trust levels, also the lux-trust session tag values.
const (
	Trusted   = "trusted"
	Untrusted = "untrusted"
	ReadOnly  = "read-only"
)

const (
	// SessionDuration is STS's minimum. A session outlives its job by at
	// most this long; the credential helper asks again when it expires.
	SessionDuration = 15 * time.Minute
	// refreshBefore: cached credentials are minted again this long before expiry.
	refreshBefore = 5 * time.Minute
	// liveTTL and jobTTL bound how long a positive Lux liveness check and an
	// in_progress job read are reused; credential helpers call per pull.
	liveTTL = 30 * time.Second
	jobTTL  = 60 * time.Second
	// repositoryTTL bounds how long a repository's name and default branch
	// are reused.
	repositoryTTL = 10 * time.Minute
	// Token bucket per runner: burstTokens, refilled at refillPerSecond,
	// charged only by a request that calls Lux, GitHub or STS.
	burstTokens     = 30
	refillPerSecond = 0.5
	// idleForget drops a runner's state after this long without a request.
	idleForget  = 2 * time.Hour
	requestTime = 20 * time.Second
)

var (
	errUnauthorized = errors.New("unauthorized")
	errUnresolved   = errors.New("job not resolved")
	errUpstream     = errors.New("upstream unavailable")
	errLimited      = errors.New("runner over its request rate")
)

type Server struct {
	o       Options
	mu      sync.Mutex
	runners map[string]*runnerState
	repos   map[int64]cachedRepository
	refs    map[refKey]cachedRef
	pruned  time.Time
}

type cachedRepository struct {
	info githubapp.RepositoryInfo
	at   time.Time
}

// runnerState is what the server knows of one runner. A JIT runner runs at
// most one job, so the resolved job and its grant never change; finished
// marks a runner whose Run ended or whose job completed.
type runnerState struct {
	mu       sync.Mutex // serialises upstream calls for the runner
	tokens   float64    // guarded by Server.mu
	filledAt time.Time  // guarded by Server.mu: tokens' last refill
	lastSeen time.Time  // guarded by Server.mu
	liveAt   time.Time
	finished bool
	job      *githubapp.WorkflowJob
	jobAt    time.Time
	grant    grant
	creds    *ststypes.Credentials
}

// grant is what the executing job may do. owner and repo are the
// repository's lower-cased GitHub names: its registry path and the
// lux-owner and lux-repo session tags.
type grant struct {
	repoID      int64
	owner, repo string
	trust       string
}

func New(o Options) (*Server, error) {
	if o.Config == nil || len(o.IdentityKey) == 0 || o.Runners == nil || o.GitHub == nil || o.STS == nil || o.InstallationID <= 0 {
		return nil, errors.New("registry: config, identity key, runners, GitHub, STS and installation are required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Server{o: o, runners: map[string]*runnerState{}, repos: map[int64]cachedRepository{}, refs: map[refKey]cachedRef{}}, nil
}

// Handler serves only the two runner routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+RegistryPath, s.serveRegistry)
	mux.HandleFunc("POST "+CredentialsPath, s.serveCredentials)
	return mux
}

type registryResponse struct {
	Registry  string `json:"registry"`
	Read      string `json:"read"`
	Push      string `json:"push"`
	Trust     string `json:"trust"`
	ExpiresAt string `json:"expiresAt"`
}

// credentialProcess is the AWS credential_process output, version 1.
type credentialProcess struct {
	Version         int    `json:"Version"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken"`
	Expiration      string `json:"Expiration"`
}

func (s *Server) serveRegistry(w http.ResponseWriter, r *http.Request) {
	g, creds, ok := s.authorize(w, r)
	if !ok {
		return
	}
	c := s.o.Config
	read := c.Host + "/" + g.owner + "/" + g.repo
	push := ""
	switch g.trust {
	case Trusted:
		push = read
	case Untrusted:
		push = read + "/pr"
	}
	writeJSON(w, registryResponse{Registry: c.Host, Read: read, Push: push, Trust: g.trust,
		ExpiresAt: creds.Expiration.UTC().Format(time.RFC3339)})
}

func (s *Server) serveCredentials(w http.ResponseWriter, r *http.Request) {
	_, creds, ok := s.authorize(w, r)
	if !ok {
		return
	}
	writeJSON(w, credentialProcess{Version: 1, AccessKeyID: aws.ToString(creds.AccessKeyId), SecretAccessKey: aws.ToString(creds.SecretAccessKey),
		SessionToken: aws.ToString(creds.SessionToken), Expiration: creds.Expiration.UTC().Format(time.RFC3339)})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// authorize runs the whole chain for a request and answers every failure
// itself, with a status and no detail. ok reports that the caller may answer.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (grant, *ststypes.Credentials, bool) {
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	runner, err := identity.Verify(s.o.IdentityKey, strings.TrimSpace(token))
	if !found || err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return grant{}, nil, false
	}
	repoID, ok := s.o.Runners.ParseRunner(runner)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return grant{}, nil, false
	}
	st := s.state(runner)
	ctx, cancel := context.WithTimeout(r.Context(), requestTime)
	defer cancel()
	st.mu.Lock()
	defer st.mu.Unlock()
	rq := &request{s: s, st: st, runner: runner, log: s.o.Log.With("runner", runner)}
	g, err := rq.resolve(ctx, repoID)
	var creds *ststypes.Credentials
	if err == nil {
		creds, err = rq.credentials(ctx, g)
	}
	switch {
	case err == nil:
		return g, creds, true
	case errors.Is(err, errUnauthorized):
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case errors.Is(err, errUnresolved):
		http.Error(w, "job not resolved", http.StatusConflict)
	case errors.Is(err, errLimited):
		// The contract's codes only: the hook and the helper fail open on 503.
		w.Header().Set("Retry-After", "2")
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	default:
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}
	return grant{}, nil, false
}

// request is one request's pass through the chain for a runner whose
// st.mu it holds.
type request struct {
	s       *Server
	st      *runnerState
	runner  string
	log     *slog.Logger
	charged bool
}

// upstream charges the runner's bucket once per request, before its first
// call to Lux, GitHub or STS; answers from the runner's cache are free.
func (rq *request) upstream() error {
	if rq.charged {
		return nil
	}
	if !rq.s.take(rq.st) {
		return errLimited
	}
	rq.charged = true
	return nil
}

// github is upstream for a GitHub call: while GitHub rate-limits the
// installation the listener answers 503 without calling it.
func (rq *request) github() error {
	if rq.s.o.Now().Before(rq.s.o.Runners.RateLimitedUntil()) {
		return errUpstream
	}
	return rq.upstream()
}

// githubFailed logs a failed GitHub call and feeds a rate limit back to the
// reconciler.
func (rq *request) githubFailed(msg string, err error, args ...any) error {
	rq.s.o.Runners.NoteRateLimit(err)
	rq.log.Warn(msg, append(args, "error", err)...)
	return errUpstream
}

// state returns the runner's state, created on first use; it prunes idle
// runners and expired ref answers at most once a minute.
func (s *Server) state(runner string) *runnerState {
	now := s.o.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.pruned) > time.Minute {
		s.pruned = now
		for name, st := range s.runners {
			if now.Sub(st.lastSeen) > idleForget {
				delete(s.runners, name)
			}
		}
		for k, c := range s.refs {
			if now.Sub(c.at) >= repositoryTTL {
				delete(s.refs, k)
			}
		}
	}
	st, ok := s.runners[runner]
	if !ok {
		st = &runnerState{tokens: burstTokens, filledAt: now}
		s.runners[runner] = st
	}
	st.lastSeen = now
	return st
}

// take charges one request to the runner's token bucket.
func (s *Server) take(st *runnerState) bool {
	now := s.o.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st.tokens = min(burstTokens, st.tokens+now.Sub(st.filledAt).Seconds()*refillPerSecond)
	st.filledAt = now
	if st.tokens < 1 {
		return false
	}
	st.tokens--
	return true
}

// resolve checks the runner's Run is live, finds the job it is executing
// and that job's trust level.
func (rq *request) resolve(ctx context.Context, repoID int64) (grant, error) {
	s, st, runner, log := rq.s, rq.st, rq.runner, rq.log
	if st.finished {
		return grant{}, errUnauthorized
	}
	// A completed delivery ends the runner at once, inside the job re-read's
	// jobTTL and the liveness check's liveTTL.
	if s.o.Runners.RunnerCompleted(runner) {
		st.finished, st.creds = true, nil
		return grant{}, errUnauthorized
	}
	now := s.o.Now()
	if now.Sub(st.liveAt) >= liveTTL {
		if err := rq.upstream(); err != nil {
			return grant{}, err
		}
		live, err := s.o.Runners.LiveRun(ctx, runner, repoID)
		if err != nil {
			log.Warn("registry: reading the runner's Lux Run failed", "error", err)
			return grant{}, errUpstream
		}
		if !live {
			st.finished, st.creds = true, nil
			return grant{}, errUnauthorized
		}
		st.liveAt = now
	}
	full, ok := s.o.Runners.RepositoryName(repoID)
	if !ok {
		return grant{}, errUnresolved
	}
	owner, name, _ := strings.Cut(full, "/")
	repo := githubapp.Repository{InstallationID: s.o.InstallationID, Owner: owner, Name: name}
	if st.job != nil {
		if now.Sub(st.jobAt) < jobTTL {
			return st.grant, nil
		}
		if err := rq.github(); err != nil {
			return grant{}, err
		}
		j, err := s.o.GitHub.GetWorkflowJob(ctx, repo, st.job.ID)
		if err != nil {
			return grant{}, rq.githubFailed("registry: re-reading the runner's job failed", err, "job", st.job.ID)
		}
		if !onRunner(j, runner) || j.Status != "in_progress" {
			st.finished, st.creds = true, nil
			return grant{}, errUnauthorized
		}
		st.jobAt = now
		return st.grant, nil
	}
	j, err := rq.findJob(ctx, repo, repoID, full)
	if err != nil {
		return grant{}, err
	}
	// A JIT runner is registered on one repository, so the executing job's
	// repository is the runner's: its current names come from GET /repos.
	info, err := s.repository(ctx, repo, repoID)
	if err != nil {
		return grant{}, rq.githubFailed("registry: reading the runner's repository failed", err)
	}
	regOwner, regRepo, ok := registryNames(info.FullName)
	if !ok {
		// Refused for the rest of the runner's life, so logged once.
		st.finished = true
		log.Warn("registry: repository name has characters outside [a-z0-9._-]; no credentials", "repository", info.FullName, "job", j.ID)
		return grant{}, errUnauthorized
	}
	run, err := s.o.GitHub.GetWorkflowRun(ctx, repo, j.RunID)
	if err != nil {
		return grant{}, rq.githubFailed("registry: reading the job's workflow run failed", err, "job", j.ID)
	}
	trust, err := s.trust(ctx, repo, repoID, run)
	if err != nil {
		return grant{}, rq.githubFailed("registry: resolving the trust level failed", err, "job", j.ID)
	}
	st.job, st.jobAt = &j, now
	st.grant = grant{repoID: repoID, owner: regOwner, repo: regRepo, trust: trust}
	log.Info("registry: runner resolved", "repository", full, "job", j.ID, "workflowRun", j.RunID, "event", run.Event, "trust", trust)
	return st.grant, nil
}

func onRunner(j githubapp.WorkflowJob, runner string) bool {
	return j.RunnerName != nil && *j.RunnerName == runner
}

// findJob confirms the runner's candidate job against GitHub. Without one,
// or when GitHub contradicts it, it runs the repository's shared fallback
// search, which publishes candidates for every runner it sees, and confirms
// the runner's new candidate.
func (rq *request) findJob(ctx context.Context, repo githubapp.Repository, repoID int64, full string) (githubapp.WorkflowJob, error) {
	j, tried, ok, err := rq.confirmCandidate(ctx, repo, full)
	if ok || err != nil {
		return j, err
	}
	if err := rq.s.o.Runners.SearchRunnerJobs(ctx, repo, repoID, rq.github); err != nil {
		if errors.Is(err, errLimited) || errors.Is(err, errUpstream) {
			return githubapp.WorkflowJob{}, err
		}
		return githubapp.WorkflowJob{}, rq.githubFailed("registry: searching for the runner's job failed", err)
	}
	if _, jobID, ok := rq.s.o.Runners.CandidateJob(rq.runner); !ok || jobID == tried {
		return githubapp.WorkflowJob{}, errUnresolved
	}
	j, _, ok, err = rq.confirmCandidate(ctx, repo, full)
	if err == nil && !ok {
		err = errUnresolved
	}
	return j, err
}

// confirmCandidate reads the runner's candidate job. ok reports that GitHub
// shows it in_progress on this runner; a candidate it shows completed on
// this runner ends the runner. jobID is the candidate read, or 0.
func (rq *request) confirmCandidate(ctx context.Context, repo githubapp.Repository, full string) (j githubapp.WorkflowJob, jobID int64, ok bool, err error) {
	cand, jobID, found := rq.s.o.Runners.CandidateJob(rq.runner)
	if !found || !strings.EqualFold(cand, full) {
		return githubapp.WorkflowJob{}, 0, false, nil
	}
	if err := rq.github(); err != nil {
		return githubapp.WorkflowJob{}, jobID, false, err
	}
	j, err = rq.s.o.GitHub.GetWorkflowJob(ctx, repo, jobID)
	var apiErr *githubapp.APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound:
		return githubapp.WorkflowJob{}, jobID, false, nil
	case err != nil:
		return githubapp.WorkflowJob{}, jobID, false, rq.githubFailed("registry: reading the runner's candidate job failed", err, "job", jobID)
	case j.ID != jobID || !onRunner(j, rq.runner):
		return githubapp.WorkflowJob{}, jobID, false, nil
	case j.Status == "in_progress":
		return j, jobID, true, nil
	case j.Status == "completed":
		rq.st.finished, rq.st.creds = true, nil
		return githubapp.WorkflowJob{}, jobID, false, errUnauthorized
	}
	return githubapp.WorkflowJob{}, jobID, false, nil
}

// credentials returns the runner's cached session, or assumes the registry
// role for it.
func (rq *request) credentials(ctx context.Context, g grant) (*ststypes.Credentials, error) {
	s, st, log := rq.s, rq.st, rq.log
	if st.creds != nil && s.o.Now().Add(refreshBefore).Before(*st.creds.Expiration) {
		return st.creds, nil
	}
	policy, err := SessionPolicy(s.o.Config, g.owner, g.repo, g.trust)
	if err != nil {
		log.Warn("registry: building the session policy failed", "error", err)
		return nil, errUpstream
	}
	if err := rq.upstream(); err != nil {
		return nil, err
	}
	out, err := s.o.STS.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(s.o.Config.RoleARN),
		RoleSessionName: aws.String(SessionName(g.repoID, st.job.ID, st.job.RunAttempt)),
		DurationSeconds: aws.Int32(int32(SessionDuration / time.Second)),
		Tags: []ststypes.Tag{
			{Key: aws.String("lux-owner"), Value: aws.String(g.owner)},
			{Key: aws.String("lux-repo"), Value: aws.String(g.repo)},
			{Key: aws.String("lux-trust"), Value: aws.String(g.trust)},
		},
		Policy: aws.String(policy),
	})
	if err != nil {
		log.Warn("registry: assuming the registry role failed", "error", err)
		return nil, errUpstream
	}
	c := out.Credentials
	if c == nil || c.AccessKeyId == nil || c.SecretAccessKey == nil || c.SessionToken == nil || c.Expiration == nil {
		log.Warn("registry: STS returned incomplete credentials")
		return nil, errUpstream
	}
	st.creds = c
	log.Info("registry: minted registry credentials", "trust", g.trust, "expires", c.Expiration.UTC().Format(time.RFC3339))
	return c, nil
}

// SessionName is lux-gha-<repo id>-<job id>-<attempt>, cut to STS's 64 characters.
func SessionName(repoID, jobID, attempt int64) string {
	n := fmt.Sprintf("lux-gha-%d-%d-%d", repoID, jobID, attempt)
	return n[:min(len(n), 64)]
}

// registryComponent is the shape ECR requires of a repository name
// component: lower-case alphanumeric runs joined by single '.', '_' or '-'.
// A GitHub name outside it (".github", "a--b") could not be pushed to.
var registryComponent = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// registryNames lower-cases owner/name and reports whether both are
// registry path components.
func registryNames(full string) (owner, repo string, ok bool) {
	owner, repo, found := strings.Cut(strings.ToLower(full), "/")
	if !found || !registryComponent.MatchString(owner) || !registryComponent.MatchString(repo) {
		return "", "", false
	}
	return owner, repo, true
}
