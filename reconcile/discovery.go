package reconcile

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marcioapm/lux-gha/config"
	"github.com/marcioapm/lux-gha/githubapp"
)

// maxInstallationRepoPages bounds GET /installation/repositories at 100 per page.
const maxInstallationRepoPages = 100

// discovery is the repository set of an installation with no configured
// repositories, and what the bridge knows of each repository's state.
type discovery struct {
	// Shared with webhook goroutines.
	mu        sync.Mutex
	requested bool            // refresh on the next pass
	members   map[string]bool // lower-case owner/name
	// marks: lower-case owner/name: job id: last queued matching delivery.
	// A mark bridges GitHub's lag between the delivery and the job showing
	// in the runs API; a read that lists the job drops it.
	marks map[string]map[int64]time.Time

	// Touched only by the pass goroutine.
	repos     []discovered // sorted by owner/name
	refreshed time.Time    // last successful refresh
	lastPass  time.Time
	credit    float64             // cold reads earned but not yet made
	attempted map[int64]time.Time // repo id: last selection for a read
	known     map[int64]bool      // repo id: read successfully since startup
	// Managed runners, and the routes of matching demand, seen at each
	// repository's last successful read. A failed or skipped read leaves
	// them as they were.
	runners map[int64][]githubapp.Runner
	demand  map[int64][]*config.Route
	// gone: repo id: when a repository with remembered state left the set.
	gone map[int64]time.Time
}

type discovered struct {
	full string
	id   int64
}

func newDiscovery() *discovery {
	return &discovery{members: map[string]bool{}, marks: map[string]map[int64]time.Time{}, attempted: map[int64]time.Time{}, known: map[int64]bool{},
		runners: map[int64][]githubapp.Runner{}, demand: map[int64][]*config.Route{}, gone: map[int64]time.Time{}}
}

// AllowsRepository is the webhook's repository filter: the configured list,
// or with discovery the set the last successful refresh returned.
func (r *Reconciler) AllowsRepository(fullName string) bool {
	if r.disc == nil {
		return r.cfg.AllowsRepository(fullName)
	}
	r.disc.mu.Lock()
	defer r.disc.mu.Unlock()
	return r.disc.members[strings.ToLower(fullName)]
}

// NoteQueued records a queued delivery for a job a route serves; with
// discovery the repository is then read every pass until a read lists the
// job, for at most hotWindow after its last delivery.
func (r *Reconciler) NoteQueued(fullName string, jobID int64) {
	if r.disc == nil {
		return
	}
	key := strings.ToLower(fullName)
	r.disc.mu.Lock()
	if r.disc.marks[key] == nil {
		r.disc.marks[key] = map[int64]time.Time{}
	}
	r.disc.marks[key][jobID] = r.now()
	r.disc.mu.Unlock()
}

// RequestRepositoryRefresh makes the next pass re-list the installation's
// repositories, and wakes it. It does nothing with configured repositories.
func (r *Reconciler) RequestRepositoryRefresh() {
	if r.disc == nil {
		return
	}
	r.disc.mu.Lock()
	r.disc.requested = true
	r.disc.mu.Unlock()
	r.Wake()
}

// refreshRepositories re-lists the installation's repositories when the
// set is older than repositoryRefresh or a refresh was requested. A failure
// keeps the last good set (empty before the first success) and retries on
// the next pass.
func (p *pass) refreshRepositories() error {
	r, d := p.r, p.r.disc
	now := r.now()
	d.mu.Lock()
	requested := d.requested
	d.requested = false
	d.mu.Unlock()
	if !requested && !d.refreshed.IsZero() && now.Sub(d.refreshed) < r.cfg.Reconcile.RepositoryRefresh.Duration {
		return nil
	}
	retry := func() {
		d.mu.Lock()
		d.requested = true
		d.mu.Unlock()
	}
	var repos []discovered
	// Offset paging can return a repository twice if the set changes
	// between pages.
	listed := map[int64]bool{}
	for page := 1; ; page++ {
		var rp githubapp.InstallationRepositoryPage
		err := errors.New("reconcile: installation repository listing exceeds page bound")
		if page <= maxInstallationRepoPages {
			rp, err = r.gh.ListInstallationRepositories(p.ctx, r.cfg.GitHub.InstallationID, githubapp.PageOptions{Page: page, PerPage: pageSize})
		}
		if p.rateLimited(err) {
			retry()
			return errAbort
		}
		if err != nil {
			r.log.Error("listing the installation's repositories failed; keeping the last set", "repositories", len(d.repos), "error", err)
			p.setError(ErrGitHub)
			retry()
			return nil
		}
		for _, repo := range rp.Repositories {
			owner, name, ok := strings.Cut(repo.FullName, "/")
			if repo.Archived || repo.Disabled || !ok || owner == "" || name == "" || listed[repo.ID] {
				continue
			}
			listed[repo.ID] = true
			repos = append(repos, discovered{full: repo.FullName, id: repo.ID})
		}
		if !rp.HasNext {
			break
		}
	}
	slices.SortFunc(repos, func(a, b discovered) int { return strings.Compare(strings.ToLower(a.full), strings.ToLower(b.full)) })
	members := make(map[string]bool, len(repos))
	for _, repo := range repos {
		members[strings.ToLower(repo.full)] = true
		r.repoIDs[repo.full] = repo.id
	}
	d.repos, d.refreshed = repos, now
	d.mu.Lock()
	d.members = members
	d.mu.Unlock()
	r.log.Debug("installation repositories refreshed", "repositories", len(repos))
	return nil
}

