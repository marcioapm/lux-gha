// Package reconcile keeps one just-in-time runner per queued matching job.
//
// GitHub gives a queued job to any idle runner whose labels cover it, so a
// runner is never tied to the job that caused it: the reconciler only keeps
// supply (live Lux Runs) level with demand (queued jobs) and removes runners
// nobody needs. All state it acts on is re-read every pass from GitHub and
// from Lux Run labels; the in-memory timers only delay cleanup after a
// restart, they never cause an action on their own. A repository not read
// in a pass is unknown to it: nothing is created or removed there, and its
// live Runs still count against the caps.
package reconcile

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marcioapm/lux-gha/config"
	"github.com/marcioapm/lux-gha/githubapp"
	"github.com/marcioapm/lux-gha/luxclient"
)

// Run labels; they are the bridge's only persistent state.
const (
	LabelManaged = "lux-gha/managed"
	LabelRepo    = "lux-gha/repo"
	LabelRoute   = "lux-gha/route"
	LabelJob     = "lux-gha/job"
	LabelGen     = "lux-gha/gen"
	LabelRunner  = "lux-gha/runner"
)

// JITConfigPath is where Lux places the JIT config file secret; JITConfigEnv
// names it for the entrypoint. Lux reserves the LUX_ env prefix.
const (
	JITConfigPath = "/run/lux-gha/jitconfig"
	JITConfigEnv  = "LUXGHA_JITCONFIG_FILE"
	jitSecretName = "jitconfig"
)

// LiveStates are the Lux Run states in which a runner may still start or run.
var LiveStates = []string{"submitted", "scheduled", "provisioning", "starting", "running", "stopping", "resuming"}

const (
	pageSize       = 100
	maxRunPages    = 10
	maxRunnerPages = 50
)

type GitHub interface {
	GetRepository(context.Context, githubapp.Repository) (githubapp.RepositoryInfo, error)
	ListWorkflowRuns(context.Context, githubapp.Repository, githubapp.WorkflowRunListOptions) (githubapp.WorkflowRunPage, error)
	ListWorkflowRunJobs(context.Context, githubapp.Repository, int64, githubapp.WorkflowJobListOptions) (githubapp.WorkflowJobPage, error)
	ListRepoRunners(context.Context, githubapp.Repository, githubapp.RunnerListOptions) (githubapp.RunnerPage, error)
	GenerateRepoJITConfig(context.Context, githubapp.Repository, githubapp.JITConfigRequest) (githubapp.JITConfig, error)
	DeleteRepoRunner(context.Context, githubapp.Repository, int64) error
	ListInstallationRepositories(context.Context, int64, githubapp.PageOptions) (githubapp.InstallationRepositoryPage, error)
	CreateCheckRun(context.Context, githubapp.Repository, githubapp.CheckRunRequest) (githubapp.CheckRun, error)
	ListCheckRunsForRef(context.Context, githubapp.Repository, string, githubapp.CheckRunListOptions) (githubapp.CheckRunPage, error)
	ForceCancelWorkflowRun(context.Context, githubapp.Repository, int64) error
}

type Lux interface {
	Create(ctx context.Context, idempotencyKey string, spec luxclient.RunSpec) (*luxclient.Run, error)
	Terminate(ctx context.Context, id string) (*luxclient.Run, error)
	List(ctx context.Context, labels map[string]string, states ...string) ([]luxclient.Run, error)
}

type Options struct {
	Config *config.Config
	GitHub GitHub
	Lux    Lux
	// Host is the GitHub API host, the first part of each Idempotency-Key.
	Host string
	Now  func() time.Time
	// After replaces time.After for the gap between passes.
	After func(time.Duration) <-chan time.Time
	Log   *slog.Logger
}

// Status is the last pass's outcome, served on GET /status. It holds counts
// and categories only, never credentials or JIT configs.
type Status struct {
	Time             time.Time      `json:"time"`
	DurationMS       int64          `json:"durationMs"`
	Error            string         `json:"error,omitempty"`
	RateLimitedUntil *time.Time     `json:"rateLimitedUntil,omitempty"`
	Demand           map[string]int `json:"demand"`
	Live             map[string]int `json:"live"`
	Idle             int            `json:"idle"`
	Busy             int            `json:"busy"`
	Provisioned      int            `json:"provisioned"`
	Terminated       int            `json:"terminated"`
	Deleted          int            `json:"deleted"`
	// CheckRuns and ForceCancelled count jobs whose Lux Run ended under them
	// (see closeStopped): check runs posted, workflow runs force-cancelled.
	CheckRuns      int `json:"checkRuns"`
	ForceCancelled int `json:"forceCancelled"`
}

// Error categories in Status.Error.
const (
	ErrRateLimited    = "github_rate_limited"
	ErrGitHub         = "github"
	ErrPermission     = "github_permission"
	ErrLux            = "lux"
	ErrNeverNotEchoed = "lux_resume_policy_not_echoed"
	ErrRunMismatch    = "lux_run_mismatch"
	ErrSizeRefused    = "lux_size_refused"
)

