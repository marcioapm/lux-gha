package reconcile

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux-gha/githubapp"
	"github.com/marcioapm/lux-gha/luxclient"
)

const hookSecret = "discovery-hook-secret"

// newDiscoveryEnv is newEnv without a repository list, plus acme/r00 ..
// acme/r<n-1> (ids 2000+i) installed next to acme/widgets.
func newDiscoveryEnv(t *testing.T, n int, edits ...func(string) string) *env {
	t.Helper()
	e := newEnv(t, append([]func(string) string{replace(`"repositories": ["acme/widgets"],`, ``)}, edits...)...)
	for i := range n {
		e.gh.addRepo(repoName(i), repoID(i))
	}
	return e
}

func repoName(i int) string { return fmt.Sprintf("acme/r%02d", i) }
func repoID(i int) int64    { return 2000 + int64(i) }

func runnerIn(repo int64, job int64, gen int) string {
	return fmt.Sprintf("lux-gha-t1-%d-%d-%d", repo, job, gen)
}

// deliver posts a signed delivery to the reconciler's webhook handler.
func (e *env) deliver(event string, body []byte) int {
	e.t.Helper()
	h, err := e.rec.Webhook([]byte(hookSecret))
	if err != nil {
		e.t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(hookSecret))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

func queuedDelivery(installation int64, repo string, job int64) []byte {
	return []byte(fmt.Sprintf(`{"action":"queued","installation":{"id":%d},"repository":{"full_name":%q},
		"workflow_job":{"id":%d,"run_id":%d,"run_attempt":1,"status":"queued","labels":["self-hosted","lux"]}}`, installation, repo, job, job*10))
}

func installationDelivery(action string, installation int64) []byte {
	return []byte(fmt.Sprintf(`{"action":%q,"installation":{"id":%d}}`, action, installation))
}

func (e *env) installCalls() int { e.gh.mu.Lock(); defer e.gh.mu.Unlock(); return e.gh.installCalls }

func (e *env) liveIn(repo int64) []string { return liveJobs(e, strconv.FormatInt(repo, 10)) }

func TestDiscoveryLoadsEveryPageAndSkipsArchivedAndDisabled(t *testing.T) {
	e := newDiscoveryEnv(t, 5)
	e.gh.repo(repoName(1)).archived = true
	e.gh.repo(repoName(3)).disabled = true
	e.gh.repo(repoName(4)).uninstalled = true
	for i := range 5 {
		e.gh.queueIn(repoName(i), 10*int64(i+1), int64(i+1), "queued", lux3...)
	}
	st := e.pass()
	// Five installed repositories at two per page.
	if st.Error != "" || e.installCalls() != 3 {
		t.Fatalf("status %+v installation pages %d", st, e.installCalls())
	}
	reads := e.gh.takeReads()
	for i, want := range []bool{true, false, true, false, false} {
		if got := reads[repoName(i)] > 0; got != want {
			t.Fatalf("%s read=%v, want %v (reads %v)", repoName(i), got, want, reads)
		}
		if got := len(e.liveIn(repoID(i))) == 1; got != want {
			t.Fatalf("%s has a Run=%v, want %v", repoName(i), got, want)
		}
		if got := e.rec.AllowsRepository(strings.ToUpper(repoName(i))); got != want {
			t.Fatalf("%s allowed=%v, want %v", repoName(i), got, want)
		}
	}
	if !e.rec.AllowsRepository(widgets) || reads[widgets] == 0 {
		t.Fatalf("acme/widgets not in the set: reads %v", reads)
	}
	// Repository IDs come from the listing: no GET /repos/{owner}/{repo}.
	if e.gh.callCount() != e.installCalls()+sumReads(reads) {
		t.Fatal("unexpected calls")
	}
}

func sumReads(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func TestDiscoveryRefreshesOnIntervalAndOnInstallationEvents(t *testing.T) {
	events := []struct{ event, action string }{
		{"installation_repositories", "added"},
		{"installation_repositories", "removed"},
		{"installation", "deleted"},
		{"installation", "suspend"},
		{"installation", "unsuspend"},
		{"installation", "new_permissions_accepted"},
	}
	for _, ev := range events {
		t.Run(ev.event+" "+ev.action, func(t *testing.T) {
			e := newDiscoveryEnv(t, 1)
			e.gh.repo(repoName(0)).uninstalled = true
			e.pass()
			calls := e.installCalls()
			e.gh.repo(repoName(0)).uninstalled = false
			e.gh.queueIn(repoName(0), 70, 7, "queued", lux3...)
			e.clock.Add(time.Minute)
			e.pass()
			if e.installCalls() != calls || len(e.liveIn(repoID(0))) != 0 || e.rec.AllowsRepository(repoName(0)) {
				t.Fatalf("refreshed without a reason: pages %d -> %d", calls, e.installCalls())
			}
			// Another installation's event is not ours.
			if code := e.deliver(ev.event, installationDelivery(ev.action, 7)); code != http.StatusNoContent {
				t.Fatalf("other installation: status %d", code)
			}
			e.pass()
			if e.installCalls() != calls {
				t.Fatal("refreshed on another installation's event")
			}
			if code := e.deliver(ev.event, installationDelivery(ev.action, 42)); code != http.StatusAccepted {
				t.Fatalf("status %d", code)
			}
			if e.installCalls() != calls {
				t.Fatal("refreshed inside the delivery")
			}
			e.pass()
			if e.installCalls() != calls+1 || !e.rec.AllowsRepository(repoName(0)) || len(e.liveIn(repoID(0))) != 1 {
				t.Fatalf("pages %d -> %d, runs %v", calls, e.installCalls(), e.liveIn(repoID(0)))
			}
		})
	}
	t.Run("interval", func(t *testing.T) {
		e := newDiscoveryEnv(t, 1)
		e.gh.repo(repoName(0)).uninstalled = true
		e.pass()
		e.gh.repo(repoName(0)).uninstalled = false
		e.gh.queueIn(repoName(0), 70, 7, "queued", lux3...)
		for range 9 {
			e.clock.Add(time.Minute)
			e.pass()
		}
		if e.installCalls() != 1 || len(e.liveIn(repoID(0))) != 0 {
			t.Fatalf("pages %d before repositoryRefresh", e.installCalls())
		}
		e.clock.Add(time.Minute)
		e.pass()
		if e.installCalls() != 2 || len(e.liveIn(repoID(0))) != 1 {
			t.Fatalf("pages %d, runs %v after repositoryRefresh", e.installCalls(), e.liveIn(repoID(0)))
		}
	})
}

func TestWorkflowJobOutsideTheSetRefreshesAndIsServedOnceListed(t *testing.T) {
	e := newDiscoveryEnv(t, 1)
	e.gh.repo(repoName(0)).uninstalled = true
	e.pass()
	e.gh.queueIn(repoName(0), 70, 7, "queued", lux3...)
	// GitHub delivers the job before the listing includes the repository.
	if code := e.deliver("workflow_job", queuedDelivery(42, repoName(0), 7)); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	e.pass()
	if e.installCalls() != 2 || len(e.liveIn(repoID(0))) != 0 {
		t.Fatalf("pages %d runs %v", e.installCalls(), e.liveIn(repoID(0)))
	}
	e.gh.repo(repoName(0)).uninstalled = false
	if code := e.deliver("workflow_job", queuedDelivery(42, repoName(0), 7)); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	e.pass()
	if e.installCalls() != 3 || len(e.liveIn(repoID(0))) != 1 {
		t.Fatalf("pages %d runs %v", e.installCalls(), e.liveIn(repoID(0)))
	}
	// In the set now: a delivery is a plain wake, with no refresh.
	if code := e.deliver("workflow_job", queuedDelivery(42, repoName(0), 7)); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	e.pass()
	if e.installCalls() != 3 {
		t.Fatalf("refreshed for a repository in the set: pages %d", e.installCalls())
	}
	// Another installation is ignored outright, in the set or not.
	for _, repo := range []string{repoName(0), "acme/elsewhere"} {
		if code := e.deliver("workflow_job", queuedDelivery(7, repo, 8)); code != http.StatusNoContent {
			t.Fatalf("other installation %s: status %d", repo, code)
		}
	}
	e.pass()
	if e.installCalls() != 3 {
		t.Fatalf("another installation's job refreshed: pages %d", e.installCalls())
	}
}

func TestFailedRefreshKeepsTheLastSetAndRetries(t *testing.T) {
	e := newDiscoveryEnv(t, 2)
	e.pass()
	e.gh.fail = func(op, _ string, _ int) error {
		if op == opInstall {
			return &githubapp.APIError{StatusCode: 502}
		}
		return nil
	}
	e.gh.queueIn(repoName(1), 70, 7, "queued", lux3...)
	e.deliver("installation_repositories", installationDelivery("removed", 42))
	e.deliver("workflow_job", queuedDelivery(42, repoName(1), 7))
	st := e.pass()
	if st.Error != ErrGitHub || !e.rec.AllowsRepository(repoName(1)) || !e.rec.AllowsRepository(widgets) {
		t.Fatalf("status %+v: the last set was not kept", st)
	}
	// The kept set is served.
	if len(e.liveIn(repoID(1))) != 1 {
		t.Fatalf("runs %v", e.liveIn(repoID(1)))
	}
	calls := e.installCalls()
	e.clock.Add(time.Minute)
	e.pass()
	if e.installCalls() != calls+1 {
		t.Fatal("a failed refresh was not retried on the next pass")
	}
	e.gh.repo(repoName(1)).uninstalled = true
	e.gh.fail = nil
	e.clock.Add(time.Minute)
	if st := e.pass(); st.Error != "" || e.rec.AllowsRepository(repoName(1)) {
		t.Fatalf("status %+v after recovery", st)
	}
}

func TestFailedFirstRefreshReadsNothingAndCreatesNothing(t *testing.T) {
	e := newDiscoveryEnv(t, 1)
	e.gh.queue(7, "queued", lux3...)
	// A Run from before a restart, whose job has since finished.
	old := e.lux.seed("api.github.com/t1/1001/6/0", luxclient.RunSpec{Labels: map[string]string{
		LabelManaged: "t1", LabelRepo: "1001", LabelRoute: "default", LabelJob: "6", LabelGen: "0", LabelRunner: runner("6", 0)}})
	e.gh.fail = func(op, _ string, _ int) error {
		if op == opInstall {
			return &githubapp.APIError{StatusCode: 500}
		}
		return nil
	}
	for range 4 {
		st := e.pass()
		if st.Error != ErrGitHub || st.Provisioned != 0 {
			t.Fatalf("status %+v", st)
		}
		e.clock.Add(time.Hour)
	}
	if reads := e.gh.takeReads(); len(reads) != 0 || e.lux.createCount() != 1 || e.gh.jitCount() != 0 ||
		len(e.lux.terminatedIDs()) != 0 || e.rec.AllowsRepository(widgets) {
		t.Fatalf("reads %v creates %d jit %d terminated %v", reads, e.lux.createCount(), e.gh.jitCount(), e.lux.terminatedIDs())
	}
	e.gh.fail = nil
	e.pass()
	if len(e.lux.liveForJob("7")) != 1 || slices.Contains(e.lux.terminatedIDs(), old) {
		t.Fatalf("after recovery: live %v terminated %v", e.liveIn(1001), e.lux.terminatedIDs())
	}
}

func TestExplicitListNeverDiscovers(t *testing.T) {
	e := newEnv(t)
	e.gh.addRepo("acme/gadgets", 1002)
	e.gh.queueIn("acme/gadgets", 70, 7, "queued", lux3...)
	for _, d := range []struct {
		event string
		body  []byte
	}{
		{"workflow_job", queuedDelivery(42, "acme/gadgets", 7)},
		{"installation_repositories", installationDelivery("added", 42)},
		{"installation", installationDelivery("new_permissions_accepted", 42)},
	} {
		if code := e.deliver(d.event, d.body); code != http.StatusNoContent {
			t.Fatalf("%s: status %d", d.event, code)
		}
	}
	for range 3 {
		e.pass()
		e.clock.Add(time.Hour)
	}
	if reads := e.gh.takeReads(); e.installCalls() != 0 || reads["acme/gadgets"] != 0 || len(e.liveIn(1002)) != 0 || reads[widgets] == 0 {
		t.Fatalf("pages %d reads %v", e.installCalls(), reads)
	}
	if e.rec.AllowsRepository("acme/gadgets") || !e.rec.AllowsRepository("ACME/widgets") {
		t.Fatal("explicit filter changed")
	}
}

// readEveryPass runs passes a minute apart for d and fails unless repo is
// read in each.
func (e *env) readEveryPass(repo string, d time.Duration) {
	e.t.Helper()
	for elapsed := time.Duration(0); elapsed < d; elapsed += time.Minute {
		e.clock.Add(time.Minute)
		e.pass()
		if reads := e.gh.takeReads(); reads[repo] == 0 {
			e.t.Fatalf("%s not read %v in", repo, elapsed+time.Minute)
		}
	}
}

func TestLiveRunMakesARepositoryHot(t *testing.T) {
	e := newDiscoveryEnv(t, 30)
	e.pass()
	e.gh.takeReads()
	// A Run whose runner is not registered and whose job is not demand: only
	// the Run keeps the repository hot, and it must be read to end the Run
	// idleGrace after its runner is first seen missing.
	id := e.lux.seed("api.github.com/t1/2005/9/0", luxclient.RunSpec{Labels: map[string]string{
		LabelManaged: "t1", LabelRepo: "2005", LabelRoute: "default", LabelJob: "9", LabelGen: "0", LabelRunner: runnerIn(2005, 9, 0)}})
	e.readEveryPass(repoName(5), 3*time.Minute)
	if !slices.Equal(e.lux.terminatedIDs(), []string{id}) {
		t.Fatalf("terminated %v", e.lux.terminatedIDs())
	}
}

// withReconcile sets reconcile durations on the test configuration.
func withReconcile(fields string) func(string) string {
	return replace(`"maxRunners": 10`, `"maxRunners": 10, "reconcile": {`+fields+`}`)
}

// The job of a queued delivery is not yet listed by the runs API, so the
// mark alone keeps its repository hot, for hotWindow after the latest
// delivery.
func TestQueuedDeliveryMakesARepositoryHotForHotWindow(t *testing.T) {
	e := newDiscoveryEnv(t, 30, withReconcile(`"hotWindow": "15m"`))
	e.pass()
	e.gh.takeReads()
	if code := e.deliver("workflow_job", queuedDelivery(42, repoName(7), 7)); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	e.readEveryPass(repoName(7), 10*time.Minute)
	// Delivered again (a re-run): hot until 15m after this delivery.
	e.deliver("workflow_job", queuedDelivery(42, repoName(7), 7))
	e.readEveryPass(repoName(7), 14*time.Minute)
	// Past hotWindow it is cold again: some passes skip it.
	skipped := 0
	for range 10 {
		e.clock.Add(time.Minute)
		e.pass()
		if e.gh.takeReads()[repoName(7)] == 0 {
			skipped++
		}
	}
	if skipped == 0 {
		t.Fatal("still read every pass after hotWindow")
	}
	// A delivery that cannot create demand does not make it hot.
	body := bytes.Replace(queuedDelivery(42, repoName(8), 8), []byte(`"lux"]`), []byte(`"gpu"]`), 1)
	e.deliver("workflow_job", body)
	skipped = 0
	for range 10 {
		e.clock.Add(time.Minute)
		e.pass()
		if e.gh.takeReads()[repoName(8)] == 0 {
			skipped++
		}
	}
	if skipped == 0 {
		t.Fatal("an unmatched delivery made the repository hot")
	}
}

// A read that lists the delivered job ends its mark: here the job was
// already picked elsewhere, so the repository has nothing to serve.
func TestReadThatListsTheDeliveredJobEndsItsMark(t *testing.T) {
	e := newDiscoveryEnv(t, 30, withReconcile(`"hotWindow": "15m"`))
	e.pass()
	e.gh.takeReads()
	e.gh.queueIn(repoName(7), 70, 7, "queued", lux3...)
	e.gh.pickElsewhere(repoName(7), 7, 900, "elsewhere")
	// A second job whose delivery arrived before the runs API lists it.
	e.deliver("workflow_job", queuedDelivery(42, repoName(7), 7))
	e.deliver("workflow_job", queuedDelivery(42, repoName(8), 8))
	e.clock.Add(time.Minute)
	e.pass()
	if reads := e.gh.takeReads(); reads[repoName(7)] == 0 || reads[repoName(8)] == 0 {
		t.Fatalf("reads %v", reads)
	}
	// Job 8 is still not listed, so its mark holds for hotWindow; job 7's
	// mark is gone.
	skipped := 0
	for range 10 {
		e.clock.Add(time.Minute)
		e.pass()
		reads := e.gh.takeReads()
		if reads[repoName(8)] == 0 {
			t.Fatal("acme/r08 not read while its job is unlisted")
		}
		if reads[repoName(7)] == 0 {
			skipped++
		}
	}
	if skipped == 0 {
		t.Fatal("still read every pass after a read listed its job")
	}
}

// sweep runs passes a minute apart for d, with a woken pass every 5 s for the
// first wakeFor, and returns, per repository, the longest gap between reads
// and, per pass, the GitHub calls it made. A pass on a minute boundary is a
// tick pass, and every hot repository must be read in it. Each hook runs
// ahead of pass i.
func (e *env) sweep(d, wakeFor time.Duration, hot []string, hooks ...func(i int)) (map[string]time.Duration, []int) {
	e.t.Helper()
	last := map[string]time.Time{}
	gaps := map[string]time.Duration{}
	start := e.clock.Now()
	for repo := range e.gh.repos {
		last[repo] = start
	}
	var calls []int
	for elapsed := time.Duration(0); elapsed < d; {
		step := time.Minute
		if elapsed < wakeFor {
			step = 5 * time.Second
		}
		e.clock.Add(step)
		elapsed += step
		for _, f := range hooks {
			f(len(calls))
		}
		tick := elapsed%time.Minute == 0
		before := e.gh.callCount()
		if tick {
			e.pass()
		} else {
			e.wake()
		}
		calls = append(calls, e.gh.callCount()-before)
		reads := e.gh.takeReads()
		for _, h := range hot {
			if tick && reads[h] == 0 {
				e.t.Fatalf("hot %s skipped at %v", h, elapsed)
			}
		}
		now := e.clock.Now()
		for repo := range reads {
			gaps[repo] = max(gaps[repo], now.Sub(last[repo]))
			last[repo] = now
		}
	}
	for repo, at := range last {
		gaps[repo] = max(gaps[repo], e.clock.Now().Sub(at))
	}
	return gaps, calls
}

// newBudgetEnv has 68 installed repositories, two of them hot with a busy
// runner each, after the startup pass.
func newBudgetEnv(t *testing.T) (*env, []string) {
	e := newDiscoveryEnv(t, 67)
	e.gh.perPage = 100
	e.gh.queueIn(repoName(10), 100, 1, "queued", lux3...)
	e.gh.queueIn(repoName(20), 200, 2, "queued", lux3...)
	runnerLists := map[string]int{}
	e.gh.fail = func(op, repo string, _ int) error {
		if op == opRunners {
			runnerLists[repo]++
		}
		return nil
	}
	startup := e.gh.callCount()
	e.pass()
	startup = e.gh.callCount() - startup
	e.gh.fail = nil
	// Every repository is read once at startup: 3 calls each, plus the
	// listing, the two jobs read and the two JIT registrations.
	if startup > 68*3+10 || len(runnerLists) != 68 {
		t.Fatalf("startup pass made %d calls, read %d repositories", startup, len(runnerLists))
	}
	for repo, n := range runnerLists {
		if n != 1 {
			t.Fatalf("startup pass read %s %d times", repo, n)
		}
	}
	for i, job := range map[int]int64{10: 1, 20: 2} {
		if err := e.gh.pick(runnerIn(repoID(i), job, 0), job); err != nil {
			t.Fatal(err)
		}
	}
	e.gh.takeReads()
	return e, []string{repoName(10), repoName(20)}
}

func TestColdSweepReadsEveryRepositoryWithinColdSweep(t *testing.T) {
	e, hot := newBudgetEnv(t)
	gaps, _ := e.sweep(time.Hour, 10*time.Minute, hot)
	if len(gaps) != 68 {
		t.Fatalf("%d repositories tracked", len(gaps))
	}
	for repo, gap := range gaps {
		if gap > 10*time.Minute {
			t.Fatalf("%s unread for %v", repo, gap)
		}
	}
}

func TestAPIBudgetWith68RepositoriesAnd2Hot(t *testing.T) {
	e, hot := newBudgetEnv(t)
	// Ten minutes of woken passes every 5 s, then ticks: 170 passes.
	_, calls := e.sweep(time.Hour, 10*time.Minute, hot)
	total := 0
	for i, n := range calls {
		// 2 hot × (2 run lists + 1 job list + 1 runner list) = 8, and at
		// most ⌈66/10⌉+1 = 8 cold repositories × 3, and 1 listing page.
		if n > 8+8*3+1 {
			t.Fatalf("pass %d made %d calls", i, n)
		}
		total += n
	}
	if total >= 5000 {
		t.Fatalf("%d calls in an hour", total)
	}
	t.Logf("%d passes, %d calls in the hour, max %d per pass", len(calls), total, slices.Max(calls))
}

// A burst across the installation: a matching queued delivery for each of
// 67 of the 68 repositories. Demand the caps keep unserved does not keep its
// repository hot, and a read that lists a delivered job ends its mark, so a
// tick pass reads the repositories with live Runs (at most maxRunners) plus
// those with a delivery not yet listed. A woken pass reads only the
// repositories delivered since the previous pass, here one: ~4 calls.
func TestAPIBudgetUnderABurst(t *testing.T) {
	for _, c := range []struct {
		name    string
		backlog bool // the jobs stay queued past maxRunners; else served at once
		wakeFor time.Duration
	}{
		{"served at once, ticks", false, 0},
		{"served at once, 5s wakes for 15m", false, 15 * time.Minute},
		{"backlog above maxRunners, ticks", true, 0},
		{"backlog above maxRunners, 5s wakes for 15m", true, 15 * time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, hot := newBudgetEnv(t)
			var burst []string
			for repo := range e.gh.repos {
				if repo != hot[0] {
					burst = append(burst, repo)
				}
			}
			slices.Sort(burst)
			for i, repo := range burst {
				if c.backlog {
					e.gh.queueIn(repo, 1000+int64(i), 100+int64(i), "queued", lux3...)
				}
			}
			// The deliveries arrive one per woken pass, or all before the
			// first ticked pass. A job served at once is picked by a runner
			// outside the bridge before the next pass.
			_, calls := e.sweep(time.Hour, c.wakeFor, hot, func(i int) {
				for j, repo := range burst {
					if c.wakeFor == 0 && i == 0 || c.wakeFor > 0 && i == j {
						job := 100 + int64(j)
						if !c.backlog {
							e.gh.queueIn(repo, 1000+int64(j), job, "queued", lux3...)
							e.gh.pickElsewhere(repo, job, 9000+job, "elsewhere")
						}
						e.deliver("workflow_job", queuedDelivery(42, repo, job))
					}
				}
			})
			total, wokenMax := 0, 0
			for i, n := range calls {
				total += n
				if c.wakeFor > 0 && (i+1)%12 != 0 && i < 180 {
					wokenMax = max(wokenMax, n)
				}
			}
			if live := len(e.lux.live()); c.backlog && live != e.cfg.MaxRunners {
				t.Fatalf("%d live Runs, want the cap %d", live, e.cfg.MaxRunners)
			}
			// One delivered repository: 2 run lists, 1 job list, 1 runner
			// list, and at most 1 JIT registration.
			if wokenMax > 5 {
				t.Fatalf("a woken pass made %d calls", wokenMax)
			}
			if total >= 5000 {
				t.Fatalf("%d calls in the hour over %d passes", total, len(calls))
			}
			t.Logf("%d passes, %d calls in the hour, max %d per pass, max %d per woken pass", len(calls), total, slices.Max(calls), wokenMax)
		})
	}
}

// A repository not read this pass, here because it left the installation,
// keeps its Runs and runners, and its Runs fill the caps.
func TestUnreadRepositoryRunsAreKeptAndCounted(t *testing.T) {
	for _, c := range []struct {
		name   string
		edit   func(string) string
		labels []string
	}{
		{"global cap", replace(`"maxRunners": 10`, `"maxRunners": 1`), lux3},
		{"route cap", func(s string) string { return s }, []string{"self-hosted", "linux", "lux-big"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newDiscoveryEnv(t, 2, c.edit)
			e.gh.queueIn(repoName(0), 70, 7, "queued", c.labels...)
			e.pass()
			id := e.runID("7")
			// The job is cancelled, so its idle runner is surplus, and the
			// repository then leaves the installation.
			e.gh.complete(7, "cancelled")
			e.gh.repo(repoName(0)).uninstalled = true
			e.deliver("installation_repositories", installationDelivery("removed", 42))
			e.gh.queueIn(repoName(1), 80, 8, "queued", c.labels...)
			for range 8 {
				e.clock.Add(time.Hour)
				st := e.pass()
				if st.Provisioned != 0 {
					t.Fatalf("status %+v", st)
				}
			}
			if slices.Contains(e.lux.terminatedIDs(), id) || len(e.gh.deletedNames()) != 0 {
				t.Fatalf("terminated %v deleted %v", e.lux.terminatedIDs(), e.gh.deletedNames())
			}
			if len(e.liveIn(repoID(1))) != 0 {
				t.Fatal("created past a cap filled by an unread repository's Run")
			}
			// The Run ends on its own; the slot frees.
			e.lux.end(id, "timeout")
			e.pass()
			if len(e.liveIn(repoID(1))) != 1 {
				t.Fatalf("runs %v", e.liveIn(repoID(1)))
			}
		})
	}
}

// The same holds for a hot repository whose read fails.
func TestFailedReadOfAHotRepositoryKeepsItsRunAndRunner(t *testing.T) {
	e := newDiscoveryEnv(t, 3)
	e.gh.queueIn(repoName(0), 70, 7, "queued", lux3...)
	e.pass()
	id := e.runID("7")
	e.gh.complete(7, "cancelled")
	e.gh.fail = func(op, repo string, _ int) error {
		if repo == repoName(0) && op == opRunners {
			return &githubapp.APIError{StatusCode: 500}
		}
		return nil
	}
	for range 5 {
		e.clock.Add(time.Hour)
		e.pass()
	}
	if len(e.lux.terminatedIDs()) != 0 || len(e.gh.deletedNames()) != 0 || len(e.lux.liveForJob("7")) != 1 {
		t.Fatalf("terminated %v deleted %v; Run %s", e.lux.terminatedIDs(), e.gh.deletedNames(), id)
	}
}

func TestReadFailureOfASelectedColdRepositoryBlocksCreationThatPass(t *testing.T) {
	e := newDiscoveryEnv(t, 20)
	e.pass()
	e.gh.takeReads()
	// Every cold repository fails; each pass selects some of them.
	e.gh.fail = func(op, repo string, _ int) error {
		if repo != repoName(19) && op == opRunners {
			return &githubapp.APIError{StatusCode: 500}
		}
		return nil
	}
	e.gh.queueIn(repoName(19), 80, 8, "queued", lux3...)
	if code := e.deliver("workflow_job", queuedDelivery(42, repoName(19), 8)); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	for range 3 {
		e.clock.Add(time.Minute)
		st := e.pass()
		reads := e.gh.takeReads()
		if reads[repoName(19)] == 0 || len(reads) < 2 {
			t.Fatalf("reads %v", reads)
		}
		if st.Error != ErrGitHub || st.Provisioned != 0 || st.Demand["default"] != 1 || len(e.liveIn(repoID(19))) != 0 {
			t.Fatalf("created while a selected repository failed: %+v", st)
		}
	}
	e.gh.fail = nil
	e.clock.Add(time.Minute)
	if st := e.pass(); st.Provisioned != 1 || len(e.liveIn(repoID(19))) != 1 {
		t.Fatalf("after the reads recover: %+v", st)
	}
}

func TestOrphanRunnerInAColdRepositoryIsDeletedAfterItsSweep(t *testing.T) {
	e := newDiscoveryEnv(t, 30)
	e.pass()
	e.gh.takeReads()
	// An offline registration with no Run appears in a cold repository.
	name := runnerIn(repoID(17), 9, 0)
	e.gh.addRunner(repoName(17), 77, name)
	e.gh.setOnline(name, false)
	var firstRead time.Time
	start := e.clock.Now()
	for e.clock.Now().Sub(start) < 20*time.Minute {
		e.clock.Add(time.Minute)
		e.pass()
		if e.gh.takeReads()[repoName(17)] > 0 && firstRead.IsZero() {
			firstRead = e.clock.Now()
		}
		if slices.Contains(e.gh.deletedNames(), name) {
			break
		}
	}
	if firstRead.IsZero() || firstRead.Sub(start) > 10*time.Minute {
		t.Fatalf("first read %v after start", firstRead.Sub(start))
	}
	deleted := e.clock.Now()
	if !slices.Contains(e.gh.deletedNames(), name) || deleted.Sub(firstRead) > 2*time.Minute {
		t.Fatalf("deleted=%v %v after its first read", e.gh.deletedNames(), deleted.Sub(firstRead))
	}
	if e.lux.createCount() != 0 {
		t.Fatal("created a Run")
	}
}

// A registration whose submission is unresolved holds a cap slot, global
// and per route, while its repository cannot be read, here because it left
// the installation.
func TestUnresolvedRegistrationInAnUnreadRepositoryHoldsASlot(t *testing.T) {
	for _, c := range []struct {
		name   string
		edit   func(string) string
		labels []string
	}{
		{"global cap", replace(`"maxRunners": 10`, `"maxRunners": 1`), lux3},
		{"route cap", func(s string) string { return s }, []string{"self-hosted", "linux", "lux-big"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newDiscoveryEnv(t, 2, c.edit)
			e.gh.queueIn(repoName(0), 70, 7, "queued", c.labels...)
			e.lux.failCreate = 1
			if st := e.pass(); st.Error != ErrLux || e.gh.jitCount() != 1 {
				t.Fatalf("status %+v", st)
			}
			e.gh.repo(repoName(0)).uninstalled = true
			e.deliver("installation_repositories", installationDelivery("removed", 42))
			e.gh.queueIn(repoName(1), 80, 8, "queued", c.labels...)
			e.deliver("workflow_job", queuedDelivery(42, repoName(1), 8))
			for _, step := range []time.Duration{time.Minute, time.Minute - time.Second} {
				e.clock.Add(step)
				st := e.pass()
				if e.gh.takeReads()[repoName(1)] == 0 || sumDemand(st) != 1 || len(e.liveIn(repoID(1))) != 0 {
					t.Fatalf("created past the cap while job 7's submission was unresolved: %+v", st)
				}
			}
			// idleGrace after the repository left, its registration is forgotten.
			e.clock.Add(2 * time.Minute)
			e.pass()
			if len(e.liveIn(repoID(1))) != 1 {
				t.Fatalf("runs %v", e.liveIn(repoID(1)))
			}
		})
	}
}

func sumDemand(st Status) int {
	n := 0
	for _, v := range st.Demand {
		n += v
	}
	return n
}

// A remembered registration whose Run is live is counted once, through the
// Lux listing, not again as unresolved.
func TestRegistrationWithALiveRunInAnUnreadRepositoryIsCountedOnce(t *testing.T) {
	e := newDiscoveryEnv(t, 2, replace(`"maxRunners": 10`, `"maxRunners": 2`))
	e.gh.queueIn(repoName(0), 70, 7, "queued", lux3...)
	e.pass()
	if len(e.liveIn(repoID(0))) != 1 {
		t.Fatalf("runs %v", e.liveIn(repoID(0)))
	}
	e.gh.repo(repoName(0)).uninstalled = true
	e.deliver("installation_repositories", installationDelivery("removed", 42))
	e.gh.queueIn(repoName(1), 80, 8, "queued", lux3...)
	e.deliver("workflow_job", queuedDelivery(42, repoName(1), 8))
	e.clock.Add(time.Minute)
	if st := e.pass(); st.Provisioned != 1 || len(e.liveIn(repoID(1))) != 1 {
		t.Fatalf("one live Run of two slots, yet nothing created: %+v", st)
	}
}

// After a restart nothing is remembered, so a registration left by the
// previous process in a repository whose first read fails is unknown; it
// may still have a submission in flight. No Run is created until that
// repository has been read and the registration resolved.
func TestPreRestartRegistrationInAnUnreadRepositoryBlocksCreation(t *testing.T) {
	e := newDiscoveryEnv(t, 30, replace(`"maxRunners": 10`, `"maxRunners": 1`))
	name := runnerIn(repoID(17), 9, 0)
	e.gh.addRunner(repoName(17), 77, name)
	e.gh.setOnline(name, false)
	e.gh.queueIn(repoName(3), 30, 3, "queued", lux3...)
	failed := false
	e.gh.fail = func(op, repo string, _ int) error {
		if op == opRunners && repo == repoName(17) && !failed {
			failed = true
			return &githubapp.APIError{StatusCode: 500}
		}
		return nil
	}
	if st := e.pass(); st.Provisioned != 0 || e.lux.createCount() != 0 {
		t.Fatalf("created while acme/r17 had never been read: %+v", st)
	}
	readOK := false
	for range 6 {
		e.clock.Add(time.Minute)
		st := e.pass()
		if e.gh.takeReads()[repoName(17)] > 0 {
			readOK = true
		}
		if !readOK {
			t.Fatal("acme/r17 not read after its first read failed")
		}
		if e.lux.createCount() > 0 {
			// The registration was resolved (deleted as an orphan) first.
			if !slices.Contains(e.gh.deletedNames(), name) || st.Provisioned != 1 {
				t.Fatalf("created while %s was unresolved: %+v deleted %v", name, st, e.gh.deletedNames())
			}
			return
		}
	}
	t.Fatal("never created once acme/r17 was read")
}

// A repository whose first read fails is not yet known: it stays selected
// every pass until a read succeeds, then goes cold.
func TestRepositoryWhoseFirstReadFailsIsReadUntilItSucceeds(t *testing.T) {
	e := newDiscoveryEnv(t, 30)
	e.gh.fail = func(op, repo string, _ int) error {
		if op == opRunners && repo == repoName(5) {
			return &githubapp.APIError{StatusCode: 500}
		}
		return nil
	}
	e.pass()
	e.gh.takeReads()
	e.readEveryPass(repoName(5), 3*time.Minute)
	e.gh.fail = nil
	e.clock.Add(time.Minute)
	e.pass()
	if e.gh.takeReads()[repoName(5)] == 0 {
		t.Fatal("not read once its reads recover")
	}
	if e.skippedIn(repoName(5), 5) == 0 {
		t.Fatal("still read every pass after a successful read")
	}
}

// skippedIn runs n passes a minute apart and counts those that did not read repo.
func (e *env) skippedIn(repo string, n int) int {
	e.t.Helper()
	skipped := 0
	for range n {
		e.clock.Add(time.Minute)
		e.pass()
		if e.gh.takeReads()[repo] == 0 {
			skipped++
		}
	}
	return skipped
}

// A hot repository leaves the installation and its installation_repositories
// delivery is lost. Its reads answer 404, which re-lists the installation on
// the next pass instead of after repositoryRefresh.
func TestRemovedRepositoryWithALostDeliveryBlocksCreationBriefly(t *testing.T) {
	e := newDiscoveryEnv(t, 3)
	e.gh.queueIn(repoName(0), 70, 7, "queued", lux3...)
	e.pass()
	id := e.runID("7")
	e.gh.repo(repoName(0)).uninstalled = true
	e.gh.queueIn(repoName(1), 80, 8, "queued", lux3...)
	e.deliver("workflow_job", queuedDelivery(42, repoName(1), 8))
	blocked := 0
	for range 10 {
		e.clock.Add(time.Minute)
		st := e.pass()
		if len(e.liveIn(repoID(1))) == 1 {
			break
		}
		if st.Error != ErrGitHub {
			t.Fatalf("not created and no read error: %+v", st)
		}
		blocked++
	}
	if blocked > 2 || len(e.liveIn(repoID(1))) != 1 {
		t.Fatalf("creation blocked for %d passes", blocked)
	}
	if e.rec.AllowsRepository(repoName(0)) {
		t.Fatal("acme/r00 still in the set")
	}
	e.clock.Add(time.Minute)
	e.gh.takeReads()
	e.pass()
	if reads := e.gh.takeReads(); reads[repoName(0)] != 0 {
		t.Fatalf("acme/r00 still read: %v", reads)
	}
	if len(e.lux.terminatedIDs()) != 0 || len(e.lux.liveForJob("7")) != 1 {
		t.Fatalf("terminated %v; Run %s", e.lux.terminatedIDs(), id)
	}
}

// A refresh requested while GitHub rate-limits the listing runs on the
// first pass after the limit.
func TestRefreshRequestedDuringARateLimitRunsAfterIt(t *testing.T) {
	e := newDiscoveryEnv(t, 1)
	e.gh.repo(repoName(0)).uninstalled = true
	e.pass()
	e.gh.repo(repoName(0)).uninstalled = false
	e.deliver("installation_repositories", installationDelivery("added", 42))
	e.gh.rateLimitUntil = e.clock.Now().Add(30 * time.Second)
	if st := e.pass(); st.Error != ErrRateLimited {
		t.Fatalf("status %+v", st)
	}
	e.clock.Add(time.Minute)
	e.pass()
	if !e.rec.AllowsRepository(repoName(0)) {
		t.Fatal("the refresh requested during the rate limit was lost")
	}
}

// Demand the caps cannot serve does not keep its repository hot; a pass
// after a slot frees reads it again.
func TestUnservableDemandIsColdUntilASlotFrees(t *testing.T) {
	for _, c := range []struct {
		name   string
		edit   func(string) string
		labels []string
	}{
		{"global cap", replace(`"maxRunners": 10`, `"maxRunners": 1`), lux3},
		{"route cap", func(s string) string { return s }, []string{"self-hosted", "linux", "lux-big"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newDiscoveryEnv(t, 30, c.edit)
			e.gh.queueIn(repoName(0), 70, 7, "queued", c.labels...)
			e.gh.queueIn(repoName(9), 90, 9, "queued", c.labels...)
			e.pass()
			e.gh.takeReads()
			id := e.runID("7")
			if len(e.liveIn(repoID(9))) != 0 {
				t.Fatal("created past the cap")
			}
			if e.skippedIn(repoName(9), 5) == 0 {
				t.Fatal("demand the cap cannot serve kept acme/r09 hot")
			}
			e.lux.end(id, "timeout")
			e.gh.complete(7, "success")
			e.clock.Add(time.Minute)
			e.pass()
			if e.gh.takeReads()[repoName(9)] == 0 || len(e.liveIn(repoID(9))) != 1 {
				t.Fatalf("not read and served once a slot freed: runs %v", e.liveIn(repoID(9)))
			}
		})
	}
}

// A repository listed on two pages is read, and acted on, once.
func TestRepositoryListedTwiceIsReadOnce(t *testing.T) {
	e := newDiscoveryEnv(t, 3)
	e.gh.listTwice = repoName(1)
	e.gh.queueIn(repoName(1), 70, 7, "queued", lux3...)
	if st := e.pass(); st.Demand["default"] != 1 || st.Provisioned != 1 {
		t.Fatalf("status %+v", st)
	}
	// 2 run lists, 1 job list, 1 runner list, 1 JIT registration.
	if reads := e.gh.takeReads(); reads[repoName(1)] != 5 {
		t.Fatalf("reads %v", reads)
	}
}

// The listing stops at its page bound.
func TestInstallationListingStopsAtItsPageBound(t *testing.T) {
	e := newDiscoveryEnv(t, 1)
	e.gh.endlessListing = true
	if st := e.pass(); st.Error != ErrGitHub || e.installCalls() != maxInstallationRepoPages {
		t.Fatalf("status %+v after %d pages", st, e.installCalls())
	}
	if reads := e.gh.takeReads(); len(reads) != 0 || e.rec.AllowsRepository(repoName(0)) {
		t.Fatalf("reads %v", reads)
	}
}

// Cold repositories read early in a sweep turning hot leave the unread ones
// paced over a smaller set; each is still read within coldSweep.
func TestColdSweepHoldsWhenTheColdSetShrinks(t *testing.T) {
	e := newDiscoveryEnv(t, 39)
	e.pass()
	start := e.clock.Now()
	e.gh.takeReads()
	readEarly := map[string]bool{}
	for range 5 {
		e.clock.Add(time.Minute)
		e.pass()
		for repo := range e.gh.takeReads() {
			readEarly[repo] = true
		}
	}
	if len(readEarly) == 0 || len(readEarly) >= 40 {
		t.Fatalf("%d read in 5 minutes", len(readEarly))
	}
	for repo := range readEarly {
		e.deliver("workflow_job", queuedDelivery(42, repo, 1))
	}
	readLate := map[string]bool{}
	for e.clock.Now().Sub(start) < 10*time.Minute {
		e.clock.Add(time.Minute)
		e.pass()
		for repo := range e.gh.takeReads() {
			readLate[repo] = true
		}
	}
	for repo := range e.gh.repos {
		if !readEarly[repo] && !readLate[repo] {
			t.Fatalf("%s unread for over 10m", repo)
		}
	}
}

// newWokenEnv has 30 installed repositories after the startup pass, with a
// live Run in each of acme/r00 .. acme/r02: one whose job was cancelled (an
// idle surplus runner), one whose runner ran its job and deregistered, and
// one plain busy runner. acme/r03 holds an offline orphan registration.
func newWokenEnv(t *testing.T, edits ...func(string) string) *env {
	e := newDiscoveryEnv(t, 30, edits...)
	for i := range 3 {
		e.gh.queueIn(repoName(i), 70+int64(i), 7+int64(i), "queued", lux3...)
	}
	orphan := runnerIn(repoID(3), 9, 0)
	e.gh.addRunner(repoName(3), 77, orphan)
	e.gh.setOnline(orphan, false)
	if st := e.pass(); st.Provisioned != 3 {
		t.Fatalf("startup: %+v", st)
	}
	e.gh.complete(7, "cancelled")
	if err := e.gh.pick(runnerIn(repoID(1), 8, 0), 8); err != nil {
		t.Fatal(err)
	}
	e.gh.complete(8, "success")
	if err := e.gh.pick(runnerIn(repoID(2), 9, 0), 9); err != nil {
		t.Fatal(err)
	}
	e.gh.takeReads()
	return e
}

func readSet(reads map[string]int) []string {
	return slices.Sorted(maps.Keys(reads))
}

func TestWokenPassReadsOnlyDeliveredRepositories(t *testing.T) {
	e := newWokenEnv(t)
	e.gh.queueIn(repoName(7), 170, 17, "queued", lux3...)
	e.deliver("workflow_job", queuedDelivery(42, repoName(7), 17))
	e.clock.Add(5 * time.Second)
	st := e.wake()
	if got := readSet(e.gh.takeReads()); !slices.Equal(got, []string{repoName(7)}) {
		t.Fatalf("woken pass read %v", got)
	}
	if st.Provisioned != 1 || !slices.Equal(e.liveIn(repoID(7)), []string{"17"}) {
		t.Fatalf("status %+v, runs %v", st, e.liveIn(repoID(7)))
	}
	// No delivery since: the next woken pass reads nothing.
	e.clock.Add(5 * time.Second)
	e.wake()
	if reads := e.gh.takeReads(); len(reads) != 0 {
		t.Fatalf("woken pass with no delivery read %v", reads)
	}
}

// Over idleGrace of woken passes, nothing in the live-Run repositories is
// read, deleted or terminated; the first tick pass reads them and, their
// timers having run through the woken passes, cleans up at once.
func TestWokenPassLeavesUnreadRepositoriesAlone(t *testing.T) {
	e := newWokenEnv(t)
	// The tick that first sees the surplus, the missing runner and the orphan.
	e.clock.Add(time.Minute)
	if st := e.pass(); st.Deleted != 0 || st.Terminated != 0 {
		t.Fatalf("tick: %+v", st)
	}
	e.gh.takeReads()
	for i := range 36 {
		job := 100 + int64(i)
		e.gh.queueIn(repoName(10+i%10), 1000+job, job, "queued", lux3...)
		e.gh.pickElsewhere(repoName(10+i%10), job, 9000+job, "elsewhere")
		e.deliver("workflow_job", queuedDelivery(42, repoName(10+i%10), job))
		e.clock.Add(5 * time.Second)
		e.wake()
		for repo := range e.gh.takeReads() {
			if repo != repoName(10+i%10) {
				t.Fatalf("woken pass %d read %s", i, repo)
			}
		}
	}
	if len(e.lux.terminatedIDs()) != 0 || len(e.gh.deletedNames()) != 0 || len(e.lux.live()) != 3 {
		t.Fatalf("woken passes acted on unread repositories: terminated %v deleted %v", e.lux.terminatedIDs(), e.gh.deletedNames())
	}
	st := e.pass()
	reads := e.gh.takeReads()
	for i := range 4 {
		if reads[repoName(i)] == 0 {
			t.Fatalf("tick pass skipped hot %s: reads %v", repoName(i), readSet(reads))
		}
	}
	// The surplus runner and the orphan deleted, the surplus Run and the
	// Run whose runner left terminated.
	if st.Deleted != 2 || st.Terminated != 2 || len(e.lux.live()) != 1 {
		t.Fatalf("tick after idleGrace: %+v, deleted %v", st, e.gh.deletedNames())
	}
}

// A tick pass reads every hot repository: live Runs, remembered runners and
// marks.
func TestTickPassReadsTheFullHotSet(t *testing.T) {
	e := newWokenEnv(t)
	e.deliver("workflow_job", queuedDelivery(42, repoName(7), 17))
	e.clock.Add(time.Minute)
	e.pass()
	reads := e.gh.takeReads()
	for _, i := range []int{0, 1, 2, 3, 7} {
		if reads[repoName(i)] == 0 {
			t.Fatalf("tick pass skipped hot %s: reads %v", repoName(i), readSet(reads))
		}
	}
}

// A woken pass holds a cap slot for a remembered registration in a
// repository it does not read, whose submission is unresolved.
func TestWokenPassHoldsSlotsOfUnreadRegistrations(t *testing.T) {
	e := newDiscoveryEnv(t, 30, replace(`"maxRunners": 10`, `"maxRunners": 1`))
	e.gh.queueIn(repoName(0), 70, 7, "queued", lux3...)
	e.lux.failCreate = 1
	if st := e.pass(); st.Error != ErrLux || e.gh.jitCount() != 1 {
		t.Fatalf("status %+v", st)
	}
	e.gh.takeReads()
	e.gh.queueIn(repoName(1), 80, 8, "queued", lux3...)
	e.deliver("workflow_job", queuedDelivery(42, repoName(1), 8))
	e.clock.Add(5 * time.Second)
	st := e.wake()
	if reads := readSet(e.gh.takeReads()); !slices.Equal(reads, []string{repoName(1)}) {
		t.Fatalf("woken pass read %v", reads)
	}
	if st.Provisioned != 0 || len(e.liveIn(repoID(1))) != 0 {
		t.Fatalf("created past the cap while job 7's submission was unresolved: %+v", st)
	}
}

// After a restart, a woken pass before any tick pass has read the whole set
// reads only the delivered repository and creates nothing: a repository not
// read since startup may hold a registration from before the restart whose
// submission is unresolved, here acme/r17's, and nothing of it is
// remembered to hold its slot.
func TestWokenPassBeforeTheFirstTickPassCreatesNothing(t *testing.T) {
	e := newDiscoveryEnv(t, 30, replace(`"maxRunners": 10`, `"maxRunners": 1`))
	name := runnerIn(repoID(17), 9, 0)
	e.gh.addRunner(repoName(17), 77, name)
	e.gh.setOnline(name, false)
	// The startup tick pass is rate-limited before it reads anything.
	e.gh.rateLimitUntil = e.clock.Now().Add(30 * time.Second)
	if st := e.pass(); st.Error != ErrRateLimited || len(e.gh.takeReads()) != 0 {
		t.Fatalf("startup: %+v", st)
	}
	e.gh.queueIn(repoName(3), 30, 3, "queued", lux3...)
	e.deliver("workflow_job", queuedDelivery(42, repoName(3), 3))
	e.clock.Add(35 * time.Second)
	st := e.wake()
	if reads := readSet(e.gh.takeReads()); !slices.Equal(reads, []string{repoName(3)}) {
		t.Fatalf("woken pass read %v", reads)
	}
	if st.Provisioned != 0 || e.lux.createCount() != 0 || e.gh.jitCount() != 0 {
		t.Fatalf("created before acme/r17 was read: %+v", st)
	}
	// The next tick pass reads every repository; the registration holds the
	// one slot until orphan cleanup deletes it.
	e.clock.Add(25 * time.Second)
	if st := e.pass(); st.Provisioned != 0 || e.gh.takeReads()[repoName(17)] == 0 {
		t.Fatalf("tick: %+v", st)
	}
	for range 4 {
		e.clock.Add(time.Minute)
		if st := e.pass(); st.Provisioned == 1 {
			if !slices.Contains(e.gh.deletedNames(), name) {
				t.Fatalf("created while %s was unresolved", name)
			}
			return
		}
	}
	t.Fatal("never created once acme/r17 was read")
}

// A tick buffered while Run waits out minGap for a wake makes that pass a
// tick pass, which reads the hot set; without one the pass is woken.
func TestFoldedTickMakesAWokenPassATickPass(t *testing.T) {
	for _, folded := range []bool{false, true} {
		t.Run(fmt.Sprintf("folded=%v", folded), func(t *testing.T) {
			e := newWokenEnv(t)
			tick := make(chan time.Time, 1)
			e.rec.ticker = func(time.Duration) (<-chan time.Time, func()) { return tick, func() {} }
			l := e.startLoop()
			l.pass()
			e.gh.takeReads()
			e.deliver("workflow_job", queuedDelivery(42, repoName(7), 17))
			if d := l.gap(); d != 5*time.Second {
				t.Fatalf("gap %v", d)
			}
			if folded {
				tick <- e.clock.Now()
			}
			e.clock.Add(5 * time.Second)
			l.fire <- e.clock.Now()
			l.pass()
			reads := e.gh.takeReads()
			if reads[repoName(7)] == 0 {
				t.Fatalf("the delivered repository was not read: %v", readSet(reads))
			}
			if hot := reads[repoName(0)] > 0 && reads[repoName(3)] > 0; hot != folded {
				t.Fatalf("hot set read=%v with folded=%v: %v", hot, folded, readSet(reads))
			}
		})
	}
}

// With an explicit list a woken pass reads every listed repository, as a
// tick pass does.
func TestExplicitListWokenPassReadsEveryRepository(t *testing.T) {
	e := newEnv(t, twoRepos(10))
	e.gh.addRepo("acme/gadgets", 1002)
	e.gh.queueIn("acme/gadgets", 70, 7, "queued", lux3...)
	e.pass()
	e.gh.takeReads()
	e.gh.queue(8, "queued", lux3...)
	e.deliver("workflow_job", queuedDelivery(42, widgets, 8))
	e.clock.Add(5 * time.Second)
	woken := e.wake()
	wokenReads := e.gh.takeReads()
	e.clock.Add(time.Minute)
	e.pass()
	tickReads := e.gh.takeReads()
	if !slices.Equal(readSet(wokenReads), []string{"acme/gadgets", widgets}) || wokenReads["acme/gadgets"] != tickReads["acme/gadgets"] {
		t.Fatalf("woken reads %v, tick reads %v", wokenReads, tickReads)
	}
	if woken.Provisioned != 1 || len(e.liveIn(1001)) != 1 {
		t.Fatalf("woken: %+v", woken)
	}
}

// A delivery whose repository no pass read before hotWindow ran out (a
// rate-limit pause longer than hotWindow) is still read by the next tick
// pass, not left to the cold rotation.
func TestDeliveryOutlastingHotWindowIsReadByTheNextTickPass(t *testing.T) {
	for _, pause := range []time.Duration{90 * time.Second, 3 * time.Minute} {
		t.Run(pause.String(), func(t *testing.T) {
			e := newDiscoveryEnv(t, 30)
			e.pass()
			e.gh.takeReads()
			e.gh.rateLimitUntil = e.clock.Now().Add(pause)
			e.deliver("workflow_job", queuedDelivery(42, repoName(27), 27))
			if st := e.pass(); st.Error != ErrRateLimited {
				t.Fatalf("status %+v", st)
			}
			e.gh.takeReads()
			e.clock.Add(pause + time.Second)
			e.pass()
			if e.gh.takeReads()[repoName(27)] == 0 {
				t.Fatalf("acme/r27 delivered during a %s pause was not read by the first tick pass after it", pause)
			}
		})
	}
}
