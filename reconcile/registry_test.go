package reconcile

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux-gha/internal/identity"
	"github.com/marcioapm/lux-gha/luxclient"
	"github.com/marcioapm/lux-gha/webhook"
)

var testIdentityKey = []byte("test-identity-key-0123456789abcdef")

const registryBlock = `"registry": {"roleArn": "arn:aws:iam::123456789012:role/lux-gha-registry", "region": "eu-west-1",
    "host": "123456789012.dkr.ecr.eu-west-1.amazonaws.com", "runnerListen": "127.0.0.1:0",
    "runnerEndpoint": "https://gha.example.com", "identityKeyFile": "/i"},
  "repositories"`

func newRegistryEnv(t *testing.T) *env {
	t.Helper()
	return newEnv(t, replace(`"repositories"`, registryBlock))
}

func TestRegistryRequiresIdentityKeyExactlyWhenConfigured(t *testing.T) {
	plain := newEnv(t)
	if _, err := New(Options{Config: plain.cfg, GitHub: plain.gh, Lux: plain.lux, Host: "h", IdentityKey: testIdentityKey}); err == nil {
		t.Fatal("identity key accepted without a registry")
	}
	reg := newRegistryEnv(t)
	if _, err := New(Options{Config: reg.cfg, GitHub: reg.gh, Lux: reg.lux, Host: "h"}); err == nil {
		t.Fatal("registry accepted without an identity key")
	}
}

// With a registry, the Run gets the identity token as a second file secret
// and the registry env; the token verifies for the Run's runner name and
// appears nowhere but in its secret. The idempotency key is unchanged.
func TestRegistryRunCarriesIdentitySecretAndEnv(t *testing.T) {
	e := newRegistryEnv(t)
	e.gh.queue(7, "queued", "self-hosted", "lux")
	if st := e.pass(); st.Error != "" || st.Provisioned != 1 {
		t.Fatalf("status %+v", st)
	}
	r := e.lux.all()[0]
	name := "lux-gha-t1-1001-7-0"
	if r.key != "api.github.com/t1/1001/7/0" {
		t.Fatalf("idempotency key %q", r.key)
	}
	wantEnv := map[string]string{
		"LUXGHA_JITCONFIG_FILE": "/run/lux-gha/jitconfig", "LUXGHA_IDENTITY_FILE": "/run/lux-gha/identity",
		"LUXGHA_RUNNER_ENDPOINT": "https://gha.example.com", "LUXGHA_REGISTRY_HOST": "123456789012.dkr.ecr.eu-west-1.amazonaws.com",
		"LUXGHA_REGISTRY_REGION": "eu-west-1",
	}
	if !maps.Equal(r.spec.Env, wantEnv) {
		t.Fatalf("env %v", r.spec.Env)
	}
	if len(r.spec.Secrets) != 2 || r.spec.Secrets[1].Name != "identity" || r.spec.Secrets[1].Path != "/run/lux-gha/identity" {
		t.Fatalf("secrets %+v", r.spec.Secrets)
	}
	token := r.spec.Secrets[1].Value
	if got, err := identity.Verify(testIdentityKey, token); err != nil || got != name {
		t.Fatalf("token verifies as %q, %v", got, err)
	}
	wire, err := json.Marshal(r.spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(wire), token) != 1 {
		t.Fatalf("token appears outside its secret: %s", wire)
	}
	var w struct {
		Secrets []struct{ Name, Value, As, Path string }
	}
	if err := json.Unmarshal(wire, &w); err != nil || w.Secrets[1].As != "file" || w.Secrets[1].Value != token {
		t.Fatalf("wire secrets %+v err=%v", w.Secrets, err)
	}

	// A replay of the same submission (a restarted bridge) sends the same
	// token, and the next pass creates nothing.
	if st := e.pass(); st.Provisioned != 0 || e.lux.createCount() != 1 {
		t.Fatalf("second pass: %+v creates=%d", st, e.lux.createCount())
	}
	again := newRegistryEnv(t)
	again.gh.queue(7, "queued", "self-hosted", "lux")
	again.pass()
	if again.lux.all()[0].spec.Secrets[1].Value != token {
		t.Fatal("token differs between submissions of the same runner")
	}
}

