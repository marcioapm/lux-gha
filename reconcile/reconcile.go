// Package reconcile keeps one just-in-time runner per queued matching job.
//
// GitHub gives a queued job to any idle runner whose labels cover it, so a
// runner is never tied to the job that caused it: the reconciler only keeps
// supply (live Lux Runs) level with demand (queued jobs) and removes runners
// nobody needs. All state it acts on is re-read every pass from GitHub and
// from Lux Run labels; the in-memory timers only delay cleanup after a
// restart, they never cause an action on their own.
package reconcile

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
}

// Error categories in Status.Error.
const (
	ErrRateLimited    = "github_rate_limited"
	ErrGitHub         = "github"
	ErrLux            = "lux"
	ErrNeverNotEchoed = "lux_resume_policy_not_echoed"
	ErrRunMismatch    = "lux_run_mismatch"
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

	// Touched only by the pass goroutine.
	repoIDs          map[string]int64
	rateLimitedUntil time.Time
	surplusSince     map[string]time.Time // runner name: idle while its trigger job is not queued
	missingSince     map[string]time.Time // Run id: live Run whose runner is not registered
	offlineSince     map[string]time.Time // runner name: registered runner with no live Run

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
	return &Reconciler{
		cfg: o.Config, gh: o.GitHub, lux: o.Lux, host: o.Host, now: o.Now, after: o.After, log: o.Log,
		wake:         make(chan struct{}, 1),
		repoIDs:      map[string]int64{},
		surplusSince: map[string]time.Time{},
		missingSince: map[string]time.Time{},
		offlineSince: map[string]time.Time{},
	}, nil
}

// Wake requests a pass without blocking; wakes before the next pass coalesce.
func (r *Reconciler) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run passes once immediately, then on every tick or wake until ctx ends. It
// is the only caller of Pass, which keeps passes serialized. A wake starts a
// pass no sooner than minGap after the previous pass started, which bounds
// the GitHub requests a stream of webhook deliveries can cause.
func (r *Reconciler) Run(ctx context.Context) {
	interval := r.cfg.Reconcile.Interval.Duration
	minGap := max(5*time.Second, interval/12)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		start := r.now()
		// A wake that arrived before this pass starts is served by it; left
		// pending, it would cause a redundant pass minGap later.
		select {
		case <-r.wake:
		default:
		}
		r.observe(true)
		r.Pass(ctx)
		r.observe(false)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.wake:
			if wait := start.Add(minGap).Sub(r.now()); wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-r.after(wait):
				}
				// The woken pass serves a tick buffered during the wait.
				select {
				case <-t.C:
				default:
				}
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

// Pass runs one reconciliation. It must not run concurrently with itself.
func (r *Reconciler) Pass(ctx context.Context) Status {
	start := r.now()
	p := &pass{r: r, ctx: ctx, ended: map[string]bool{}, st: Status{Time: start, Demand: map[string]int{}, Live: map[string]int{}}}
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
	runners map[string]githubapp.Runner // managed runners by name
}

type pass struct {
	r         *Reconciler
	ctx       context.Context
	st        Status
	repos     []*repoView
	truncated bool
	// unread: a configured repository could not be read this pass, so its
	// unresolved submissions are unknown and no Run may be created.
	unread    bool
	live      []luxclient.Run
	ended     map[string]bool // Run ids terminated this pass
	liveByJob map[string]bool // repo id/job id
	liveRoute map[string]int
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
	if err := p.readGitHub(); err != nil {
		return err
	}
	if err := p.readLux(); err != nil {
		return err
	}
	if err := p.provision(); err != nil {
		return err
	}
	if err := p.removeSurplus(); err != nil {
		return err
	}
	return p.cleanUp()
}

