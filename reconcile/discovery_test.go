package reconcile

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	e.gh.repo(repoName(1)).uninstalled = true
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

func TestQueuedDeliveryMakesARepositoryHotForHotWindow(t *testing.T) {
	e := newDiscoveryEnv(t, 30)
	e.pass()
	e.gh.takeReads()
	if code := e.deliver("workflow_job", queuedDelivery(42, repoName(7), 7)); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
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

// sweep runs passes a minute apart for d, with a woken pass every 5 s for the
// first wakeFor, and returns, per repository, the longest gap between reads
// and, per pass, the GitHub calls it made.
func (e *env) sweep(d, wakeFor time.Duration, hot []string) (map[string]time.Duration, []int) {
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
		before := e.gh.callCount()
		e.pass()
		calls = append(calls, e.gh.callCount()-before)
		reads := e.gh.takeReads()
		for _, h := range hot {
			if reads[h] == 0 {
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
	startup := e.gh.callCount()
	e.pass()
	startup = e.gh.callCount() - startup
	// Every repository is read once at startup: 3 calls each, plus the
	// listing, the two jobs read and the two JIT registrations.
	if startup > 68*3+10 {
		t.Fatalf("startup pass made %d calls", startup)
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

// A registration whose submission is unresolved holds a cap slot while its
// repository cannot be read, here because it left the installation.
func TestUnresolvedRegistrationInAnUnreadRepositoryHoldsASlot(t *testing.T) {
	e := newDiscoveryEnv(t, 2, replace(`"maxRunners": 10`, `"maxRunners": 1`))
	e.gh.queueIn(repoName(0), 70, 7, "queued", lux3...)
	e.lux.failCreate = 1
	if st := e.pass(); st.Error != ErrLux || e.gh.jitCount() != 1 {
		t.Fatalf("status %+v", st)
	}
	e.gh.repo(repoName(0)).uninstalled = true
	e.deliver("installation_repositories", installationDelivery("removed", 42))
	e.gh.queueIn(repoName(1), 80, 8, "queued", lux3...)
	e.deliver("workflow_job", queuedDelivery(42, repoName(1), 8))
	e.clock.Add(time.Minute)
	e.pass()
	e.clock.Add(time.Minute - time.Second)
	if st := e.pass(); st.Provisioned != 0 || e.gh.takeReads()[repoName(1)] == 0 {
		t.Fatalf("created past the cap while job 7's submission was unresolved: %+v", st)
	}
	// idleGrace after the repository left, its registration is forgotten.
	e.clock.Add(2 * time.Minute)
	e.pass()
	if len(e.liveIn(repoID(1))) != 1 {
		t.Fatalf("runs %v", e.liveIn(repoID(1)))
	}
}