// The pass's own jobs listing is the attribution source: an in_progress job
// on one of this instance's runners is published for the runner listener,
// replaced when the runner's job changes and dropped when it completes. A
// signed in_progress delivery names it before the next pass does.
func TestPassesAndDeliveriesPublishRunnerAssignments(t *testing.T) {
	e := newRegistryEnv(t)
	e.gh.queue(7, "queued", "self-hosted", "lux")
	e.gh.queue(8, "queued", "self-hosted", "lux")
	e.pass()
	a := "lux-gha-t1-1001-7-0"
	if _, _, ok := e.rec.CandidateJob(a); ok {
		t.Fatal("assignment before pickup")
	}
	// The runner provisioned for job 7 takes job 8.
	e.pick(a, 8)
	e.pass()
	repo, job, ok := e.rec.CandidateJob(a)
	if !ok || job != 8 || repo != "acme/widgets" {
		t.Fatalf("assignment %s %d ok=%v", repo, job, ok)
	}
	if full, ok := e.rec.RepositoryName(1001); !ok || full != "acme/widgets" {
		t.Fatalf("repository name %q %v", full, ok)
	}
	e.gh.complete(8, "success")
	e.pass()
	if _, _, ok := e.rec.CandidateJob(a); ok {
		t.Fatal("assignment kept after the job completed")
	}

	b := "lux-gha-t1-1001-8-0"
	e.onJob(webhookEvent("in_progress", b, 9), webhookEvent("in_progress", "someone-else", 10))
	if repo, job, ok := e.rec.CandidateJob(b); !ok || job != 9 || repo != "acme/widgets" {
		t.Fatalf("delivery assignment %s %d ok=%v", repo, job, ok)
	}
	if _, _, ok := e.rec.CandidateJob("someone-else"); ok {
		t.Fatal("recorded a runner of another instance")
	}
}

func TestParseRunnerAcceptsOnlyThisInstancesCanonicalNames(t *testing.T) {
	e := newRegistryEnv(t)
	for name, want := range map[string]int64{
		"lux-gha-t1-1001-7-0":  1001,
		"lux-gha-t2-1001-7-0":  0,
		"lux-gha-t1-1001-07-0": 0,
		"lux-gha-t1-x-7-0":     0,
		"lux-gha-t1-1001-7":    0,
		"lux-gha-t1-0-7-0":     0,
	} {
		got, ok := e.rec.ParseRunner(name)
		if got != want || ok != (want != 0) {
			t.Errorf("%s: %d %v", name, got, ok)
		}
	}
}

func webhookEvent(action, runner string, jobID int64) webhook.WorkflowJobEvent {
	return webhook.WorkflowJobEvent{Action: action, InstallationID: 42, Repository: widgets, JobID: jobID, RunID: jobID * 10,
		Status: action, Labels: lux3, RunnerName: runner}
}

// onJob hands verified workflow_job events to the reconciler's webhook
// callback, as a signed delivery would.
func (e *env) onJob(events ...webhook.WorkflowJobEvent) {
	e.t.Helper()
	hook, err := e.rec.Webhook([]byte("s"))
	if err != nil {
		e.t.Fatal(err)
	}
	for _, ev := range events {
		if err := hook.OnWorkflowJob(context.Background(), ev); err != nil {
			e.t.Fatal(err)
		}
	}
}

// pick has runner take job jobID on the fake GitHub.
func (e *env) pick(runner string, jobID int64) {
	e.t.Helper()
	if err := e.gh.pick(runner, jobID); err != nil {
		e.t.Fatal(err)
	}
}

