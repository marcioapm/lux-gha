package reconcile

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux-gha/config"
	"github.com/marcioapm/lux-gha/githubapp"
	"github.com/marcioapm/lux-gha/luxclient"
	"github.com/marcioapm/lux-gha/webhook"
)

var lux3 = []string{"self-hosted", "linux", "lux"}

const testConfig = `{
  "listen": "127.0.0.1:0", "instance": "t1", "maxRunners": 10,
  "github": {"appID": "Iv1.abc", "installationID": 42, "runnerGroupID": 7,
             "privateKeyFile": "/k", "webhookSecretFile": "/w"},
  "lux": {"url": "https://lux.example.com", "apiKeyFile": "/l"},
  "repositories": ["acme/widgets"],
  "routes": [
    {"name": "default", "labels": ["self-hosted", "linux", "lux"], "pool": "gha",
     "image": "ghcr.io/acme/runner@sha256:abc", "cpus": 2, "memoryMiB": 4096, "diskGiB": 20,
     "timeout": "90m", "egressHosts": ["github.com", "*.actions.githubusercontent.com"]},
    {"name": "big", "labels": ["self-hosted", "linux", "lux-big"], "pool": "gha-big",
     "image": "ghcr.io/acme/runner-big@sha256:def", "timeout": "2h", "nestedContainers": true,
     "maxRunners": 1, "command": ["/opt/start"]}
  ]
}`

type env struct {
	t     *testing.T
	clock *fakeClock
	gh    *fakeGitHub
	lux   *fakeLux
	cfg   *config.Config
	rec   *Reconciler
}

func newEnv(t *testing.T, edits ...func(string) string) *env {
	t.Helper()
	raw := testConfig
	for _, e := range edits {
		raw = e(raw)
	}
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	e := &env{t: t, clock: clock, gh: newFakeGitHub(clock), lux: &fakeLux{clock: clock}, cfg: cfg}
	// The runner process connects once its Run starts.
	e.lux.onCreate = func(r luxclient.Run) {
		e.lux.setState(r.ID, "running")
		e.gh.setOnline(r.Labels[LabelRunner], true)
	}
	e.rec = e.newReconciler()
	return e
}