type Reconciler struct {
	cfg   *config.Config
	gh    GitHub
	lux   Lux
	host  string
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
	log   *slog.Logger
	wake  chan struct{}
	// passHook, set only by tests, observes Run entering and leaving a pass.
	passHook func(enter bool)
	// ticker starts Run's interval timer; tests replace it.
	ticker func(time.Duration) (<-chan time.Time, func())

	// Touched only by the pass goroutine.
	repoIDs          map[string]int64
	rateLimitedUntil time.Time
	surplusSince     map[string]time.Time   // runner name: idle while its trigger job is not queued
	missingSince     map[string]time.Time   // Run id: live Run whose runner is not registered
	offlineSince     map[string]time.Time   // runner name: registered runner with no live Run
	queuedTurn       bool                   // alternates the sole demand allowance when maxJobsPerPass is 1
	stopped          map[string]*stoppedRun // runner name: ended Run under an in_progress job
	refused          map[string]*refusal    // repo id/job id: queued job whose Run Lux refused for its size
	disc             *discovery             // nil with an explicit repository list

	mu     sync.Mutex
	status Status
}

func New(o Options) (*Reconciler, error) {
	if o.Config == nil || o.GitHub == nil || o.Lux == nil || o.Host == "" {
		return nil, errors.New("reconcile: config, GitHub, Lux and host are required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.After == nil {
		o.After = time.After
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	r := &Reconciler{
		cfg: o.Config, gh: o.GitHub, lux: o.Lux, host: o.Host, now: o.Now, after: o.After, log: o.Log,
		wake:         make(chan struct{}, 1),
		repoIDs:      map[string]int64{},
		surplusSince: map[string]time.Time{},
		missingSince: map[string]time.Time{},
		offlineSince: map[string]time.Time{},
		stopped:      map[string]*stoppedRun{},
		refused:      map[string]*refusal{},
		ticker: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
	}
	if o.Config.DiscoverRepositories() {
		r.disc = newDiscovery()
	}
	return r, nil
}

// Wake requests a pass without blocking; wakes before the next pass coalesce.
func (r *Reconciler) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run passes once immediately, then on every tick or wake until ctx ends. It
// is the only caller of runPass, which keeps passes serialized. A wake starts
// a pass no sooner than minGap after the previous pass started, which bounds
// the GitHub requests a stream of webhook deliveries can cause. The first
// pass is a tick pass, and so is a woken pass with a tick due when it starts.
func (r *Reconciler) Run(ctx context.Context) {
	interval := r.cfg.Reconcile.Interval.Duration
	minGap := max(5*time.Second, interval/12)
	tick, stop := r.ticker(interval)
	defer stop()
	woken := false
	for {
		start := r.now()
		// A wake that arrived before this pass starts is served by it; left
		// pending, it would cause a redundant pass minGap later.
		select {
		case <-r.wake:
		default:
		}
		r.observe(true)
		r.runPass(ctx, woken)
		r.observe(false)
		select {
		case <-ctx.Done():
			return
		case <-tick:
			woken = false
		case <-r.wake:
			woken = true
			if wait := start.Add(minGap).Sub(r.now()); wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-r.after(wait):
				}
			}
			// A tick buffered by now is served by this pass, as a tick.
			select {
			case <-tick:
				woken = false
			default:
			}
		}
	}
}

func (r *Reconciler) observe(enter bool) {
	if r.passHook != nil {
		r.passHook(enter)
	}
}