// forgetRemoved drops what is remembered of a repository idleGrace after it
// left the set. Until then its registrations keep holding cap slots: it
// can no longer be read, so they cannot be resolved or deleted, and
// idleGrace is how long orphan cleanup holds one in a readable repository.
func (p *pass) forgetRemoved() {
	r, d := p.r, p.r.disc
	now := r.now()
	present := make(map[int64]bool, len(d.repos))
	for _, repo := range d.repos {
		present[repo.id] = true
		delete(d.gone, repo.id)
	}
	for full, id := range r.repoIDs {
		if !present[id] {
			delete(r.repoIDs, full)
		}
	}
	for _, id := range sortedKeys(d.attempted) {
		if present[id] {
			continue
		}
		since, ok := d.gone[id]
		if !ok {
			d.gone[id] = now
			continue
		}
		if now.Sub(since) >= r.cfg.Reconcile.IdleGrace.Duration {
			delete(d.attempted, id)
			delete(d.known, id)
			delete(d.runners, id)
			delete(d.demand, id)
			delete(d.gone, id)
		}
	}
}

// selectRepositories returns this pass's reads: every hot repository, every
// repository not yet read successfully since startup, and the cold
// repositories selected longest ago, as many as keep each read at least
// once per coldSweep.
func (p *pass) selectRepositories() []string {
	r, d := p.r, p.r.disc
	rc := r.cfg.Reconcile
	now := r.now()
	liveRepo := map[string]bool{}
	for _, run := range p.live {
		liveRepo[run.Labels[LabelRepo]] = true
	}
	d.mu.Lock()
	for name, jobs := range d.marks {
		for job, at := range jobs {
			if now.Sub(at) >= rc.HotWindow.Duration {
				delete(jobs, job)
			}
		}
		if len(jobs) == 0 {
			delete(d.marks, name)
		}
	}
	marked := make(map[string]bool, len(d.marks))
	for name := range d.marks {
		marked[name] = true
	}
	d.mu.Unlock()

	var selected []string
	var cold []discovered
	hot, fresh := 0, 0
	for _, repo := range d.repos {
		switch {
		case liveRepo[strconv.FormatInt(repo.id, 10)] || len(d.runners[repo.id]) > 0 || p.servable(d.demand[repo.id]) || marked[strings.ToLower(repo.full)]:
			hot++
			selected = append(selected, repo.full)
		case !d.known[repo.id]:
			// It may hold a registration from before a restart whose
			// submission is still unresolved.
			fresh++
			selected = append(selected, repo.full)
		default:
			cold = append(cold, repo)
		}
	}
	sweep := rc.ColdSweep.Duration
	elapsed := rc.Interval.Duration
	if !d.lastPass.IsZero() {
		elapsed = min(max(now.Sub(d.lastPass), 0), sweep)
	}
	d.lastPass = now
	// Stable sort: equally stale repositories stay in name order.
	slices.SortStableFunc(cold, func(a, b discovered) int { return d.attempted[a.id].Compare(d.attempted[b.id]) })
	// A cold repository must be read before the tick that would leave it
	// unread past coldSweep, so the rotation completes within coldSweep
	// minus interval. Each pass earns its elapsed share of that; the
	// fraction carries over, so woken passes read no extra.
	n := len(cold)
	if period := sweep - rc.Interval.Duration; period > 0 {
		d.credit = min(d.credit+float64(len(cold))*float64(elapsed)/float64(period), float64(len(cold)))
		n = max(int(d.credit), 0)
	}
	for n < len(cold) && now.Sub(d.attempted[cold[n].id]) > sweep-rc.Interval.Duration {
		n++
	}
	// Reads forced ahead of the pace are taken from later passes.
	d.credit = max(d.credit-float64(n), -float64(len(cold)))
	for _, repo := range cold[:n] {
		selected = append(selected, repo.full)
	}
	r.log.Debug("repositories selected for this pass", "known", len(d.repos), "hot", hot, "new", fresh,
		"cold", len(cold), "coldRead", n, "skipped", len(cold)-n)
	return selected
}