func (e *env) newReconciler() *Reconciler {
	r, err := New(Options{Config: e.cfg, GitHub: e.gh, Lux: e.lux, Host: "api.github.com", Now: e.clock.Now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) pass() Status {
	e.t.Helper()
	return e.rec.Pass(context.Background())
}

func replace(old, new string) func(string) string {
	return func(s string) string { return strings.Replace(s, old, new, 1) }
}

func runner(job string, gen int) string {
	return "lux-gha-t1-1001-" + job + "-" + string(rune('0'+gen))
}

func (e *env) runID(job string) string {
	e.t.Helper()
	live := e.lux.liveForJob(job)
	if len(live) != 1 {
		e.t.Fatalf("job %s: %d live Runs, want 1", job, len(live))
	}
	return live[0].run.ID
}

func TestQueuedJobGetsExactlyOneRunnerAndRun(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", "self-hosted", "lux")
	st := e.pass()
	if st.Error != "" || st.Provisioned != 1 || st.Demand["default"] != 1 {
		t.Fatalf("status %+v", st)
	}
	if e.gh.jitCount() != 1 || e.lux.createCount() != 1 {
		t.Fatalf("jit=%d creates=%d", e.gh.jitCount(), e.lux.createCount())
	}
	req := e.gh.jit[0]
	if req.Name != "lux-gha-t1-1001-7-0" || req.RunnerGroupID != 7 || !slices.Equal(req.Labels, []string{"linux", "lux", "self-hosted"}) {
		t.Fatalf("JIT request %+v", req)
	}
	r := e.lux.all()[0]
	wantLabels := map[string]string{LabelManaged: "t1", LabelRepo: "1001", LabelRoute: "default", LabelJob: "7", LabelGen: "0", LabelRunner: "lux-gha-t1-1001-7-0"}
	if !mapsEqual(r.run.Labels, wantLabels) {
		t.Fatalf("labels %v", r.run.Labels)
	}
	if r.key != "api.github.com/1001/7/0" {
		t.Fatalf("idempotency key %q", r.key)
	}
	s := r.spec
	secret := e.gh.jitSecret["lux-gha-t1-1001-7-0"]
	if len(s.Secrets) != 1 || s.Secrets[0].Value != secret || s.Secrets[0].Path != JITConfigPath {
		t.Fatalf("secrets %+v", s.Secrets)
	}
	if s.Env[JITConfigEnv] != JITConfigPath || len(s.Env) != 1 {
		t.Fatalf("env %v", s.Env)
	}
	wire, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Name     string
		Image    struct{ Ref string }
		Workload struct {
			Adapter string
			Command []string
		}
		Secrets   []struct{ Name, Value, As, Path string }
		Resources struct {
			CPUs   float64
			Memory int64
			Disk   int64
		}
		Timeout      string
		Placement    struct{ Pool string }
		Network      struct{ Egress []struct{ Host string } }
		Sandbox      struct{ NestedContainers bool }
		ResumePolicy string
	}
	if err := json.Unmarshal(wire, &w); err != nil {
		t.Fatal(err)
	}
	if w.Name != "gha acme/widgets job 7" || w.Image.Ref != "ghcr.io/acme/runner@sha256:abc" || w.Workload.Adapter != "generic" ||
		!slices.Equal(w.Workload.Command, []string{"/usr/local/bin/lux-gha-runner"}) || w.Secrets[0].As != "file" ||
		w.Resources.CPUs != 2 || w.Resources.Memory != 4096<<20 || w.Resources.Disk != 20<<30 || w.Timeout != "1h30m0s" ||
		w.Placement.Pool != "gha" || len(w.Network.Egress) != 2 || w.Network.Egress[1].Host != "*.actions.githubusercontent.com" ||
		w.Sandbox.NestedContainers || w.ResumePolicy != "never" {
		t.Fatalf("wire spec %s", wire)
	}
	// The secret appears once in the wire form: as the secret's value.
	if strings.Count(string(wire), secret) != 1 {
		t.Fatalf("JIT config appears outside its secret: %s", wire)
	}

	st = e.pass()
	if st.Provisioned != 0 || e.gh.jitCount() != 1 || e.lux.createCount() != 1 || len(e.lux.terminatedIDs()) != 0 {
		t.Fatalf("second pass changed supply: %+v", st)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestRouteSpecificCommandAndShape(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(8, "queued", "lux-big")
	e.pass()
	r := e.lux.all()
	if len(r) != 1 || !slices.Equal(r[0].spec.Command, []string{"/opt/start"}) || r[0].spec.Pool != "gha-big" ||
		!r[0].spec.Sandbox.NestedContainers || r[0].run.Labels[LabelRoute] != "big" || r[0].spec.Timeout != 2*time.Hour {
		t.Fatalf("runs %+v", r)
	}
}

func TestRepeatedWakesAndStaleEventsNeverDuplicateARun(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	for range 5 {
		e.rec.Wake()
		e.pass()
		e.clock.Add(10 * time.Second)
	}
	if e.lux.createCount() != 1 || e.gh.jitCount() != 1 {
		t.Fatalf("creates=%d jit=%d", e.lux.createCount(), e.gh.jitCount())
	}
	if err := e.gh.pick(runner("7", 0), 7); err != nil {
		t.Fatal(err)
	}
	// A late "queued" delivery for job 7 only wakes a pass; GitHub says in_progress.
	e.rec.Wake()
	e.pass()
	e.gh.complete(7, "success")
	e.rec.Wake()
	e.pass()
	if e.lux.createCount() != 1 || e.gh.jitCount() != 1 {
		t.Fatalf("creates=%d jit=%d after stale events", e.lux.createCount(), e.gh.jitCount())
	}
}

func TestSwappedPickupKeepsBothBusyRunnersAndAddsNone(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(1, "queued", lux3...)
	e.gh.queue(2, "queued", lux3...)
	e.pass()
	if err := e.gh.pick(runner("1", 0), 2); err != nil {
		t.Fatal(err)
	}
	if err := e.gh.pick(runner("2", 0), 1); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		st := e.pass()
		if st.Busy != 2 || st.Idle != 0 {
			t.Fatalf("status %+v", st)
		}
		e.clock.Add(5 * time.Minute)
	}
	if e.lux.createCount() != 2 || len(e.lux.terminatedIDs()) != 0 || len(e.gh.deletedNames()) != 0 {
		t.Fatalf("creates=%d terminated=%v deleted=%v", e.lux.createCount(), e.lux.terminatedIDs(), e.gh.deletedNames())
	}
	// Each runner finishes the other's job, deregisters, and its Run exits.
	e.gh.complete(1, "success")
	e.gh.complete(2, "success")
	for _, r := range e.lux.live() {
		e.lux.end(r.run.ID, "")
	}
	e.pass()
	if e.lux.createCount() != 2 || len(e.lux.terminatedIDs()) != 0 {
		t.Fatalf("creates=%d terminated=%v", e.lux.createCount(), e.lux.terminatedIDs())
	}
}

func TestCancelledJobRunnerRemovedOnlyAfterIdleGrace(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	e.pass()
	id := e.runID("7")
	e.gh.complete(7, "cancelled")

	e.pass()
	e.clock.Add(2*time.Minute - time.Second)
	e.pass()
	if len(e.gh.deletedNames()) != 0 || len(e.lux.terminatedIDs()) != 0 {
		t.Fatalf("removed before idleGrace: deleted=%v terminated=%v", e.gh.deletedNames(), e.lux.terminatedIDs())
	}
	e.clock.Add(time.Second)
	st := e.pass()
	if !slices.Equal(e.gh.deletedNames(), []string{runner("7", 0)}) || !slices.Equal(e.lux.terminatedIDs(), []string{id}) {
		t.Fatalf("deleted=%v terminated=%v", e.gh.deletedNames(), e.lux.terminatedIDs())
	}
	if st.Deleted != 1 || st.Terminated != 1 || e.lux.createCount() != 1 {
		t.Fatalf("status %+v creates=%d", st, e.lux.createCount())
	}
}

func TestUnstartedRunForCancelledJobIsReclaimedAfterGrace(t *testing.T) {
	e := newEnv(t)
	e.lux.onCreate = nil // the Run waits for capacity; its runner never connects
	e.gh.queue(7, "queued", lux3...)
	e.pass()
	id := e.runID("7")
	e.gh.complete(7, "cancelled")
	e.pass()
	e.clock.Add(2 * time.Minute)
	e.pass()
	if !slices.Equal(e.gh.deletedNames(), []string{runner("7", 0)}) || !slices.Equal(e.lux.terminatedIDs(), []string{id}) {
		t.Fatalf("deleted=%v terminated=%v", e.gh.deletedNames(), e.lux.terminatedIDs())
	}
}

func TestSurplusLeavesOneIdleRunnerPerQueuedJob(t *testing.T) {
	e := newEnv(t, replace(`"maxRunners": 10`, `"maxRunners": 2`))
	e.gh.queue(1, "queued", lux3...)
	e.gh.queue(2, "queued", lux3...)
	e.pass()
	e.gh.complete(1, "cancelled")
	e.gh.complete(2, "cancelled")
	// Job 3 is queued but the global cap leaves it without a Run of its own:
	// either idle runner can take it.
	e.gh.queue(3, "queued", lux3...)
	e.pass()
	e.clock.Add(3 * time.Minute)
	st := e.pass()
	if st.Demand["default"] != 1 || len(e.gh.deletedNames()) != 1 || len(e.lux.terminatedIDs()) != 1 {
		t.Fatalf("status %+v deleted=%v terminated=%v", st, e.gh.deletedNames(), e.lux.terminatedIDs())
	}
	if len(e.lux.live()) != 1 || len(e.gh.runnerNames()) != 1 {
		t.Fatalf("live=%d runners=%v", len(e.lux.live()), e.gh.runnerNames())
	}
	// The remaining idle runner is not touched while job 3 is queued.
	e.clock.Add(time.Hour)
	if err := e.gh.pick(e.gh.runnerNames()[0], 3); err != nil {
		t.Fatal(err)
	}
}

func TestLuxCreateFailureReprovisionsAndCleansTheOrphanRunner(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	e.lux.failCreate = 1
	st := e.pass()
	if st.Error != ErrLux || e.gh.jitCount() != 1 || e.lux.createCount() != 0 {
		t.Fatalf("status %+v jit=%d", st, e.gh.jitCount())
	}
	e.pass()
	live := e.lux.liveForJob("7")
	if len(live) != 1 || live[0].run.Labels[LabelGen] != "1" || live[0].run.Labels[LabelRunner] != runner("7", 1) || live[0].key != "api.github.com/1001/7/1" {
		t.Fatalf("reprovisioned %+v", live)
	}
	if len(e.gh.deletedNames()) != 0 {
		t.Fatalf("orphan deleted before idleGrace: %v", e.gh.deletedNames())
	}
	e.clock.Add(2 * time.Minute)
	e.pass()
	if !slices.Equal(e.gh.deletedNames(), []string{runner("7", 0)}) || !slices.Equal(e.gh.runnerNames(), []string{runner("7", 1)}) {
		t.Fatalf("deleted=%v runners=%v", e.gh.deletedNames(), e.gh.runnerNames())
	}
	if len(e.lux.terminatedIDs()) != 0 || e.lux.createCount() != 1 {
		t.Fatalf("terminated=%v creates=%d", e.lux.terminatedIDs(), e.lux.createCount())
	}
	// gen 1 is lost before pickup and its runner is gone: the next gen is 2,
	// not a replay of gen 1's Idempotency-Key.
	e.lux.end(live[0].run.ID, "its secrets are no longer held")
	e.gh.setOnline(runner("7", 1), false)
	e.pass()
	e.clock.Add(2 * time.Minute)
	e.pass()
	if !slices.Contains(e.gh.deletedNames(), runner("7", 1)) {
		t.Fatalf("gen 1 runner not deleted: %v", e.gh.deletedNames())
	}
	live = e.lux.liveForJob("7")
	if len(live) != 1 || live[0].run.Labels[LabelGen] != "2" || e.lux.createCount() != 2 {
		t.Fatalf("after loss: live %+v creates=%d", live, e.lux.createCount())
	}
}

func TestJITConflictDeletesRunnerByNameThenProvisions(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	// A runner of this name was registered by a pass that died before Create,
	// and the full listing does not show it yet.
	e.gh.GenerateRepoJITConfig(context.Background(), e.repo(), jitReq(runner("7", 0)))
	e.gh.unlisted[runner("7", 0)] = true
	e.gh.jit = nil

	st := e.pass()
	if st.Provisioned != 0 || st.Deleted != 1 || !slices.Equal(e.gh.deletedNames(), []string{runner("7", 0)}) || e.lux.createCount() != 0 {
		t.Fatalf("status %+v deleted=%v", st, e.gh.deletedNames())
	}
	st = e.pass()
	if st.Provisioned != 1 || e.lux.all()[0].run.Labels[LabelRunner] != runner("7", 0) {
		t.Fatalf("status %+v", st)
	}
}

func TestPrePickupLossBumpsGenerationUpToTheCap(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	for gen := range 5 {
		e.pass()
		live := e.lux.liveForJob("7")
		if len(live) != 1 || live[0].run.Labels[LabelGen] != string(rune('0'+gen)) {
			t.Fatalf("gen %d: live %+v", gen, live)
		}
		// luxd restarted before placement: the never Run cannot get its secrets back.
		e.lux.end(live[0].run.ID, "its secrets are no longer held")
		e.gh.setOnline(runner("7", gen), false)
	}
	e.pass()
	e.pass()
	if e.lux.createCount() != 5 || e.gh.jitCount() != 5 || len(e.lux.live()) != 0 {
		t.Fatalf("creates=%d jit=%d live=%d", e.lux.createCount(), e.gh.jitCount(), len(e.lux.live()))
	}
	// The registered-but-dead runners are cleaned up after idleGrace.
	e.clock.Add(2 * time.Minute)
	e.pass()
	if len(e.gh.runnerNames()) != 0 || len(e.gh.deletedNames()) != 5 {
		t.Fatalf("runners=%v deleted=%v", e.gh.runnerNames(), e.gh.deletedNames())
	}
	e.pass()
	if e.lux.createCount() != 5 {
		t.Fatalf("provisioned past the cap: creates=%d", e.lux.createCount())
	}
}

func TestMidJobLossProvisionsNothing(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	e.pass()
	if err := e.gh.pick(runner("7", 0), 7); err != nil {
		t.Fatal(err)
	}
	e.lux.end(e.runID("7"), "host lost")
	for range 3 {
		e.pass()
		e.clock.Add(5 * time.Minute)
	}
	if e.lux.createCount() != 1 || e.gh.jitCount() != 1 || len(e.gh.deletedNames()) != 0 {
		t.Fatalf("creates=%d jit=%d deleted=%v", e.lux.createCount(), e.gh.jitCount(), e.gh.deletedNames())
	}
}

func TestOnlyQueuedMatchingJobsWithinCapsGetRuns(t *testing.T) {
	e := newEnv(t, replace(`"maxRunners": 10`, `"maxRunners": 3`))
	e.gh.queue(1, "waiting", lux3...)
	e.gh.queue(2, "queued", "self-hosted", "gpu")
	e.gh.queue(3, "queued", "self-hosted", "linux") // both routes: ambiguous
	e.gh.queue(4, "queued", "lux-big")
	e.gh.queue(5, "queued", "lux-big")
	e.gh.queue(6, "queued", lux3...)
	e.gh.queue(7, "queued", lux3...)
	e.gh.queue(8, "queued", lux3...)
	st := e.pass()
	var jobs []string
	for _, r := range e.lux.live() {
		jobs = append(jobs, r.run.Labels[LabelJob])
	}
	slices.Sort(jobs)
	// big's own cap is 1; the global cap of 3 takes two default jobs.
	if !slices.Equal(jobs, []string{"4", "6", "7"}) || st.Live["big"] != 1 || st.Live["default"] != 2 {
		t.Fatalf("jobs %v status %+v", jobs, st)
	}
	if st.Demand["default"] != 3 || st.Demand["big"] != 2 {
		t.Fatalf("demand %v", st.Demand)
	}
	e.pass()
	if e.lux.createCount() != 3 {
		t.Fatalf("creates=%d", e.lux.createCount())
	}
}

func TestResumePolicyNotEchoedTerminatesAndStopsProvisioning(t *testing.T) {
	e := newEnv(t)
	e.lux.noEcho = true
	e.gh.queue(1, "queued", lux3...)
	e.gh.queue(2, "queued", lux3...)
	st := e.pass()
	all := e.lux.all()
	if st.Error != ErrNeverNotEchoed || len(all) != 1 || all[0].run.State != "terminated" || !slices.Equal(e.lux.terminatedIDs(), []string{all[0].run.ID}) {
		t.Fatalf("status %+v runs %+v", st, all)
	}
	if e.gh.jitCount() != 1 {
		t.Fatalf("jit=%d", e.gh.jitCount())
	}
}

func TestRateLimitStopsThePassUntilRetryAt(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	e.pass()
	e.gh.complete(7, "cancelled")
	e.pass() // surplus timer starts
	e.clock.Add(3 * time.Minute)
	until := e.clock.Now().Add(10 * time.Minute)
	e.gh.rateLimitUntil = until
	e.gh.queue(8, "queued", lux3...)

	st := e.pass()
	if st.Error != ErrRateLimited || st.RateLimitedUntil == nil || !st.RateLimitedUntil.Equal(until) {
		t.Fatalf("status %+v", st)
	}
	calls := e.gh.callCount()
	e.clock.Add(5 * time.Minute)
	st = e.pass()
	if st.Error != ErrRateLimited || e.gh.callCount() != calls {
		t.Fatalf("called GitHub while limited: %d -> %d, %+v", calls, e.gh.callCount(), st)
	}
	if len(e.gh.deletedNames()) != 0 || len(e.lux.terminatedIDs()) != 0 || e.lux.createCount() != 1 {
		t.Fatalf("acted while limited: deleted=%v terminated=%v creates=%d", e.gh.deletedNames(), e.lux.terminatedIDs(), e.lux.createCount())
	}
	e.clock.Add(5 * time.Minute)
	st = e.pass()
	if st.Error != "" || st.RateLimitedUntil != nil || e.lux.createCount() != 2 {
		t.Fatalf("after RetryAt: %+v creates=%d", st, e.lux.createCount())
	}
	// Job 8 has its own runner, so job 7's idle one is surplus.
	if !slices.Equal(e.gh.deletedNames(), []string{runner("7", 0)}) {
		t.Fatalf("after RetryAt: %+v creates=%d deleted=%v", st, e.lux.createCount(), e.gh.deletedNames())
	}
}

func TestRestartRederivesStateWithoutDuplicates(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(1, "queued", lux3...)
	e.gh.queue(2, "queued", lux3...)
	e.lux.failCreate = 1 // job 1's runner is registered without a Run
	e.pass()
	e.pass()
	if e.lux.createCount() != 2 {
		t.Fatalf("creates=%d", e.lux.createCount())
	}

	e.rec = e.newReconciler()
	st := e.pass()
	if st.Provisioned != 0 || e.lux.createCount() != 2 || e.gh.jitCount() != 3 {
		t.Fatalf("restart duplicated: %+v creates=%d jit=%d", st, e.lux.createCount(), e.gh.jitCount())
	}
	e.gh.complete(2, "cancelled")
	e.pass()
	e.clock.Add(2 * time.Minute)
	e.pass()
	deleted := e.gh.deletedNames()
	slices.Sort(deleted)
	if !slices.Equal(deleted, []string{runner("1", 0), runner("2", 0)}) || len(e.lux.terminatedIDs()) != 1 {
		t.Fatalf("deleted=%v terminated=%v", deleted, e.lux.terminatedIDs())
	}
	if live := e.lux.live(); len(live) != 1 || live[0].run.Labels[LabelRunner] != runner("1", 1) {
		t.Fatalf("live %+v", live)
	}
}

func TestFinishedRunWhoseRunnerDeregisteredIsTerminatedAfterGrace(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	e.pass()
	id := e.runID("7")
	e.gh.pick(runner("7", 0), 7)
	e.gh.complete(7, "success") // runner deregisters; its Run has not exited yet
	e.pass()
	e.clock.Add(2*time.Minute - time.Second)
	e.pass()
	if len(e.lux.terminatedIDs()) != 0 {
		t.Fatal("terminated before idleGrace")
	}
	e.clock.Add(time.Second)
	e.pass()
	if !slices.Equal(e.lux.terminatedIDs(), []string{id}) || e.lux.createCount() != 1 {
		t.Fatalf("terminated=%v creates=%d", e.lux.terminatedIDs(), e.lux.createCount())
	}
}

func (e *env) repo() githubapp.Repository {
	return githubapp.Repository{InstallationID: 42, Owner: "acme", Name: "widgets"}
}

func jitReq(name string) githubapp.JITConfigRequest {
	return githubapp.JITConfigRequest{Name: name, RunnerGroupID: 7, Labels: lux3}
}

func TestWebhookWakesARunLoopWithoutWaitingForThePass(t *testing.T) {
	e := newEnv(t, replace(`"maxRunners": 10`, `"maxRunners": 10, "reconcile": {"interval": "1h"}`))
	e.gh.block = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.rec.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	// The first pass is now blocked inside GitHub; deliveries must not wait for it.
	secret := []byte("hook-secret")
	h, err := webhook.New(secret, 42, e.cfg.AllowsRepository, func(context.Context, webhook.WorkflowJobEvent) error {
		e.rec.Wake()
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.gh.queue(7, "queued", lux3...)
	body := []byte(`{"action":"queued","installation":{"id":42},"repository":{"full_name":"acme/widgets"},
		"workflow_job":{"id":7,"run_id":70,"run_attempt":1,"status":"queued","labels":["self-hosted","lux"]}}`)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	for range 3 {
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GitHub-Event", "workflow_job")
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("webhook status %d", rec.Code)
		}
	}
	// Each pass lists queued then in_progress runs, so two receives on
	// block are one pass. Release the startup pass and the woken one.
	for range 4 {
		select {
		case e.gh.block <- struct{}{}:
		case <-time.After(10 * time.Second):
			t.Fatal("pass did not reach GitHub")
		}
	}
	// The three wakes coalesced into that one pass: no third pass starts.
	select {
	case e.gh.block <- struct{}{}:
		t.Fatal("a third pass started")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	<-done
	if e.lux.createCount() != 1 || e.gh.jitCount() != 1 {
		t.Fatalf("creates=%d jit=%d", e.lux.createCount(), e.gh.jitCount())
	}
}

func TestStatusJSONHasCountsAndNoSecret(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	e.pass()
	e.pass()
	rec := httptest.NewRecorder()
	e.rec.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	body := rec.Body.String()
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"time", "durationMs", "demand", "live", "idle", "busy", "provisioned", "terminated", "deleted"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("missing %q in %s", k, body)
		}
	}
	if got["provisioned"] != 0.0 || got["idle"] != 1.0 || got["demand"].(map[string]any)["default"] != 1.0 || got["live"].(map[string]any)["default"] != 1.0 {
		t.Fatalf("body %s", body)
	}
	for _, s := range e.gh.jitSecret {
		if strings.Contains(body, s) {
			t.Fatalf("JIT config in status: %s", body)
		}
	}
	if strings.Contains(body, "SECRET") {
		t.Fatalf("secret in status: %s", body)
	}
}