func (r *Reconciler) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func (r *Reconciler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	st := r.Status()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

// Pass runs one tick pass. It must not run concurrently with itself.
func (r *Reconciler) Pass(ctx context.Context) Status {
	return r.runPass(ctx, false)
}

// runPass runs one reconciliation. With discovery, a woken pass reads only
// the repositories with a delivery since the previous pass; with an explicit
// list, woken and tick passes are the same.
func (r *Reconciler) runPass(ctx context.Context, woken bool) Status {
	start := r.now()
	p := &pass{r: r, ctx: ctx, woken: woken, ended: map[string]bool{}, st: Status{Time: start, Demand: map[string]int{}, Live: map[string]int{}}}
	if start.Before(r.rateLimitedUntil) {
		p.st.Error = ErrRateLimited
	} else if err := p.run(); err != nil {
		p.fail(err)
	}
	if start.Before(r.rateLimitedUntil) {
		until := r.rateLimitedUntil
		p.st.RateLimitedUntil = &until
	}
	p.st.DurationMS = r.now().Sub(start).Milliseconds()
	r.mu.Lock()
	r.status = p.st
	r.mu.Unlock()
	return p.st
}

// errAbort ends a pass after a failure that makes every later step unsafe or
// futile (rate limit, Lux unreachable); the category is already recorded.
var errAbort = errors.New("reconcile: pass aborted")

type repoView struct {
	full    string
	ref     githubapp.Repository
	id      int64
	demand  map[int64]*config.Route
	meta    map[int64]jobMeta           // demand job id: what the listing said of it
	runners map[string]githubapp.Runner // managed runners by name
	seen    map[int64]bool              // every job id listed, any status
	// stuck: in_progress jobs on this instance's runners, candidates for
	// closeStopped. runRunners: workflow run id: the runner names its
	// listed jobs hold; partial: workflow runs whose job listing the
	// budget or page bound cut short.
	stuck      []githubapp.WorkflowJob
	runRunners map[int64][]string
	partial    map[int64]bool
}

type pass struct {
	// delivered: discovery mode, the delivered repositories this pass
	// selected; recordReads returns the unread ones for the next pass.
	delivered map[string]bool
	r         *Reconciler
	ctx       context.Context
	woken     bool
	st        Status
	repos     []*repoView
	truncated bool
	// budget: demand jobs this pass may still take on (maxJobsPerPass).
	// luxReads: Runs closeStopped may still read this pass (maxRunners);
	// newly ended busy runners are bounded by the previous pass's live Runs.
	budget, luxReads int
	statusBudget     [2]int
	// noChecks, noCancel: GitHub refused the Checks or Actions write
	// permission this pass; later calls needing it are skipped.
	noChecks, noCancel bool
	// unread: a repository's registrations are unknown this pass (its read
	// failed, or it has not been read since startup), so its unresolved
	// submissions are unknown and no Run may be created.
	unread     bool
	live       []luxclient.Run
	ended      map[string]bool // Run ids terminated this pass
	liveByJob  map[string]bool // repo id/job id
	liveRunner map[string]bool // runner name
	liveRoute  map[string]int
	// Cap slots held this pass by submissions with an unknown outcome, in
	// total and by route; their Runs may commit at any moment.
	unresolved      int
	unresolvedRoute map[string]int
}

func (p *pass) fail(err error) {
	if errors.Is(err, errAbort) {
		return
	}
	var apiErr *githubapp.APIError
	if errors.As(err, &apiErr) {
		if apiErr.RateLimited {
			p.r.rateLimitedUntil = apiErr.RetryAt
			p.st.Error = ErrRateLimited
			p.r.log.Warn("GitHub rate limit: pausing reconciliation", "until", apiErr.RetryAt)
			return
		}
		p.setError(ErrGitHub)
		return
	}
	p.setError(ErrLux)
}

func (p *pass) setError(cat string) {
	if p.st.Error == "" {
		p.st.Error = cat
	}
}

// rateLimited records err if it is a GitHub rate limit and reports whether it was.
func (p *pass) rateLimited(err error) bool {
	var apiErr *githubapp.APIError
	if errors.As(err, &apiErr) && apiErr.RateLimited {
		p.fail(err)
		return true
	}
	return false
}

func (p *pass) run() error {
	if p.r.disc != nil {
		return p.runDiscovered()
	}
	if err := p.readGitHub(p.r.cfg.Repositories); err != nil {
		return err
	}
	if err := p.readLux(); err != nil {
		return err
	}
	return p.act()
}

// runDiscovered reads Lux before GitHub: the live Runs decide which
// repositories are hot and must be read.
func (p *pass) runDiscovered() error {
	defer p.recordReads()
	if err := p.refreshRepositories(); err != nil {
		return err
	}
	p.forgetRemoved()
	if err := p.readLux(); err != nil {
		return err
	}
	if err := p.readGitHub(p.selectRepositories()); err != nil {
		return err
	}
	p.requireKnown()
	return p.act()
}

func (p *pass) act() error {
	if err := p.provision(); err != nil {
		return err
	}
	if err := p.removeSurplus(); err != nil {
		return err
	}
	if err := p.cleanUp(); err != nil {
		return err
	}
	if err := p.closeStopped(); err != nil {
		return err
	}
	return p.closeRefused()
}

// readGitHub reads each named repository. Every later step acts only on
// repositories read successfully here; any other is unknown this pass.
func (p *pass) readGitHub(repositories []string) error {
	n := p.r.cfg.Reconcile.MaxJobsPerPass
	p.statusBudget = [2]int{(n + 1) / 2, n / 2}
	if n == 1 {
		if p.r.queuedTurn {
			p.statusBudget = [2]int{0, 1}
		}
		p.r.queuedTurn = !p.r.queuedTurn
	}
	p.luxReads = p.r.cfg.MaxRunners
	for _, full := range repositories {
		owner, name, _ := strings.Cut(full, "/")
		v := &repoView{full: full, ref: githubapp.Repository{InstallationID: p.r.cfg.GitHub.InstallationID, Owner: owner, Name: name},
			demand: map[int64]*config.Route{}, meta: map[int64]jobMeta{}, runners: map[string]githubapp.Runner{}, seen: map[int64]bool{},
			runRunners: map[int64][]string{}, partial: map[int64]bool{}}
		err := p.readRepo(v)
		if p.rateLimited(err) {
			return errAbort
		}
		p.noteRead(full, err)
		if err != nil {
			// A partial view could hide registered runners; act on none of it.
			p.r.log.Error("reading repository failed", "repository", full, "error", err)
			p.setError(ErrGitHub)
			p.unread = true
			continue
		}
		p.repos = append(p.repos, v)
		for _, route := range v.demand {
			p.st.Demand[route.Name]++
		}
	}
	return nil
}

func (p *pass) readRepo(v *repoView) error {
	id, ok := p.r.repoIDs[v.full]
	if !ok {
		info, err := p.r.gh.GetRepository(p.ctx, v.ref)
		if err != nil {
			return err
		}
		id = info.ID
		p.r.repoIDs[v.full] = id
	}
	v.id = id
	if err := p.readDemand(v); err != nil {
		return err
	}
	prefix := p.r.runnerPrefix(v.id)
	for page := 1; ; page++ {
		if page > maxRunnerPages {
			return errors.New("reconcile: runner listing exceeds page bound")
		}
		rp, err := p.r.gh.ListRepoRunners(p.ctx, v.ref, githubapp.RunnerListOptions{PageOptions: githubapp.PageOptions{Page: page, PerPage: pageSize}})
		if err != nil {
			return err
		}
		for _, rn := range rp.Runners {
			if strings.HasPrefix(rn.Name, prefix) {
				v.runners[rn.Name] = rn
			}
		}
		if !rp.HasNext {
			break
		}
	}
	return nil
}

// readDemand reserves separate bounded demand portions for both statuses.
// in_progress is read first to expose stopped jobs even under queued backlog.
func (p *pass) readDemand(v *repoView) error {
	for i, status := range []string{"in_progress", "queued"} {
		p.budget = p.statusBudget[i]
		for page := 1; ; page++ {
			if page > maxRunPages || p.budget <= 0 {
				p.truncated = true
				break
			}
			runs, err := p.r.gh.ListWorkflowRuns(p.ctx, v.ref, githubapp.WorkflowRunListOptions{PageOptions: githubapp.PageOptions{Page: page, PerPage: pageSize}, Status: status})
			if err != nil {
				return err
			}
			for _, run := range runs.Runs {
				// Charged before the error check: a failed job page still spends it.
				err := p.readRunJobs(v, run)
				p.statusBudget[i] = p.budget
				if err != nil {
					return err
				}
			}
			if !runs.HasNext {
				break
			}
		}
		if p.budget == 0 {
			p.truncated = true
		}
	}
	return nil
}

func (p *pass) readRunJobs(v *repoView, run githubapp.WorkflowRun) error {
	runID := run.ID
	for page := 1; ; page++ {
		if page > maxRunPages || p.budget <= 0 {
			p.truncated = true
			v.partial[runID] = true
			return nil
		}
		jobs, err := p.r.gh.ListWorkflowRunJobs(p.ctx, v.ref, runID, githubapp.WorkflowJobListOptions{PageOptions: githubapp.PageOptions{Page: page, PerPage: pageSize}, Filter: "latest"})
		if err != nil {
			return err
		}
		for _, j := range jobs.Jobs {
			v.seen[j.ID] = true
			if j.RunnerName != nil && *j.RunnerName != "" {
				v.runRunners[runID] = append(v.runRunners[runID], *j.RunnerName)
				if j.Status == "in_progress" && p.r.managedRunner(v.id, *j.RunnerName) {
					v.stuck = append(v.stuck, j)
				}
			}
			// waiting (environment approval) and every other status are not demand.
			if j.Status != "queued" {
				continue
			}
			route, err := p.r.cfg.MatchRoute(j.Labels)
			if err != nil {
				continue
			}
			if _, seen := v.demand[j.ID]; !seen {
				if p.budget <= 0 {
					p.truncated = true
					v.partial[runID] = true
					return nil
				}
				p.budget--
				v.demand[j.ID] = route
				v.meta[j.ID] = newJobMeta(v.id, run, j)
			}
		}
		if !jobs.HasNext {
			return nil
		}
	}
}

func (p *pass) readLux() error {
	runs, err := p.r.lux.List(p.ctx, map[string]string{LabelManaged: p.r.cfg.Instance}, LiveStates...)
	if err != nil {
		p.r.log.Error("listing Lux Runs failed", "error", err)
		p.setError(ErrLux)
		return errAbort
	}
	p.live = runs
	p.liveByJob = map[string]bool{}
	p.liveRunner = map[string]bool{}
	p.liveRoute = map[string]int{}
	p.unresolvedRoute = map[string]int{}
	for _, run := range runs {
		p.liveByJob[run.Labels[LabelRepo]+"/"+run.Labels[LabelJob]] = true
		p.liveRunner[run.Labels[LabelRunner]] = true
		p.liveRoute[run.Labels[LabelRoute]]++
		p.st.Live[run.Labels[LabelRoute]]++
	}
	return nil
}

type provisionable struct {
	v     *repoView
	jobID int64
	route *config.Route
	gen   int
}

// provision classifies every unserved job, then every managed registration
// of a job outside that set, before creating any Run. The cap check for each
// creation thus counts every adopted Run and every unresolved submission,
// whether or not its job is still queued and within maxJobsPerPass.
func (p *pass) provision() error {
	cfg := p.r.cfg
	var todo []provisionable
	classified := map[string]bool{}
	for _, v := range p.repos {
		for _, jobID := range sortedKeys(v.demand) {
			route := v.demand[jobID]
			if p.liveByJob[jobKey(v, jobID)] || p.r.refused[jobKey(v, jobID)] != nil {
				continue
			}
			classified[jobKey(v, jobID)] = true
			run, gen, err := p.classifyJob(v, jobID, route)
			if err != nil {
				return err
			}
			if run != nil {
				p.addLive(v, jobID, *run)
			} else if gen >= 0 {
				todo = append(todo, provisionable{v, jobID, route, gen})
			}
		}
	}
	if err := p.classifyRegistrations(classified); err != nil {
		return err
	}
	p.holdUnreadRegistrations()
	if p.unread && len(todo) > 0 {
		// Its registrations, and so its unresolved submissions, are unknown:
		// any creation could exceed a cap once they commit.
		p.r.log.Warn("not provisioning: a repository could not be read", "jobs", len(todo))
		return nil
	}
	for _, j := range todo {
		if len(p.live)+p.unresolved >= cfg.MaxRunners ||
			(j.route.MaxRunners > 0 && p.liveRoute[j.route.Name]+p.unresolvedRoute[j.route.Name] >= j.route.MaxRunners) {
			continue
		}
		run, err := p.createRun(j)
		if err != nil {
			return err
		}
		if run != nil {
			p.addLive(j.v, j.jobID, *run)
			p.st.Provisioned++
		}
	}
	return nil
}

func jobKey(v *repoView, jobID int64) string {
	return strconv.FormatInt(v.id, 10) + "/" + strconv.FormatInt(jobID, 10)
}

func (r *Reconciler) jobLabels(v *repoView, jobID int64) map[string]string {
	return map[string]string{
		LabelManaged: r.cfg.Instance,
		LabelRepo:    strconv.FormatInt(v.id, 10),
		LabelJob:     strconv.FormatInt(jobID, 10),
	}
}

func (p *pass) readJobHistory(owned map[string]string, log *slog.Logger) ([]luxclient.Run, error) {
	history, err := p.r.lux.List(p.ctx, owned)
	if err != nil {
		log.Error("listing the job's Runs failed", "error", err)
		p.setError(ErrLux)
		return nil, errAbort
	}
	return history, nil
}

// addLive makes later steps of this pass see run like ones listed at the start.
func (p *pass) addLive(v *repoView, jobID int64, run luxclient.Run) {
	p.live = append(p.live, run)
	p.liveByJob[jobKey(v, jobID)] = true
	p.liveRunner[run.Labels[LabelRunner]] = true
	p.liveRoute[run.Labels[LabelRoute]]++
	p.st.Live[run.Labels[LabelRoute]]++
}

// classifyJob reads the job's Run history once. It returns the live Run to
// adopt, or the gen to submit next, or neither (gen -1) when the job waits
// on an unresolved submission or reached the gen cap; a non-nil error aborts
// the pass.
func (p *pass) classifyJob(v *repoView, jobID int64, route *config.Route) (*luxclient.Run, int, error) {
	r := p.r
	log := r.log.With("repository", v.full, "job", jobID, "route", route.Name)
	owned := r.jobLabels(v, jobID)
	jobPrefix := r.runnerPrefix(v.id) + owned[LabelJob] + "-"
	previous, err := p.readJobHistory(owned, log)
	if err != nil {
		return nil, -1, err
	}
	gen := 0
	hasRun := map[int]bool{}
	for _, run := range previous {
		n, err := strconv.Atoi(run.Labels[LabelGen])
		if err != nil {
			continue
		}
		hasRun[n] = true
		gen = max(gen, n+1)
		if slices.Contains(LiveStates, run.State) {
			// A submission whose outcome was unknown committed after the supply read.
			if !carries(run, owned, n, jobPrefix) {
				return nil, -1, p.mismatch(log, run)
			}
			log.Info("adopting a live Run for the job", "run", run.ID, "gen", n)
			return &run, -1, nil
		}
	}
	// A registered runner whose gen has no Run is a submission with an
	// unknown outcome: it may still be in flight. Its gen is not passed until
	// orphan cleanup deletes the runner, so at most one Run serves the job.
	// Until then it holds a cap slot, so its late Run cannot exceed a cap.
	for name := range v.runners {
		n, err := strconv.Atoi(strings.TrimPrefix(name, jobPrefix))
		if err == nil && strings.HasPrefix(name, jobPrefix) && !hasRun[n] {
			log.Info("runner registered without a Run; waiting for its Run or its cleanup", "runner", name)
			p.unresolved++
			p.unresolvedRoute[route.Name]++
			return nil, -1, nil
		}
	}
	if gen >= r.cfg.Reconcile.MaxGensPerJob {
		log.Warn("job reached the runner generation cap; not provisioning", "gens", gen)
		return nil, -1, nil
	}
	return nil, gen, nil
}

// classifyRegistrations accounts for managed runners of jobs not classified
// this pass (cancelled, picked elsewhere, or beyond maxJobsPerPass) whose
// name no live Run carries. With one history read per job, a gen with a live
// Run is adopted, a gen without any Run holds a cap slot (its submission may
// still commit), and a gen whose Run ended is left to orphan cleanup.
func (p *pass) classifyRegistrations(classified map[string]bool) error {
	r := p.r
	for _, v := range p.repos {
		gens := map[int64][]int{}
		for name := range v.runners {
			jobID, n, ok := r.parseRunnerName(v.id, name)
			if !ok || classified[jobKey(v, jobID)] || p.liveRunner[name] {
				continue
			}
			gens[jobID] = append(gens[jobID], n)
		}
		for _, jobID := range sortedKeys(gens) {
			if err := p.classifyJobRegistrations(v, jobID, gens[jobID]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *pass) classifyJobRegistrations(v *repoView, jobID int64, gens []int) error {
	r := p.r
	log := r.log.With("repository", v.full, "job", jobID)
	owned := r.jobLabels(v, jobID)
	jobPrefix := r.runnerPrefix(v.id) + owned[LabelJob] + "-"
	history, err := p.readJobHistory(owned, log)
	if err != nil {
		return err
	}
	slices.Sort(gens)
	for _, n := range gens {
		var live *luxclient.Run
		hasRun := false
		for _, run := range history {
			if run.Labels[LabelGen] != strconv.Itoa(n) {
				continue
			}
			hasRun = true
			if slices.Contains(LiveStates, run.State) {
				live = &run
				break
			}
		}
		name := jobPrefix + strconv.Itoa(n)
		switch {
		case live != nil:
			if !carries(*live, owned, n, jobPrefix) {
				return p.mismatch(log, *live)
			}
			log.Info("adopting a live Run for a runner whose job is not demand", "run", live.ID, "gen", n)
			p.addLive(v, jobID, *live)
		case !hasRun:
			log.Info("runner registered without a Run; holding a cap slot for it", "runner", name)
			p.unresolved++
			for _, route := range r.routesOf(v.runners[name]) {
				p.unresolvedRoute[route.Name]++
			}
		}
	}
	return nil
}

// routesOf returns every route whose labels the runner carries. The runner
// was registered with exactly its route's labels, but GitHub also reports its
// default ones (self-hosted, OS, architecture), so more than one route can
// match; the slot is held against each of them, never against none.
func (r *Reconciler) routesOf(rn githubapp.Runner) []*config.Route {
	have := make([]string, 0, len(rn.Labels))
	for _, l := range rn.Labels {
		have = append(have, strings.ToLower(l.Name))
	}
	var routes []*config.Route
	for i := range r.cfg.Routes {
		if containsAll(have, r.cfg.Routes[i].Labels) {
			routes = append(routes, &r.cfg.Routes[i])
		}
	}
	return routes
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, strings.ToLower(w)) {
			return false
		}
	}
	return true
}

// createRun registers j's runner and submits its Run. It returns the Run
// when it is live; a non-nil error aborts the pass.
func (p *pass) createRun(j provisionable) (*luxclient.Run, error) {
	r, v, route, gen := p.r, j.v, j.route, j.gen
	log := r.log.With("repository", v.full, "job", j.jobID, "route", route.Name)
	owned := r.jobLabels(v, j.jobID)
	repoID, job := owned[LabelRepo], owned[LabelJob]
	jobPrefix := r.runnerPrefix(v.id) + job + "-"
	name := jobPrefix + strconv.Itoa(gen)
	jit, err := r.gh.GenerateRepoJITConfig(p.ctx, v.ref, githubapp.JITConfigRequest{Name: name, RunnerGroupID: r.cfg.GitHub.RunnerGroupID, Labels: route.Labels})
	if p.rateLimited(err) {
		return nil, errAbort
	}
	var apiErr *githubapp.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
		// A runner with this name exists: a JIT from a pass that ended before Create.
		log.Warn("runner name already registered; deleting it", "runner", name)
		return nil, p.deleteRunnerByName(v, name)
	}
	if err != nil {
		log.Error("JIT config failed", "error", err)
		p.setError(ErrGitHub)
		return nil, nil
	}
	v.runners[name] = jit.Runner
	meta := v.meta[j.jobID]
	labels := metaLabels(v.full, meta)
	// Set last: the identity labels are never shadowed by a descriptive one.
	maps.Copy(labels, map[string]string{
		LabelManaged: r.cfg.Instance, LabelRepo: repoID, LabelRoute: route.Name,
		LabelJob: job, LabelGen: strconv.Itoa(gen), LabelRunner: name,
		LabelApp: AppName,
	})
	spec := luxclient.RunSpec{
		Name:               runName(v.full, j.jobID, meta),
		Labels:             labels,
		ImageRef:           route.Image,
		Command:            route.Command,
		Env:                map[string]string{JITConfigEnv: JITConfigPath},
		Secrets:            []luxclient.FileSecret{{Name: jitSecretName, Value: jit.EncodedJITConfig, Path: JITConfigPath}},
		Resources:          routeResources(route),
		Timeout:            route.Timeout.Duration,
		Grace:              r.cfg.Reconcile.StopGrace.Duration,
		Pool:               route.Pool,
		Sandbox:            luxclient.Sandbox{NestedContainers: route.NestedContainers},
		UnrestrictedEgress: route.UnrestrictedEgress,
	}
	for _, h := range route.EgressHosts {
		spec.Egress = append(spec.Egress, luxclient.EgressRule{Host: h})
	}
	run, err := r.lux.Create(p.ctx, fmt.Sprintf("%s/%s/%s/%s/%d", r.host, r.cfg.Instance, repoID, job, gen), spec)
	if run != nil && !carries(*run, owned, gen, jobPrefix) {
		// A replayed key answered with a Run this submission did not describe.
		return nil, p.mismatch(log, *run)
	}
	if errors.Is(err, luxclient.ErrResumePolicyNotEchoed) {
		log.Error("Lux did not echo resumePolicy never: terminating the Run and stopping provisioning", "run", run.ID)
		if _, terr := r.lux.Terminate(p.ctx, run.ID); terr != nil {
			log.Error("terminating the Run failed", "run", run.ID, "error", terr)
		} else {
			p.st.Terminated++
		}
		p.setError(ErrNeverNotEchoed)
		return nil, errAbort
	}
	if code, ok := sizeRefusal(err); ok {
		// A 4xx: no Run exists, and the same submission is refused again.
		return nil, p.refuse(j, code, name, jit.Runner.ID, log)
	}
	if err != nil {
		// The outcome is unknown: the Run may still appear. Until it does or
		// orphan cleanup deletes the runner, this job is not provisioned again.
		log.Error("Lux create failed; stopping provisioning", "error", err)
		p.setError(ErrLux)
		return nil, errAbort
	}
	if !slices.Contains(LiveStates, run.State) {
		// A replayed key answers with the earlier Run, which has ended.
		log.Warn("Lux returned an ended Run for a new runner", "run", run.ID, "state", run.State)
		return nil, nil
	}
	log.Info("provisioned runner", "runner", name, "gen", gen, "run", run.ID)
	return run, nil
}

// routeResources is a size route's size, or a raw route's cpus, memory and disk.
func routeResources(route *config.Route) luxclient.Resources {
	if route.Size != "" {
		return luxclient.Resources{Size: route.Size}
	}
	return luxclient.Resources{CPUs: route.CPUs, Memory: route.MemoryMiB << 20, Disk: route.DiskGiB << 30}
}

// carries reports whether run has the ownership labels and the gen and
// runner name a submission for gen would have given it.
func carries(run luxclient.Run, owned map[string]string, gen int, jobPrefix string) bool {
	for k, v := range owned {
		if run.Labels[k] != v {
			return false
		}
	}
	return run.Labels[LabelGen] == strconv.Itoa(gen) && run.Labels[LabelRunner] == jobPrefix+strconv.Itoa(gen)
}

// mismatch handles a Run that is not the one this instance would own: it is
// neither counted nor terminated, and provisioning stops for the pass.
func (p *pass) mismatch(log *slog.Logger, run luxclient.Run) error {
	log.Error("Lux returned a Run with unexpected labels; leaving it alone and stopping provisioning", "run", run.ID, "labels", run.Labels)
	p.setError(ErrRunMismatch)
	return errAbort
}

func (p *pass) deleteRunnerByName(v *repoView, name string) error {
	rp, err := p.r.gh.ListRepoRunners(p.ctx, v.ref, githubapp.RunnerListOptions{Name: name})
	if p.rateLimited(err) {
		return errAbort
	}
	if err != nil {
		p.setError(ErrGitHub)
		return nil
	}
	for _, rn := range rp.Runners {
		if rn.Name != name || rn.Busy {
			continue
		}
		err := p.r.gh.DeleteRepoRunner(p.ctx, v.ref, rn.ID)
		if p.rateLimited(err) {
			return errAbort
		}
		if err == nil {
			p.st.Deleted++
		}
	}
	return nil
}

// removeSurplus deletes idle runners beyond their repo and route's queued
// demand once their trigger job has not been queued for idleGrace. The runner
// is deleted first and a Run is terminated only after that succeeded, so a
// Run whose runner holds a job never is: GitHub answers DELETE on a runner
// assigned to a job with 422 "Runner ... is still running a job", as
// actions-runner-controller relies on (runner_graceful_stop.go).
func (p *pass) removeSurplus() error {
	r := p.r
	now := r.now()
	seen := map[string]bool{}
	for _, v := range p.repos {
		repoID := strconv.FormatInt(v.id, 10)
		idle := map[string]int{}
		type candidate struct {
			run    luxclient.Run
			runner githubapp.Runner
			since  time.Time
		}
		var candidates []candidate
		for _, run := range p.live {
			if run.Labels[LabelRepo] != repoID {
				continue
			}
			rn, ok := v.runners[run.Labels[LabelRunner]]
			if !ok {
				continue
			}
			if rn.Busy {
				p.st.Busy++
				continue
			}
			// A registered runner whose Run has not started yet (offline) is
			// supply too: left alone, a Run waiting for capacity for a
			// cancelled job would hold a runner slot indefinitely.
			if rn.Status == "online" {
				p.st.Idle++
			}
			idle[run.Labels[LabelRoute]]++
			job, _ := strconv.ParseInt(run.Labels[LabelJob], 10, 64)
			if _, queued := v.demand[job]; queued || p.truncated {
				continue
			}
			since, ok := r.surplusSince[rn.Name]
			if !ok {
				since = now
				r.surplusSince[rn.Name] = now
			}
			seen[rn.Name] = true
			candidates = append(candidates, candidate{run, rn, since})
		}
		demand := map[string]int{}
		for _, route := range v.demand {
			demand[route.Name]++
		}
		slices.SortFunc(candidates, func(a, b candidate) int { return a.since.Compare(b.since) })
		for _, c := range candidates {
			route := c.run.Labels[LabelRoute]
			if now.Sub(c.since) < r.cfg.Reconcile.IdleGrace.Duration || idle[route] <= demand[route] {
				continue
			}
			err := r.gh.DeleteRepoRunner(p.ctx, v.ref, c.runner.ID)
			if p.rateLimited(err) {
				return errAbort
			}
			if err != nil {
				// 422 for a runner that took a job meanwhile; any other error
				// leaves the runner and its Run alone too.
				r.log.Info("surplus runner not deleted; keeping its Run", "runner", c.runner.Name, "error", err)
				continue
			}
			p.st.Deleted++
			delete(v.runners, c.runner.Name)
			idle[route]--
			if _, err := r.lux.Terminate(p.ctx, c.run.ID); err != nil {
				r.log.Error("terminating surplus Run failed", "run", c.run.ID, "error", err)
				p.setError(ErrLux)
				continue
			}
			p.st.Terminated++
			p.ended[c.run.ID] = true
			r.log.Info("removed surplus runner", "runner", c.runner.Name, "run", c.run.ID)
		}
	}
	p.pruneTimers(r.surplusSince, seen, false)
	return nil
}

// cleanUp terminates live Runs whose runner has left GitHub (it ran its one
// job and deregistered) and deletes offline managed runners without a live
// Run (lost before pickup, or a submission whose outcome stayed unknown).
func (p *pass) cleanUp() error {
	r := p.r
	now := r.now()
	grace := r.cfg.Reconcile.IdleGrace.Duration
	missing, offline := map[string]bool{}, map[string]bool{}
	for _, v := range p.repos {
		repoID := strconv.FormatInt(v.id, 10)
		for _, run := range p.live {
			if run.Labels[LabelRepo] != repoID || p.ended[run.ID] {
				continue
			}
			if _, ok := v.runners[run.Labels[LabelRunner]]; ok {
				continue
			}
			missing[run.ID] = true
			since, ok := r.missingSince[run.ID]
			if !ok {
				r.missingSince[run.ID] = now
				continue
			}
			if now.Sub(since) < grace {
				continue
			}
			if _, err := r.lux.Terminate(p.ctx, run.ID); err != nil {
				r.log.Error("terminating finished Run failed", "run", run.ID, "error", err)
				p.setError(ErrLux)
				continue
			}
			p.st.Terminated++
			r.log.Info("terminated Run whose runner deregistered", "run", run.ID, "runner", run.Labels[LabelRunner])
		}
		for _, name := range sortedKeys(v.runners) {
			rn := v.runners[name]
			if p.liveRunner[name] || rn.Status != "offline" || rn.Busy {
				continue
			}
			offline[name] = true
			since, ok := r.offlineSince[name]
			if !ok {
				r.offlineSince[name] = now
				continue
			}
			if now.Sub(since) < grace {
				continue
			}
			err := r.gh.DeleteRepoRunner(p.ctx, v.ref, rn.ID)
			if p.rateLimited(err) {
				return errAbort
			}
			if err != nil {
				r.log.Info("orphan runner not deleted", "runner", name, "error", err)
				continue
			}
			p.st.Deleted++
			r.log.Info("deleted orphan runner", "runner", name)
		}
	}
	p.pruneTimers(r.missingSince, missing, true)
	p.pruneTimers(r.offlineSince, offline, false)
	return nil
}

func (r *Reconciler) runnerPrefix(repoID int64) string {
	return "lux-gha-" + r.cfg.Instance + "-" + strconv.FormatInt(repoID, 10) + "-"
}

// parseRunnerName parses lux-gha-<instance>-<repo id>-<job id>-<gen>. Only
// the canonical spelling is accepted ("-007-01" is not), so a name maps to
// one job and gen and back.
func (r *Reconciler) parseRunnerName(repoID int64, name string) (job int64, gen int, ok bool) {
	prefix := r.runnerPrefix(repoID)
	if !strings.HasPrefix(name, prefix) {
		return 0, 0, false
	}
	js, gs, ok := strings.Cut(name[len(prefix):], "-")
	job, jerr := strconv.ParseInt(js, 10, 64)
	gen, gerr := strconv.Atoi(gs)
	if !ok || jerr != nil || gerr != nil || gen < 0 || name != prefix+strconv.FormatInt(job, 10)+"-"+strconv.Itoa(gen) {
		return 0, 0, false
	}
	return job, gen, true
}

func (r *Reconciler) managedRunner(repoID int64, name string) bool {
	_, _, ok := r.parseRunnerName(repoID, name)
	return ok
}

func prune[V any](m map[string]V, keep map[string]bool) {
	for k := range m {
		if !keep[k] {
			delete(m, k)
		}
	}
}

func sortedKeys[K cmp.Ordered, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
