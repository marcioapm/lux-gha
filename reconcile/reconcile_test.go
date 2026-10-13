package reconcile

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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
	e := &env{t: t, clock: clock, gh: newFakeGitHub(clock), lux: newFakeLux(clock), cfg: cfg}
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

// wake runs one woken pass, as Run does for a wake with no tick due.
func (e *env) wake() Status {
	e.t.Helper()
	return e.rec.runPass(context.Background(), true)
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
	e.gh.editJob(7, func(j *fakeJob) { j.workflow, j.name, j.attempt = "Lib (Java)", "build", 2 })
	branch := "lux/lib-on-lux"
	e.gh.describeRun(githubapp.WorkflowRun{ID: 70, HeadBranch: &branch, RunNumber: 12})
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
	identity := map[string]string{LabelManaged: "t1", LabelRepo: "1001", LabelRoute: "default", LabelJob: "7", LabelGen: "0", LabelRunner: "lux-gha-t1-1001-7-0"}
	descriptive := map[string]string{LabelRepoName: "acme/widgets", LabelWorkflow: "Lib (Java)", LabelJobName: "build",
		LabelRunID: "70", LabelRunAttempt: "2", LabelBranch: "lux/lib-on-lux", LabelSHA: "000000000000",
		LabelApp: "lux-gha"}
	wantLabels := maps.Clone(identity)
	maps.Copy(wantLabels, descriptive)
	if !mapsEqual(r.run.Labels, wantLabels) {
		t.Fatalf("labels %v", r.run.Labels)
	}
	if r.key != "api.github.com/t1/1001/7/0" {
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
			Grace   string
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
	if w.Name != "widgets · Lib (Java) / build · lux/lib-on-lux run 12" || w.Image.Ref != "ghcr.io/acme/runner@sha256:abc" || w.Workload.Adapter != "generic" ||
		!slices.Equal(w.Workload.Command, []string{"/usr/local/bin/lux-gha-runner"}) || w.Workload.Grace != "1m0s" || w.Secrets[0].As != "file" ||
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

func TestUnrestrictedEgressRouteSubmitsUnrestrictedNetwork(t *testing.T) {
	e := newEnv(t, replace(`"egressHosts": ["github.com", "*.actions.githubusercontent.com"]`, `"unrestrictedEgress": true`))
	e.gh.queue(7, "queued", "self-hosted", "lux")
	e.gh.queue(8, "queued", "lux-big")
	if st := e.pass(); st.Error != "" || st.Provisioned != 2 {
		t.Fatalf("status %+v", st)
	}
	network := map[string]string{}
	for _, r := range e.lux.all() {
		wire, err := json.Marshal(r.spec)
		if err != nil {
			t.Fatal(err)
		}
		var w struct{ Network json.RawMessage }
		if err := json.Unmarshal(wire, &w); err != nil {
			t.Fatal(err)
		}
		network[r.run.Labels[LabelRoute]] = string(w.Network)
	}
	// "big" sets neither field: default deny with no rules, so no network.
	if network["default"] != `{"unrestricted":true}` || network["big"] != `{}` {
		t.Fatalf("network per route %v", network)
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
	e := newEnv(t, replace(`"maxRunners": 10`, `"maxRunners": 10, "reconcile": {"stopGrace": "3m"}`))
	e.gh.queue(8, "queued", "lux-big")
	e.pass()
	r := e.lux.all()
	if len(r) != 1 || !slices.Equal(r[0].spec.Command, []string{"/opt/start"}) || r[0].spec.Pool != "gha-big" ||
		!r[0].spec.Sandbox.NestedContainers || r[0].run.Labels[LabelRoute] != "big" || r[0].spec.Timeout != 2*time.Hour ||
		r[0].spec.Grace != 3*time.Minute {
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

func TestListedJobWithHostileNamesIsProvisionedWithBoundedLabels(t *testing.T) {
	e := newEnv(t)
	long := hostile()
	e.gh.queue(7, "queued", lux3...)
	e.gh.editJob(7, func(j *fakeJob) { j.workflow, j.name, j.attempt = long, long, 1 })
	e.gh.describeRun(githubapp.WorkflowRun{ID: 70, HeadBranch: &long, RunNumber: 4, PullRequests: []githubapp.PullRequestRef{pullRequest(34, metaRepoID)}})
	if st := e.pass(); st.Error != "" || st.Provisioned != 1 {
		t.Fatalf("status %+v", st)
	}
	r := e.lux.all()[0]
	if err := r.spec.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(r.spec.Name) > maxRunName || !strings.HasSuffix(r.spec.Name, "… run 4") {
		t.Fatalf("name %d bytes %q", len(r.spec.Name), r.spec.Name)
	}
	for _, k := range []string{LabelWorkflow, LabelJobName, LabelBranch} {
		if v := r.run.Labels[k]; v == "" || len(v) > luxclient.MaxLabelValue || strings.ContainsAny(v, "\n\x00\x1b") {
			t.Fatalf("%s = %q", k, v)
		}
	}
	if r.run.Labels[LabelPR] != "34" || r.run.Labels[LabelRunner] != runner("7", 0) || r.run.Labels[LabelJob] != "7" || r.key != "api.github.com/t1/1001/7/0" {
		t.Fatalf("labels %v key %q", r.run.Labels, r.key)
	}
}

// A submission whose outcome was unknown is replayed with the same key after
// the job's metadata changed; Lux answers with the first Run, whose name and
// descriptive labels are the old ones. It is still this job's Run.
func TestReplayAfterMetadataChangeStillCarries(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	e.gh.editJob(7, func(j *fakeJob) { j.workflow, j.name = "CI", "build" })
	old := "main"
	e.gh.describeRun(githubapp.WorkflowRun{ID: 70, HeadBranch: &old, RunNumber: 1})
	e.lux.lateCommit = 1 << 20 // commits only when its key is submitted again
	if st := e.pass(); st.Error != ErrLux || e.lux.createCount() != 1 {
		t.Fatalf("status %+v", st)
	}
	renamed := "release"
	e.gh.editJob(7, func(j *fakeJob) { j.workflow, j.name = "CI renamed", "build all" })
	e.gh.describeRun(githubapp.WorkflowRun{ID: 70, HeadBranch: &renamed, RunNumber: 1, PullRequests: []githubapp.PullRequestRef{pullRequest(9, metaRepoID)}})
	// The runner never connects; orphan cleanup deletes it after idleGrace
	// and the job is submitted again under the same key.
	var st Status
	for range 6 {
		st = e.pass()
		if st.Provisioned > 0 {
			break
		}
		e.clock.Add(time.Minute)
	}
	if st.Error != "" || st.Provisioned != 1 || e.lux.createCount() != 1 {
		t.Fatalf("status %+v creates=%d", st, e.lux.createCount())
	}
	r := e.lux.all()[0]
	if r.key != "api.github.com/t1/1001/7/0" || r.spec.Name != "widgets · CI / build · main run 1" ||
		r.run.Labels[LabelBranch] != "main" || r.run.Labels[LabelPR] != "" || r.run.Labels[LabelGen] != "0" {
		t.Fatalf("key %q name %q labels %v", r.key, r.spec.Name, r.run.Labels)
	}
	if !slices.Equal(e.gh.deletedNames(), []string{runner("7", 0)}) || len(e.lux.liveForJob("7")) != 1 {
		t.Fatalf("deleted=%v live=%v", e.gh.deletedNames(), e.lux.liveForJob("7"))
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

func TestTruncatedDemandProvisionsWithinBoundAndRemovesNothing(t *testing.T) {
	e := newEnv(t, replace(`"maxRunners": 10`, `"maxRunners": 10, "reconcile": {"maxJobsPerPass": 4}`))
	e.gh.queue(1, "queued", lux3...)
	e.pass()
	e.gh.complete(1, "cancelled")
	e.gh.queue(2, "queued", lux3...)
	e.gh.queue(3, "queued", lux3...)
	e.gh.queue(4, "queued", lux3...)
	e.pass()
	e.clock.Add(time.Hour)
	st := e.pass()
	// Job 4 was beyond the bound, so job 1's idle runner may be its supply.
	if e.lux.createCount() != 3 || len(e.gh.deletedNames()) != 0 || len(e.lux.terminatedIDs()) != 0 {
		t.Fatalf("status %+v creates=%d deleted=%v terminated=%v", st, e.lux.createCount(), e.gh.deletedNames(), e.lux.terminatedIDs())
	}
}

func TestGitHubErrorSkipsTheRepositoryWithoutCleanup(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	e.pass()
	e.gh.complete(7, "cancelled")
	e.pass()
	e.clock.Add(time.Hour)
	e.gh.fail = func(op, _ string, _ int) error {
		if op == opRunners {
			return &githubapp.APIError{StatusCode: 500}
		}
		return nil
	}
	e.pass()
	e.clock.Add(time.Hour)
	st := e.pass()
	if st.Error != ErrGitHub || len(e.gh.deletedNames()) != 0 || len(e.lux.terminatedIDs()) != 0 {
		t.Fatalf("status %+v deleted=%v terminated=%v", st, e.gh.deletedNames(), e.lux.terminatedIDs())
	}
}

func TestRunnerThatTurnsBusyBeforeDeletionKeepsItsRun(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	e.pass()
	e.gh.complete(7, "cancelled")
	e.pass()
	e.clock.Add(3 * time.Minute)
	// Job 8 arrives after the pass listed demand and runners; job 7's runner
	// takes it just before the bridge deletes it.
	e.gh.beforeDelete = func() {
		rn := e.gh.runnerByName(runner("7", 0))
		rn.Busy = true
		e.gh.repos[widgets].jobs[8] = &fakeJob{id: 8, runID: 80, status: "in_progress", labels: lux3, runner: rn.Name, runnerID: rn.ID}
	}
	e.pass()
	if len(e.gh.deletedNames()) != 0 || len(e.lux.terminatedIDs()) != 0 || len(e.lux.liveForJob("7")) != 1 {
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

func TestLuxCreateFailureWaitsForOrphanCleanupThenReprovisions(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	e.lux.failCreate = 1
	st := e.pass()
	if st.Error != ErrLux || e.gh.jitCount() != 1 || e.lux.createCount() != 0 {
		t.Fatalf("status %+v jit=%d", st, e.gh.jitCount())
	}
	// gen 0's runner has no Run: its submission's outcome is unknown, so no
	// other gen is provisioned until orphan cleanup removes it.
	e.pass()
	e.clock.Add(2*time.Minute - time.Second)
	e.pass()
	if e.lux.createCount() != 0 || e.gh.jitCount() != 1 || len(e.gh.deletedNames()) != 0 {
		t.Fatalf("creates=%d jit=%d deleted=%v", e.lux.createCount(), e.gh.jitCount(), e.gh.deletedNames())
	}
	e.clock.Add(time.Second)
	e.pass()
	if !slices.Equal(e.gh.deletedNames(), []string{runner("7", 0)}) || e.lux.createCount() != 0 {
		t.Fatalf("deleted=%v creates=%d", e.gh.deletedNames(), e.lux.createCount())
	}
	e.pass()
	live := e.lux.liveForJob("7")
	if len(live) != 1 || live[0].run.Labels[LabelRunner] != runner("7", 0) || live[0].key != "api.github.com/t1/1001/7/0" {
		t.Fatalf("reprovisioned %+v", live)
	}
	// gen 0 is lost before pickup while its runner stays registered: gen 1
	// follows at once, not a replay of gen 0's Idempotency-Key.
	e.lux.end(live[0].run.ID, "its secrets are no longer held")
	e.gh.setOnline(runner("7", 0), false)
	e.pass()
	live = e.lux.liveForJob("7")
	if len(live) != 1 || live[0].run.Labels[LabelGen] != "1" || e.lux.createCount() != 2 {
		t.Fatalf("after loss: live %+v creates=%d", live, e.lux.createCount())
	}
	e.clock.Add(2 * time.Minute)
	e.pass()
	if !slices.Equal(e.gh.runnerNames(), []string{runner("7", 1)}) || len(e.lux.terminatedIDs()) != 0 {
		t.Fatalf("runners=%v terminated=%v", e.gh.runnerNames(), e.lux.terminatedIDs())
	}
}

func TestJITConflictDeletesRunnerByNameThenProvisions(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	// A runner of this name was registered by a pass that died before Create,
	// and the full listing does not show it yet.
	e.gh.GenerateRepoJITConfig(context.Background(), e.repo(), jitReq(runner("7", 0)))
	e.gh.repos[widgets].unlisted[runner("7", 0)] = true
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

func TestRunnerThatServedAnotherJobLeadsToNextGen(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	e.pass()
	// Job 9 is queued and picked by job 7's runner before any pass sees it.
	e.gh.queue(9, "queued", lux3...)
	if err := e.gh.pick(runner("7", 0), 9); err != nil {
		t.Fatal(err)
	}
	e.gh.complete(9, "success")
	e.lux.end(e.runID("7"), "")
	// Job 7 is still queued; its gen-0 runner and Run are both gone.
	e.pass()
	live := e.lux.liveForJob("7")
	if len(live) != 1 || live[0].run.Labels[LabelGen] != "1" || live[0].key != "api.github.com/t1/1001/7/1" {
		t.Fatalf("live %+v", live)
	}
	if err := e.gh.pick(runner("7", 1), 7); err != nil {
		t.Fatal(err)
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
	if e.lux.createCount() != 1 || len(e.lux.liveForJob("2")) != 1 {
		t.Fatalf("creates=%d", e.lux.createCount())
	}

	e.rec = e.newReconciler()
	st := e.pass()
	if st.Provisioned != 0 || e.lux.createCount() != 1 || e.gh.jitCount() != 2 {
		t.Fatalf("restart duplicated: %+v creates=%d jit=%d", st, e.lux.createCount(), e.gh.jitCount())
	}
	e.gh.complete(2, "cancelled")
	e.pass()
	e.clock.Add(2 * time.Minute)
	e.pass()
	// Job 1's orphan is deleted; until job 1 has a Run, job 2's idle runner
	// may serve it, so it is not surplus yet.
	if !slices.Equal(e.gh.deletedNames(), []string{runner("1", 0)}) || len(e.lux.terminatedIDs()) != 0 {
		t.Fatalf("deleted=%v terminated=%v", e.gh.deletedNames(), e.lux.terminatedIDs())
	}
	e.pass()
	deleted := e.gh.deletedNames()
	slices.Sort(deleted)
	if !slices.Equal(deleted, []string{runner("1", 0), runner("2", 0)}) || len(e.lux.terminatedIDs()) != 1 {
		t.Fatalf("deleted=%v terminated=%v", deleted, e.lux.terminatedIDs())
	}
	if live := e.lux.live(); len(live) != 1 || live[0].run.Labels[LabelRunner] != runner("1", 0) {
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

// loop runs e.rec.Run with every pass held at entry until released, and the
// wait for the minimum gap between passes under the test's control.
type loop struct {
	t                *testing.T
	entered, exited  chan struct{}
	release, stop    chan struct{}
	afters           chan time.Duration
	fire             chan time.Time
	cancel           context.CancelFunc
	done             chan struct{}
	stopped, aborted bool
}

func (e *env) startLoop() *loop {
	l := &loop{t: e.t, entered: make(chan struct{}), exited: make(chan struct{}), release: make(chan struct{}),
		stop: make(chan struct{}), afters: make(chan time.Duration, 16), fire: make(chan time.Time), done: make(chan struct{})}
	e.rec.passHook = func(enter bool) {
		ch := l.exited
		if enter {
			ch = l.entered
		}
		select {
		case ch <- struct{}{}:
		case <-l.stop:
			return
		}
		if enter {
			select {
			case <-l.release:
			case <-l.stop:
			}
		}
	}
	e.rec.after = func(d time.Duration) <-chan time.Time { l.afters <- d; return l.fire }
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	go func() { e.rec.Run(ctx); close(l.done) }()
	e.t.Cleanup(l.shutdown)
	return l
}

func (l *loop) shutdown() {
	if l.stopped {
		return
	}
	l.stopped = true
	close(l.stop)
	l.cancel()
	select {
	case <-l.done:
	case <-time.After(10 * time.Second):
		l.t.Error("Run did not return after cancellation")
	}
}

func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

// pass lets one held pass run to completion.
func (l *loop) pass() {
	l.t.Helper()
	await(l.t, l.entered, "a pass to start")
	l.release <- struct{}{}
	await(l.t, l.exited, "the pass to end")
}

// gap returns the wait Run requested before its next pass, failing if a
// pass starts without one.
func (l *loop) gap() time.Duration {
	l.t.Helper()
	select {
	case d := <-l.afters:
		select {
		case <-l.entered:
			l.t.Fatal("a pass started while Run waited for the gap")
		default:
		}
		return d
	case <-l.entered:
		l.t.Fatal("a pass started without waiting for the minimum gap")
	case <-time.After(10 * time.Second):
		l.t.Fatal("Run did not wait for its next pass")
	}
	return 0
}

func TestWebhookWakesARunLoopWithoutWaitingForThePass(t *testing.T) {
	e := newEnv(t, replace(`"maxRunners": 10`, `"maxRunners": 10, "reconcile": {"interval": "1h"}`))
	l := e.startLoop()
	await(t, l.entered, "the startup pass")

	// The startup pass is held; deliveries must not wait for it.
	secret := []byte("hook-secret")
	h, err := webhook.New(secret, 42, e.cfg.AllowsRepository, func(context.Context, webhook.WorkflowJobEvent) error {
		e.rec.Wake()
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"action":"queued","installation":{"id":42},"repository":{"full_name":"acme/widgets"},
		"workflow_job":{"id":7,"run_id":70,"run_attempt":1,"status":"queued","labels":["self-hosted","lux"]}}`)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	for range 3 {
		codes := make(chan int, 1)
		go func() {
			req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-GitHub-Event", "workflow_job")
			req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			codes <- rec.Code
		}()
		if code := await(t, codes, "the webhook handler"); code != http.StatusAccepted {
			t.Fatalf("webhook status %d", code)
		}
	}
	l.release <- struct{}{}
	await(t, l.exited, "the startup pass to end")
	e.gh.queue(7, "queued", lux3...)
	// The three wakes coalesce into one pass after the minimum gap (1h/12).
	if d := l.gap(); d != 5*time.Minute {
		t.Fatalf("gap %v", d)
	}
	e.clock.Add(5 * time.Minute)
	l.fire <- e.clock.Now()
	l.pass()
	if e.lux.createCount() != 1 || e.gh.jitCount() != 1 {
		t.Fatalf("creates=%d jit=%d", e.lux.createCount(), e.gh.jitCount())
	}
}

func TestWakesDuringAndAfterAPassWaitForTheMinimumGap(t *testing.T) {
	e := newEnv(t, replace(`"maxRunners": 10`, `"maxRunners": 10, "reconcile": {"interval": "2m"}`))
	l := e.startLoop()
	await(t, l.entered, "the startup pass")
	e.clock.Add(3 * time.Second)
	for range 5 {
		e.rec.Wake()
	}
	l.release <- struct{}{}
	await(t, l.exited, "the startup pass to end")
	for range 5 {
		e.rec.Wake()
	}
	// minGap is 2m/12 = 10s from the start of the last pass, 3s ago.
	if d := l.gap(); d != 7*time.Second {
		t.Fatalf("gap %v, want 7s", d)
	}
	// A wake during the gap wait is served by the pass the wait leads to.
	e.rec.Wake()
	e.clock.Add(7 * time.Second)
	l.fire <- e.clock.Now()
	l.pass()
	// The 2m tick is far off, so with the wake consumed Run stays idle. The
	// window is real time: a stale wake makes Run ask for a gap at once.
	select {
	case d := <-l.afters:
		t.Fatalf("a consumed wake led to another gap request of %v", d)
	case <-l.entered:
		t.Fatal("a pass started without a wake or tick")
	case <-time.After(200 * time.Millisecond):
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

// assertOneLive fails when a job has more than one live Run.
func (e *env) assertAtMostOneLive(job string) {
	e.t.Helper()
	if live := e.lux.liveForJob(job); len(live) > 1 {
		e.t.Fatalf("job %s has %d live Runs", job, len(live))
	}
}

func TestSubmissionWithUnknownOutcomeNeverYieldsTwoLiveRuns(t *testing.T) {
	// The failed Create's Run commits at the Nth Lux List after it: 0 is the
	// next pass's supply read, 1 its per-job history read, 2 the following
	// pass's supply read (after this pass's history read showed nothing).
	for _, tc := range []struct {
		name    string
		lists   int
		restart bool
	}{
		{"before supply read", 0, false},
		{"between supply and history read", 1, false},
		{"after history read", 2, false},
		{"after history read across a restart", 2, true},
		{"between reads across a restart", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.gh.queue(7, "queued", lux3...)
			e.lux.lateCommit = tc.lists
			if st := e.pass(); st.Error != ErrLux || e.lux.createCount() != 1 || len(e.lux.all()) != 0 {
				t.Fatalf("status %+v runs %+v", st, e.lux.all())
			}
			if tc.restart {
				e.rec = e.newReconciler()
			}
			for range 6 {
				e.pass()
				e.assertAtMostOneLive("7")
				e.clock.Add(30 * time.Second)
			}
			live := e.lux.liveForJob("7")
			if len(live) != 1 || live[0].run.Labels[LabelGen] != "0" || e.lux.createCount() != 1 || e.gh.jitCount() != 1 {
				t.Fatalf("live %+v creates=%d jit=%d", live, e.lux.createCount(), e.gh.jitCount())
			}
			if len(e.gh.deletedNames()) != 0 || len(e.lux.terminatedIDs()) != 0 {
				t.Fatalf("deleted=%v terminated=%v", e.gh.deletedNames(), e.lux.terminatedIDs())
			}
			if err := e.gh.pick(runner("7", 0), 7); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLiveRunFromHistoryReadCountsAgainstCaps(t *testing.T) {
	e := newEnv(t, replace(`"maxRunners": 10`, `"maxRunners": 1`))
	e.gh.queue(7, "queued", lux3...)
	e.gh.queue(8, "queued", lux3...)
	e.lux.lateCommit = 1
	e.pass()
	// Job 7's Run appears at the history read; it fills the global cap of 1.
	st := e.pass()
	if st.Provisioned != 0 || st.Live["default"] != 1 || e.lux.createCount() != 1 || len(e.lux.liveForJob("8")) != 0 {
		t.Fatalf("status %+v creates=%d", st, e.lux.createCount())
	}
}

func TestUnresolvedSubmissionHoldsACapSlot(t *testing.T) {
	big := []string{"self-hosted", "linux", "lux-big"}
	globalCap := []func(string) string{replace(`"maxRunners": 10`, `"maxRunners": 1`)}
	// The big route has maxRunners 1 under a global cap of 10.
	type job struct {
		repo   string
		id     int64
		runner string
	}
	j7, j8, j9 := job{widgets, 7, runner("7", 0)}, job{widgets, 8, runner("8", 0)}, job{widgets, 9, runner("9", 0)}
	gadgets9 := job{"acme/gadgets", 9, "lux-gha-t1-1002-9-0"}
	truncate := func(maxRunners string) []func(string) string {
		return []func(string) string{replace(`"maxRunners": 10`, `"maxRunners": `+maxRunners+`, "reconcile": {"maxJobsPerPass": 2}`)}
	}
	// What happens to the unresolved job's trigger while its submission is
	// in flight; "truncated" leaves it queued but sorts it after the fresh
	// job beyond the queued status's allowance of 1.
	const (
		queued    = "queued"
		cancelled = "cancelled"
		picked    = "picked elsewhere"
		truncated = "truncated"
	)
	type testCase struct {
		name              string
		edits             []func(string) string
		labels            []string
		restart           bool
		unresolved, fresh job
		lateCommit        int
		trigger           string
	}
	// The unresolved job's Create fails in the first pass; the fresh job is
	// queued after it. In the second pass the Lux Lists are the supply read
	// and one history read per job, in repo then job order. lateCommit 2
	// commits the Run at the second history read; 3 at the third pass's
	// supply read, after the second pass provisioned whatever it would.
	cases := []testCase{
		{"global cap", globalCap, lux3, false, j7, j8, 2, queued},
		{"global cap across a restart", globalCap, lux3, true, j7, j8, 2, queued},
		{"route cap", nil, big, false, j7, j8, 2, queued},
		{"route cap across a restart", nil, big, true, j7, j8, 2, queued},
		{"global cap, unresolved job sorts last", globalCap, lux3, false, j9, j8, 3, queued},
		{"global cap, unresolved job sorts last, across a restart", globalCap, lux3, true, j9, j8, 3, queued},
		{"route cap, unresolved job sorts last", nil, big, false, j9, j8, 3, queued},
		{"route cap, unresolved job sorts last, across a restart", nil, big, true, j9, j8, 3, queued},
		{"global cap, unresolved job in the second repository", []func(string) string{twoRepos(1)}, lux3, false, gadgets9, j8, 3, queued},
		{"global cap, unresolved job in the second repository, across a restart", []func(string) string{twoRepos(1)}, lux3, true, gadgets9, j8, 3, queued},
		{"route cap, unresolved job in the second repository", []func(string) string{twoRepos(10)}, big, false, gadgets9, j8, 3, queued},
		{"route cap, unresolved job in the second repository, across a restart", []func(string) string{twoRepos(10)}, big, true, gadgets9, j8, 3, queued},
	}
	// For a trigger outside demand, lateCommit 1 commits the Run at the
	// fresh job's history read, 2 at the unresolved registration's history
	// read, and 3 at the third pass's supply read.
	for _, trigger := range []string{cancelled, picked, truncated} {
		for _, c := range []struct {
			name   string
			edits  []func(string) string
			labels []string
		}{
			{"global cap", globalCap, lux3},
			{"route cap", nil, big},
		} {
			edits, u := c.edits, j7
			if trigger == truncated {
				edits, u = truncate(map[bool]string{true: "1", false: "10"}[c.name == "global cap"]), j9
			}
			for _, restart := range []bool{false, true} {
				for _, late := range []int{1, 2, 3} {
					name := fmt.Sprintf("%s, trigger %s, commit at list %d", c.name, trigger, late)
					if restart {
						name += ", across a restart"
					}
					cases = append(cases, testCase{name, edits, c.labels, restart, u, j8, late, trigger})
				}
			}
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.edits...)
			e.gh.addRepo("acme/gadgets", 1002)
			u, f := tc.unresolved, tc.fresh
			uJob, fJob := strconv.FormatInt(u.id, 10), strconv.FormatInt(f.id, 10)
			e.gh.queueIn(u.repo, u.id*10, u.id, "queued", tc.labels...)
			e.lux.lateCommit = tc.lateCommit
			if st := e.pass(); st.Error != ErrLux || e.lux.createCount() != 1 {
				t.Fatalf("status %+v creates=%d", st, e.lux.createCount())
			}
			switch tc.trigger {
			case cancelled:
				e.gh.complete(u.id, "cancelled")
			case picked:
				e.gh.pickElsewhere(u.repo, u.id, 9000, "other-runner")
			}
			e.gh.queueIn(f.repo, f.id*10, f.id, "queued", tc.labels...)
			if tc.restart {
				e.rec = e.newReconciler()
			}
			for range 4 {
				e.pass()
				if live := e.lux.live(); len(live) > 1 {
					t.Fatalf("cap of 1 exceeded: %d live Runs", len(live))
				}
				e.clock.Add(30 * time.Second)
			}
			if len(e.lux.liveForJob(uJob)) != 1 || len(e.lux.liveForJob(fJob)) != 0 || e.lux.createCount() != 1 {
				t.Fatalf("unresolved=%d fresh=%d creates=%d", len(e.lux.liveForJob(uJob)), len(e.lux.liveForJob(fJob)), e.lux.createCount())
			}
			id := e.runID(uJob)
			if tc.trigger == queued || tc.trigger == truncated {
				// The unresolved job's runner serves it and its Run ends, freeing the slot.
				if err := e.gh.pick(u.runner, u.id); err != nil {
					t.Fatal(err)
				}
				e.gh.complete(u.id, "success")
			} else {
				// The runner is lost before taking a job; its ended Run frees the
				// slot and the registration is only an orphan awaiting cleanup.
				e.gh.setOnline(u.runner, false)
			}
			e.lux.end(id, "exited")
			e.pass()
			if len(e.lux.live()) != 1 || len(e.lux.liveForJob(fJob)) != 1 || e.lux.createCount() != 2 {
				t.Fatalf("live=%d fresh=%d creates=%d", len(e.lux.live()), len(e.lux.liveForJob(fJob)), e.lux.createCount())
			}
			if err := e.gh.pick(f.runner, f.id); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func instanceEnv(e *env, instance string) *Reconciler {
	raw := strings.Replace(testConfig, `"instance": "t1"`, `"instance": "`+instance+`"`, 1)
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		e.t.Fatal(err)
	}
	r, err := New(Options{Config: cfg, GitHub: e.gh, Lux: e.lux, Host: "api.github.com", Now: e.clock.Now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func TestInstancesSharingATenantNeverAdoptOrTerminateEachOthersRuns(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	e.pass()
	t1Run := e.runID("7")
	// Instance t2 replaces t1 while job 7 is still queued.
	t2 := instanceEnv(e, "t2")
	for range 4 {
		st := t2.Pass(context.Background())
		if st.Error != "" {
			t.Fatalf("t2 status %+v", st)
		}
		e.clock.Add(time.Minute)
	}
	var t2Runs []fakeRun
	for _, r := range e.lux.liveForJob("7") {
		if r.run.Labels[LabelManaged] == "t2" {
			t2Runs = append(t2Runs, r)
		}
	}
	if len(t2Runs) != 1 || t2Runs[0].key != "api.github.com/t2/1001/7/0" || t2Runs[0].run.Labels[LabelRunner] != "lux-gha-t2-1001-7-0" {
		t.Fatalf("t2 Runs %+v", t2Runs)
	}
	if len(e.lux.terminatedIDs()) != 0 || len(e.lux.liveForJob("7")) != 2 {
		t.Fatalf("terminated=%v; t1 Run %s must stay live", e.lux.terminatedIDs(), t1Run)
	}
}

func TestReplayedRunWithForeignLabelsIsNeitherAdoptedNorTerminated(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	// Another client already used this bridge's key for a different Run.
	foreign := e.lux.seed("api.github.com/t1/1001/7/0", luxclient.RunSpec{Labels: map[string]string{
		LabelManaged: "t0", LabelRepo: "1001", LabelJob: "7", LabelGen: "0", LabelRunner: "lux-gha-t0-1001-7-0"}})
	// The replay stops provisioning; the runner registered for it is then an
	// orphan, and the job is tried again after its cleanup.
	mismatches := 0
	for range 8 {
		st := e.pass()
		if st.Error == ErrRunMismatch {
			mismatches++
		}
		if st.Provisioned != 0 || st.Live["default"] != 0 || st.Terminated != 0 {
			t.Fatalf("status %+v", st)
		}
		e.clock.Add(time.Minute)
	}
	if mismatches < 2 {
		t.Fatalf("mismatches=%d", mismatches)
	}
	if len(e.lux.terminatedIDs()) != 0 || len(e.lux.liveForJob("7")) != 1 || e.lux.liveForJob("7")[0].run.ID != foreign {
		t.Fatalf("terminated=%v live=%+v", e.lux.terminatedIDs(), e.lux.liveForJob("7"))
	}
}

func TestFakeContract(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	first := luxclient.RunSpec{Name: "a", ImageRef: "img@sha256:1", Command: []string{"/x"}, Timeout: time.Hour, Labels: map[string]string{"k": "1"}}
	run1, err := e.lux.Create(ctx, "key", first)
	if err != nil {
		t.Fatal(err)
	}
	e.lux.end(run1.ID, "")
	second := first
	second.Labels = map[string]string{"k": "2"}
	run2, err := e.lux.Create(ctx, "key", second)
	if err != nil || run2.ID != run1.ID || run2.Labels["k"] != "1" || run2.State != "terminated" || e.lux.createCount() != 1 {
		t.Fatalf("replay %+v err=%v creates=%d", run2, err, e.lux.createCount())
	}

	e.gh.queue(7, "queued", lux3...)
	e.gh.queue(8, "queued", "self-hosted", "lux-big")
	e.pass()
	if err := e.gh.pick(runner("7", 0), 8); err == nil {
		t.Fatal("a default runner picked a lux-big job")
	}
	if err := e.gh.pick(runner("7", 0), 7); err != nil {
		t.Fatal(err)
	}
	jobs := func() githubapp.WorkflowJob {
		p, err := e.gh.ListWorkflowRunJobs(ctx, e.repo(), 70, githubapp.WorkflowJobListOptions{Filter: "latest"})
		if err != nil || len(p.Jobs) != 1 {
			t.Fatalf("jobs %+v err=%v", p, err)
		}
		return p.Jobs[0]
	}
	rn := e.gh.runnerByName(runner("7", 0))
	j := jobs()
	if j.RunnerName == nil || *j.RunnerName != runner("7", 0) || j.RunnerID == nil || *j.RunnerID != rn.ID || j.Conclusion != nil {
		t.Fatalf("assigned job %+v", j)
	}
	e.gh.complete(7, "success")
	j = jobs()
	if e.gh.runnerByName(runner("7", 0)) != nil || j.RunnerName == nil || *j.RunnerName != runner("7", 0) || j.Conclusion == nil || *j.Conclusion != "success" {
		t.Fatalf("completed job %+v", j)
	}
	// Repositories are separate: acme/gadgets sees none of acme/widgets' state.
	e.gh.addRepo("acme/gadgets", 1002)
	gadgets := githubapp.Repository{InstallationID: 42, Owner: "acme", Name: "gadgets"}
	runs, _ := e.gh.ListWorkflowRuns(ctx, gadgets, githubapp.WorkflowRunListOptions{Status: "queued"})
	runners, _ := e.gh.ListRepoRunners(ctx, gadgets, githubapp.RunnerListOptions{})
	if len(runs.Runs) != 0 || len(runners.Runners) != 0 {
		t.Fatalf("gadgets sees runs %+v runners %+v", runs, runners)
	}
}

func twoRepos(maxRunners int) func(string) string {
	return func(s string) string {
		s = strings.Replace(s, `"maxRunners": 10`, `"maxRunners": `+strconv.Itoa(maxRunners), 1)
		return strings.Replace(s, `["acme/widgets"]`, `["acme/widgets", "acme/gadgets"]`, 1)
	}
}

func liveJobs(e *env, repo string) []string {
	var jobs []string
	for _, r := range e.lux.live() {
		if r.run.Labels[LabelRepo] == repo {
			jobs = append(jobs, r.run.Labels[LabelJob])
		}
	}
	slices.Sort(jobs)
	return jobs
}

func TestTwoRepositoriesShareTheGlobalCapAndFailIndependently(t *testing.T) {
	e := newEnv(t, twoRepos(3))
	e.gh.addRepo("acme/gadgets", 1002)
	e.gh.queue(1, "queued", lux3...)
	e.gh.queue(2, "queued", lux3...)
	e.gh.queueIn("acme/gadgets", 30, 3, "queued", lux3...)
	e.gh.queueIn("acme/gadgets", 40, 4, "queued", lux3...)
	st := e.pass()
	if st.Demand["default"] != 4 || st.Provisioned != 3 || len(e.lux.live()) != 3 {
		t.Fatalf("status %+v", st)
	}
	if got := liveJobs(e, "1001"); !slices.Equal(got, []string{"1", "2"}) {
		t.Fatalf("widgets Runs %v", got)
	}
	if got := liveJobs(e, "1002"); !slices.Equal(got, []string{"3"}) {
		t.Fatalf("gadgets Runs %v", got)
	}
	// Each runner is registered in its own repository.
	if err := e.gh.pick("lux-gha-t1-1002-3-0", 3); err != nil {
		t.Fatal(err)
	}
	if err := e.gh.pick(runner("1", 0), 3); err == nil {
		t.Fatal("a widgets runner picked a gadgets job")
	}

	// widgets fails to read; gadgets proceeds once job 3 frees a slot.
	e.gh.complete(3, "success")
	e.gh.complete(1, "cancelled")
	e.gh.fail = func(op, repo string, _ int) error {
		if repo == widgets && op == opRunners {
			return &githubapp.APIError{StatusCode: 500}
		}
		return nil
	}
	e.clock.Add(time.Minute)
	e.pass()
	e.clock.Add(2 * time.Minute)
	e.pass() // job 3's Run, whose runner deregistered, frees its slot
	st = e.pass()
	if st.Error != ErrGitHub {
		t.Fatalf("status %+v", st)
	}
	// widgets' unresolved submissions are unknown, so nothing is created.
	if got := liveJobs(e, "1002"); len(got) != 0 || st.Provisioned != 0 {
		t.Fatalf("gadgets Runs %v while widgets was unreadable, status %+v", got, st)
	}
	e.gh.fail = nil
	e.pass()
	if got := liveJobs(e, "1002"); !slices.Equal(got, []string{"4"}) {
		t.Fatalf("gadgets Runs %v once widgets read again", got)
	}
	for _, r := range e.lux.all() {
		if r.run.Labels[LabelRepo] == "1001" && slices.Contains(e.lux.terminatedIDs(), r.run.ID) {
			t.Fatalf("terminated widgets Run %s while widgets was unreadable", r.run.ID)
		}
	}
	if slices.ContainsFunc(e.gh.deletedNames(), func(n string) bool { return strings.HasPrefix(n, "lux-gha-t1-1001-") }) {
		t.Fatalf("deleted widgets runners %v", e.gh.deletedNames())
	}
}

func rateLimitOn(e *env, target string, until time.Time) {
	e.gh.fail = func(op, _ string, _ int) error {
		if op == target {
			return &githubapp.APIError{StatusCode: 403, RateLimited: true, RetryAt: until}
		}
		return nil
	}
}

func TestRateLimitAfterDiscoveryStopsThePassUntilRetryAt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(e *env) // leaves the next pass due to hit target
		op    string
	}{
		{"JIT registration", func(e *env) {
			e.gh.queue(7, "queued", lux3...)
			e.gh.queue(8, "queued", lux3...)
		}, opJIT},
		{"surplus deletion", func(e *env) {
			e.gh.queue(7, "queued", lux3...)
			e.pass()
			e.gh.complete(7, "cancelled")
			e.pass()
			e.clock.Add(3 * time.Minute)
		}, opDelete},
		{"orphan deletion", func(e *env) {
			e.gh.queue(7, "queued", lux3...)
			e.lux.failCreate = 1
			e.pass()
			e.gh.complete(7, "cancelled")
			e.pass()
			e.clock.Add(3 * time.Minute)
		}, opDelete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tc.setup(e)
			creates, jits, deleted := e.lux.createCount(), e.gh.jitCount(), len(e.gh.deletedNames())
			until := e.clock.Now().Add(10 * time.Minute)
			rateLimitOn(e, tc.op, until)
			st := e.pass()
			if st.Error != ErrRateLimited || st.RateLimitedUntil == nil || !st.RateLimitedUntil.Equal(until) {
				t.Fatalf("status %+v", st)
			}
			if e.lux.createCount() != creates || e.gh.jitCount() != jits || len(e.gh.deletedNames()) != deleted || len(e.lux.terminatedIDs()) != 0 {
				t.Fatalf("acted after the limit: creates=%d jit=%d deleted=%v terminated=%v",
					e.lux.createCount(), e.gh.jitCount(), e.gh.deletedNames(), e.lux.terminatedIDs())
			}
			calls := e.gh.callCount()
			e.clock.Add(9 * time.Minute)
			if st := e.pass(); st.Error != ErrRateLimited || e.gh.callCount() != calls {
				t.Fatalf("called GitHub while limited: %d -> %d, %+v", calls, e.gh.callCount(), st)
			}
			e.gh.fail = nil
			e.clock.Add(time.Minute)
			if st := e.pass(); st.Error != "" || st.RateLimitedUntil != nil {
				t.Fatalf("after RetryAt: %+v", st)
			}
		})
	}
}

// failOn fails one operation's page with a transport error, not an HTTP status.
func failOn(target, repo string, page int) func(string, string, int) error {
	return func(op, r string, p int) error {
		if op == target && r == repo && p == page {
			return errors.New("fake: connection reset")
		}
		return nil
	}
}

func TestFailedJobPageSkipsTheRepositoryAndHoldsBackCreation(t *testing.T) {
	e := newEnv(t, twoRepos(10))
	e.gh.addRepo("acme/gadgets", 1002)
	// A cancelled job's idle runner in widgets, past idleGrace.
	e.gh.queue(9, "queued", lux3...)
	e.pass()
	e.gh.complete(9, "cancelled")
	e.pass()
	e.clock.Add(3 * time.Minute)
	// Three queued jobs in one widgets workflow: two pages at two per page.
	for id := int64(1); id <= 3; id++ {
		e.gh.queueIn(widgets, 70, id, "queued", lux3...)
	}
	e.gh.queueIn("acme/gadgets", 80, 4, "queued", lux3...)
	e.gh.fail = failOn(opJobs, widgets, 2)
	st := e.pass()
	if st.Error != ErrGitHub || len(liveJobs(e, "1001")) != 1 || len(e.gh.deletedNames()) != 0 || len(e.lux.terminatedIDs()) != 0 {
		t.Fatalf("acted on widgets: %+v live=%v deleted=%v terminated=%v", st, liveJobs(e, "1001"), e.gh.deletedNames(), e.lux.terminatedIDs())
	}
	// widgets may hide an unresolved submission, so gadgets waits too.
	if got := liveJobs(e, "1002"); len(got) != 0 {
		t.Fatalf("gadgets Runs %v while widgets was partly read", got)
	}
	e.gh.fail = nil
	e.pass()
	if got := liveJobs(e, "1002"); !slices.Equal(got, []string{"4"}) {
		t.Fatalf("gadgets Runs %v once widgets read again", got)
	}
}

func TestFailedJobPageStillChargesQueuedAllowance(t *testing.T) {
	e := newEnv(t, twoRepos(10))
	e.gh.addRepo("acme/gadgets", 1002)
	e.cfg.Reconcile.MaxJobsPerPass = 6
	for id := int64(1); id <= 3; id++ {
		e.gh.queueIn(widgets, 70, id, "queued", lux3...)
	}
	for id := int64(4); id <= 6; id++ {
		e.gh.queueIn("acme/gadgets", 80, id, "queued", lux3...)
	}
	e.gh.fail = failOn(opJobs, widgets, 2)
	st := e.pass()
	if st.Error != ErrGitHub || st.Demand["default"] != 1 || st.Provisioned != 0 || e.lux.createCount() != 0 {
		t.Fatalf("failed-read accounting: %+v creates=%d", st, e.lux.createCount())
	}
}

func TestRunnerListingBeyondItsPageBoundRemovesNothing(t *testing.T) {
	e := newEnv(t, twoRepos(10))
	e.gh.addRepo("acme/gadgets", 1002)
	e.gh.queue(7, "queued", lux3...)
	e.pass()
	id := e.runID("7")
	// 100 other runners sort first and fill all 50 pages of two; job 7's
	// runner, which finished nothing and is still registered, is on page 51.
	for i := int64(1); i <= 2*maxRunnerPages; i++ {
		e.gh.addRunner(widgets, i, "other-"+strconv.FormatInt(i, 10))
	}
	e.gh.queueIn("acme/gadgets", 80, 4, "queued", lux3...)
	for range 3 {
		st := e.pass()
		if st.Error != ErrGitHub {
			t.Fatalf("status %+v", st)
		}
		e.clock.Add(2 * time.Minute)
	}
	if slices.Contains(e.lux.terminatedIDs(), id) || len(e.gh.deletedNames()) != 0 {
		t.Fatalf("terminated=%v deleted=%v", e.lux.terminatedIDs(), e.gh.deletedNames())
	}
	// widgets' registrations are unknown, so gadgets is not provisioned.
	if got := liveJobs(e, "1002"); len(got) != 0 {
		t.Fatalf("gadgets Runs %v while widgets' runners were unread", got)
	}
}

func TestWorkflowRunPageBoundKeepsIdleSupply(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	e.pass()
	e.gh.complete(7, "cancelled")
	// More queued workflow runs than 10 pages of two hold; their jobs are not
	// demand, so the job budget is not what truncates the view.
	for id := int64(100); id < 100+2*maxRunPages+2; id++ {
		e.gh.queue(id, "queued", "self-hosted", "gpu")
	}
	e.pass()
	e.clock.Add(time.Hour)
	e.pass()
	if len(e.gh.deletedNames()) != 0 || len(e.lux.terminatedIDs()) != 0 {
		t.Fatalf("deleted=%v terminated=%v", e.gh.deletedNames(), e.lux.terminatedIDs())
	}
}

func TestQueuedJobBehindAnActiveSiblingGetsARun(t *testing.T) {
	e := newEnv(t)
	e.gh.queueIn(widgets, 70, 1, "in_progress", lux3...)
	e.gh.queueIn(widgets, 70, 2, "queued", lux3...)
	st := e.pass()
	if st.Demand["default"] != 1 || len(e.lux.liveForJob("2")) != 1 || len(e.lux.liveForJob("1")) != 0 {
		t.Fatalf("status %+v live=%+v", st, e.lux.live())
	}
}

func TestRunWhoseTerminateFailedAfterRunnerDeletionIsTerminatedLater(t *testing.T) {
	e := newEnv(t)
	e.gh.queue(7, "queued", lux3...)
	e.pass()
	id := e.runID("7")
	e.gh.complete(7, "cancelled")
	e.pass()
	e.clock.Add(2 * time.Minute)
	e.lux.failTerminate = 1
	st := e.pass()
	if st.Error != ErrLux || !slices.Equal(e.gh.deletedNames(), []string{runner("7", 0)}) || len(e.lux.terminatedIDs()) != 0 {
		t.Fatalf("status %+v deleted=%v terminated=%v", st, e.gh.deletedNames(), e.lux.terminatedIDs())
	}
	e.pass()
	e.clock.Add(2 * time.Minute)
	e.pass()
	if !slices.Equal(e.lux.terminatedIDs(), []string{id}) || e.lux.createCount() != 1 {
		t.Fatalf("terminated=%v creates=%d", e.lux.terminatedIDs(), e.lux.createCount())
	}
}

// A repository that cannot be read may hold a submission with an unknown
// outcome; creating a Run anywhere could exceed a cap once it commits.
func TestUnreadableRepositoryHoldsBackCreation(t *testing.T) {
	big := []string{"self-hosted", "linux", "lux-big"}
	for _, c := range []struct {
		name   string
		edits  []func(string) string
		labels []string
	}{
		{"global cap", []func(string) string{twoRepos(1)}, lux3},
		{"route cap", []func(string) string{twoRepos(10)}, big},
	} {
		for _, restart := range []bool{false, true} {
			name := c.name
			if restart {
				name += " across a restart"
			}
			t.Run(name, func(t *testing.T) {
				e := newEnv(t, c.edits...)
				e.gh.addRepo("acme/gadgets", 1002)
				e.gh.queue(7, "queued", c.labels...)
				e.lux.lateCommit = 1
				if st := e.pass(); st.Error != ErrLux || e.lux.createCount() != 1 {
					t.Fatalf("status %+v creates=%d", st, e.lux.createCount())
				}
				e.gh.complete(7, "cancelled")
				e.gh.queueIn("acme/gadgets", 80, 8, "queued", c.labels...)
				e.gh.fail = func(op, repo string, _ int) error {
					if repo == widgets && op == opRunners {
						return &githubapp.APIError{StatusCode: 500}
					}
					return nil
				}
				if restart {
					e.rec = e.newReconciler()
				}
				for range 3 {
					e.pass()
					if live := e.lux.live(); len(live) > 1 {
						t.Fatalf("cap of 1 exceeded: %d live Runs", len(live))
					}
				}
				if e.lux.createCount() != 1 {
					t.Fatalf("creates=%d while widgets was unreadable", e.lux.createCount())
				}
			})
		}
	}
}

// GitHub reports its default labels on every self-hosted runner, so an
// unresolved runner's labels can contain several routes; each holds a slot.
func TestUnresolvedRunnerHoldsASlotOnEveryRouteItsLabelsContain(t *testing.T) {
	for _, c := range []struct{ name, other string }{
		{"equally specific route", `["ARM64"]`},
		{"more specific route", `["ARM64", "linux"]`},
	} {
		for _, restart := range []bool{false, true} {
			name := c.name
			if restart {
				name += " across a restart"
			}
			t.Run(name, func(t *testing.T) {
				e := newEnv(t,
					replace(`"labels": ["self-hosted", "linux", "lux"], "pool": "gha",`, `"labels": ["lux"], "pool": "gha", "maxRunners": 1,`),
					replace(`"labels": ["self-hosted", "linux", "lux-big"]`, `"labels": `+c.other))
				e.gh.queue(7, "queued", "lux")
				e.lux.lateCommit = 3
				if st := e.pass(); st.Error != ErrLux || e.lux.createCount() != 1 {
					t.Fatalf("status %+v creates=%d", st, e.lux.createCount())
				}
				e.gh.complete(7, "cancelled")
				e.gh.queue(8, "queued", "lux")
				if restart {
					e.rec = e.newReconciler()
				}
				for range 4 {
					e.pass()
					if live := e.lux.live(); len(live) > 1 {
						t.Fatalf("default route cap of 1 exceeded: %d live Runs", len(live))
					}
					e.clock.Add(30 * time.Second)
				}
				if len(e.lux.liveForJob("8")) != 0 {
					t.Fatalf("job 8 got a Run while job 7's submission held the slot")
				}
			})
		}
	}
}