// A default-deny route's Runs can reach the runner endpoint: the bridge
// adds one host rule for runnerEndpoint's hostname, the tunnel in front of
// the runner listener; an unrestricted route needs none. An IP endpoint
// gets no rule.
func TestRegistryRunsMayReachTheRunnerEndpoint(t *testing.T) {
	e := newRegistryEnv(t)
	e.gh.queue(7, "queued", "self-hosted", "lux")
	e.gh.queue(8, "queued", "lux-big")
	e.pass()
	egress := map[string][]luxclient.EgressRule{}
	for _, r := range e.lux.all() {
		egress[r.run.Labels[LabelRoute]] = r.spec.Egress
	}
	want := []luxclient.EgressRule{{Host: "github.com"}, {Host: "*.actions.githubusercontent.com"}, {Host: "gha.example.com"}}
	if !slices.Equal(egress["default"], want) || !slices.Equal(egress["big"], []luxclient.EgressRule{{Host: "gha.example.com"}}) {
		t.Fatalf("egress %v", egress)
	}

	open := newEnv(t, replace(`"repositories"`, strings.Replace(registryBlock, "https://gha.example.com", "https://gha.example.com:8443", 1)),
		replace(`"egressHosts": ["github.com", "*.actions.githubusercontent.com"]`, `"unrestrictedEgress": true`))
	open.gh.queue(7, "queued", "self-hosted", "lux")
	open.gh.queue(8, "queued", "lux-big")
	open.pass()
	for _, r := range open.lux.all() {
		switch r.run.Labels[LabelRoute] {
		case "default":
			if len(r.spec.Egress) != 0 || !r.spec.UnrestrictedEgress {
				t.Fatalf("unrestricted route egress %v", r.spec.Egress)
			}
		case "big":
			if !slices.Equal(r.spec.Egress, []luxclient.EgressRule{{Host: "gha.example.com"}}) {
				t.Fatalf("endpoint with a port: egress %v", r.spec.Egress)
			}
		}
	}

	ip := newEnv(t, replace(`"repositories"`, strings.Replace(registryBlock, "https://gha.example.com", "https://192.0.2.10:8443", 1)))
	ip.gh.queue(8, "queued", "lux-big")
	ip.pass()
	if r := ip.lux.all(); len(r) != 1 || len(r[0].spec.Egress) != 0 {
		t.Fatalf("IP endpoint egress %+v", r)
	}
}

const cacheBlock = `"cache": {"listen": "127.0.0.1:0", "publicURL": "https://gha-cache.example.com",
    "s3": {"bucket": "cache-bucket", "region": "eu-west-1"}, "signingKeyFile": "/c", "repositoryIDs": [1001]},
  "repositories"`

// job7Wire is the request body for job 7 on the default route.
func job7Wire(t *testing.T, edits ...func(string) string) string {
	t.Helper()
	e := newEnv(t, edits...)
	e.gh.queue(7, "queued", "self-hosted", "lux")
	if st := e.pass(); st.Error != "" || st.Provisioned != 1 {
		t.Fatalf("status %+v", st)
	}
	wire, err := json.Marshal(e.lux.all()[0].spec)
	if err != nil {
		t.Fatal(err)
	}
	return string(wire)
}

// The cache is served by the bridge, not configured in the Run, so it adds
// no env, secret or egress rule: a cache block leaves the Run spec of either
// a plain or a registry configuration byte-identical, and with both the Run
// carries the registry's full set.
func TestCacheAndRegistryRunSpecs(t *testing.T) {
	if got := job7Wire(t); got != rawRouteWire {
		t.Fatalf("neither\n got: %s\nwant: %s", got, rawRouteWire)
	}
	if got := job7Wire(t, replace(`"repositories"`, cacheBlock)); got != rawRouteWire {
		t.Fatalf("cache only\n got: %s\nwant: %s", got, rawRouteWire)
	}
	registryOnly := job7Wire(t, replace(`"repositories"`, registryBlock))
	both := job7Wire(t, replace(`"repositories"`, registryBlock), replace(`"repositories"`, cacheBlock))
	if both != registryOnly {
		t.Fatalf("cache and registry\n got: %s\nwant: %s", both, registryOnly)
	}
	var w struct {
		Env     map[string]string
		Secrets []struct{ Name, Path string }
		Network struct{ Egress []luxclient.EgressRule }
	}
	if err := json.Unmarshal([]byte(both), &w); err != nil {
		t.Fatal(err)
	}
	wantEnv := map[string]string{
		"LUXGHA_JITCONFIG_FILE": "/run/lux-gha/jitconfig", "LUXGHA_IDENTITY_FILE": "/run/lux-gha/identity",
		"LUXGHA_RUNNER_ENDPOINT": "https://gha.example.com", "LUXGHA_REGISTRY_HOST": "123456789012.dkr.ecr.eu-west-1.amazonaws.com",
		"LUXGHA_REGISTRY_REGION": "eu-west-1",
	}
	wantEgress := []luxclient.EgressRule{{Host: "github.com"}, {Host: "*.actions.githubusercontent.com"}, {Host: "gha.example.com"}}
	if !maps.Equal(w.Env, wantEnv) || len(w.Secrets) != 2 || w.Secrets[0].Name != "jitconfig" || w.Secrets[1].Name != "identity" ||
		!slices.Equal(w.Network.Egress, wantEgress) {
		t.Fatalf("cache and registry spec %s", both)
	}
}