func (p *pass) readGitHub() error {
	budget := p.r.cfg.Reconcile.MaxJobsPerPass
	for _, full := range p.r.cfg.Repositories {
		owner, name, _ := strings.Cut(full, "/")
		v := &repoView{full: full, ref: githubapp.Repository{InstallationID: p.r.cfg.GitHub.InstallationID, Owner: owner, Name: name},
			demand: map[int64]*config.Route{}, runners: map[string]githubapp.Runner{}}
		err := p.readRepo(v, &budget)
		if p.rateLimited(err) {
			return errAbort
		}
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

func (p *pass) readRepo(v *repoView, budget *int) error {
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
	if err := p.readDemand(v, budget); err != nil {
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

// readDemand collects queued jobs that match a route. Queued jobs live in
// queued runs and, behind finished jobs, in in_progress runs.
func (p *pass) readDemand(v *repoView, budget *int) error {
	for _, status := range []string{"queued", "in_progress"} {
		for page := 1; ; page++ {
			if page > maxRunPages || *budget <= 0 {
				p.truncated = true
				return nil
			}
			runs, err := p.r.gh.ListWorkflowRuns(p.ctx, v.ref, githubapp.WorkflowRunListOptions{PageOptions: githubapp.PageOptions{Page: page, PerPage: pageSize}, Status: status})
			if err != nil {
				return err
			}
			for _, run := range runs.Runs {
				if err := p.readRunJobs(v, run.ID, budget); err != nil {
					return err
				}
			}
			if !runs.HasNext {
				break
			}
		}
	}
	return nil
}

func (p *pass) readRunJobs(v *repoView, runID int64, budget *int) error {
	for page := 1; ; page++ {
		if page > maxRunPages || *budget <= 0 {
			p.truncated = true
			return nil
		}
		jobs, err := p.r.gh.ListWorkflowRunJobs(p.ctx, v.ref, runID, githubapp.WorkflowJobListOptions{PageOptions: githubapp.PageOptions{Page: page, PerPage: pageSize}, Filter: "latest"})
		if err != nil {
			return err
		}
		for _, j := range jobs.Jobs {
			// waiting (environment approval) and every other status are not demand.
			if j.Status != "queued" {
				continue
			}
			route, err := p.r.cfg.MatchRoute(j.Labels)
			if err != nil {
				continue
			}
			if _, seen := v.demand[j.ID]; !seen {
				if *budget <= 0 {
					p.truncated = true
					return nil
				}
				*budget--
				v.demand[j.ID] = route
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
	p.liveRoute = map[string]int{}
	p.unresolvedRoute = map[string]int{}
	for _, run := range runs {
		p.liveByJob[run.Labels[LabelRepo]+"/"+run.Labels[LabelJob]] = true
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
			if p.liveByJob[jobKey(v, jobID)] {
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

// addLive makes later steps of this pass see run like ones listed at the start.
func (p *pass) addLive(v *repoView, jobID int64, run luxclient.Run) {
	p.live = append(p.live, run)
	p.liveByJob[jobKey(v, jobID)] = true
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
	repoID, job := strconv.FormatInt(v.id, 10), strconv.FormatInt(jobID, 10)
	jobPrefix := r.runnerPrefix(v.id) + job + "-"
	owned := map[string]string{LabelManaged: r.cfg.Instance, LabelRepo: repoID, LabelJob: job}
	previous, err := r.lux.List(p.ctx, owned)
	if err != nil {
		log.Error("listing the job's Runs failed", "error", err)
		p.setError(ErrLux)
		return nil, -1, errAbort
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
	liveRunner := map[string]bool{}
	for _, run := range p.live {
		liveRunner[run.Labels[LabelRunner]] = true
	}
	for _, v := range p.repos {
		prefix := r.runnerPrefix(v.id)
		gens := map[int64][]int{}
		for name := range v.runners {
			job, gen, ok := strings.Cut(strings.TrimPrefix(name, prefix), "-")
			jobID, jerr := strconv.ParseInt(job, 10, 64)
			n, gerr := strconv.Atoi(gen)
			if !ok || jerr != nil || gerr != nil || name != prefix+job+"-"+gen ||
				classified[jobKey(v, jobID)] || liveRunner[name] {
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
	repoID, job := strconv.FormatInt(v.id, 10), strconv.FormatInt(jobID, 10)
	jobPrefix := r.runnerPrefix(v.id) + job + "-"
	owned := map[string]string{LabelManaged: r.cfg.Instance, LabelRepo: repoID, LabelJob: job}
	history, err := r.lux.List(p.ctx, owned)
	if err != nil {
		log.Error("listing the job's Runs failed", "error", err)
		p.setError(ErrLux)
		return errAbort
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
	repoID, job := strconv.FormatInt(v.id, 10), strconv.FormatInt(j.jobID, 10)
	jobPrefix := r.runnerPrefix(v.id) + job + "-"
	owned := map[string]string{LabelManaged: r.cfg.Instance, LabelRepo: repoID, LabelJob: job}
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
	spec := luxclient.RunSpec{
		Name: fmt.Sprintf("gha %s job %d", v.full, j.jobID),
		Labels: map[string]string{
			LabelManaged: r.cfg.Instance, LabelRepo: repoID, LabelRoute: route.Name,
			LabelJob: job, LabelGen: strconv.Itoa(gen), LabelRunner: name,
		},
		ImageRef:  route.Image,
		Command:   route.Command,
		Env:       map[string]string{JITConfigEnv: JITConfigPath},
		Secrets:   []luxclient.FileSecret{{Name: jitSecretName, Value: jit.EncodedJITConfig, Path: JITConfigPath}},
		Resources: luxclient.Resources{CPUs: route.CPUs, Memory: route.MemoryMiB << 20, Disk: route.DiskGiB << 30},
		Timeout:   route.Timeout.Duration,
		Pool:      route.Pool,
		Sandbox:   luxclient.Sandbox{NestedContainers: route.NestedContainers},
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
	prune(r.surplusSince, seen)
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
	liveRunner := map[string]bool{}
	for _, run := range p.live {
		liveRunner[run.Labels[LabelRunner]] = true
	}
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
			if liveRunner[name] || rn.Status != "offline" || rn.Busy {
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
	prune(r.missingSince, missing)
	prune(r.offlineSince, offline)
	return nil
}

func (r *Reconciler) runnerPrefix(repoID int64) string {
	return "lux-gha-" + r.cfg.Instance + "-" + strconv.FormatInt(repoID, 10) + "-"
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