// servable reports whether this pass could create a Run for demand on one
// of routes: the global cap and that route's cap have room. Demand the caps
// keep unserved does not keep its repository hot; during a backlog the hot
// set is then bounded by free capacity, and the pass after a slot frees
// reads every demand repository again.
func (p *pass) servable(routes []*config.Route) bool {
	if len(p.live) >= p.r.cfg.MaxRunners {
		return false
	}
	for _, route := range routes {
		if route.MaxRunners == 0 || p.liveRoute[route.Name] < route.MaxRunners {
			return true
		}
	}
	return false
}

// noteRead moves a selected repository to the back of the cold rotation,
// whether its read succeeded or not: a repository that keeps failing then
// holds creation back for one pass per coldSweep, not every pass. A 404 or
// 403 means the installation may have lost the repository, so the next pass
// re-lists the installation rather than wait for repositoryRefresh.
func (p *pass) noteRead(full string, err error) {
	d := p.r.disc
	if d == nil {
		return
	}
	id := p.r.repoIDs[full]
	_, before := d.attempted[id]
	d.attempted[id] = p.r.now()
	ok := err == nil
	if ok {
		d.known[id] = true
		return
	}
	if !d.known[id] && before {
		p.r.log.Warn("repository has never been read successfully; creation is blocked while it fails", "repository", full)
	}
	var apiErr *githubapp.APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusForbidden) {
		d.mu.Lock()
		d.requested = true
		d.mu.Unlock()
	}
}

// recordReads remembers, for each repository read this pass, whether it
// holds managed runners or matching demand, including runners this pass
// registered, and drops the marks of jobs the read listed. Called after the
// pass, aborted or not.
func (p *pass) recordReads() {
	d := p.r.disc
	for _, v := range p.repos {
		key := strings.ToLower(v.full)
		d.mu.Lock()
		for job := range d.marks[key] {
			if v.seen[job] {
				delete(d.marks[key], job)
			}
		}
		if len(d.marks[key]) == 0 {
			delete(d.marks, key)
		}
		d.mu.Unlock()
		runners := make([]githubapp.Runner, 0, len(v.runners))
		for _, name := range sortedKeys(v.runners) {
			runners = append(runners, v.runners[name])
		}
		if len(runners) > 0 {
			d.runners[v.id] = runners
		} else {
			delete(d.runners, v.id)
		}
		if len(v.demand) > 0 {
			routes := make([]*config.Route, 0, len(v.demand))
			for _, job := range sortedKeys(v.demand) {
				if !slices.Contains(routes, v.demand[job]) {
					routes = append(routes, v.demand[job])
				}
			}
			d.demand[v.id] = routes
		} else {
			delete(d.demand, v.id)
		}
	}
}

// holdUnreadRegistrations holds a cap slot, against the global cap and each
// route its labels carry, for every managed runner remembered in a
// repository not read this pass whose name no live Run carries: its
// submission may still commit. Hot repositories are read every pass, so
// this covers one whose read failed or that left the installation.
func (p *pass) holdUnreadRegistrations() {
	d := p.r.disc
	if d == nil {
		return
	}
	read := map[int64]bool{}
	for _, v := range p.repos {
		read[v.id] = true
	}
	liveRunner := map[string]bool{}
	for _, run := range p.live {
		liveRunner[run.Labels[LabelRunner]] = true
	}
	for _, id := range sortedKeys(d.runners) {
		if read[id] {
			continue
		}
		for _, rn := range d.runners[id] {
			if liveRunner[rn.Name] {
				continue
			}
			p.unresolved++
			for _, route := range p.r.routesOf(rn) {
				p.unresolvedRoute[route.Name]++
			}
		}
	}
}
