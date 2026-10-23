package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"

	"github.com/marcioapm/lux-gha/githubapp"
	"github.com/marcioapm/lux-gha/internal/identity"
	"github.com/marcioapm/lux-gha/registry"
)

// fakeSTS answers AssumeRole with a fresh session per call, valid for the
// requested duration from the test clock, and records every request.
type fakeSTS struct {
	mu    sync.Mutex
	clock *fakeClock
	calls []*sts.AssumeRoleInput
	fail  bool
}

func (f *fakeSTS) AssumeRole(_ context.Context, in *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	if f.fail {
		return nil, errors.New("fake sts: service unavailable")
	}
	n := len(f.calls)
	exp := f.clock.Now().Add(time.Duration(aws.ToInt32(in.DurationSeconds)) * time.Second)
	return &sts.AssumeRoleOutput{Credentials: &ststypes.Credentials{
		AccessKeyId: aws.String(strings.Repeat("A", 15) + string(rune('0'+n))), SecretAccessKey: aws.String("SECRET-ACCESS-KEY-" + string(rune('0'+n))),
		SessionToken: aws.String("SESSION-TOKEN-" + string(rune('0'+n))), Expiration: &exp,
	}}, nil
}

func (f *fakeSTS) requests() []*sts.AssumeRoleInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

type listener struct {
	*env
	sts  *fakeSTS
	srv  *registry.Server
	logs *bytes.Buffer
}

func newListener(t *testing.T) *listener {
	t.Helper()
	e := newRegistryEnv(t)
	l := &listener{env: e, sts: &fakeSTS{clock: e.clock}, logs: &bytes.Buffer{}}
	srv, err := registry.New(registry.Options{Config: e.cfg.Registry, InstallationID: e.cfg.GitHub.InstallationID, IdentityKey: testIdentityKey,
		Runners: e.rec, GitHub: e.gh, STS: l.sts, Now: e.clock.Now, Log: slog.New(slog.NewTextHandler(l.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	l.srv = srv
	e.gh.branch(true, "trunk", "release/x", "feature", "releasex/1", "fix")
	return l
}

func (l *listener) post(path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	l.srv.Handler().ServeHTTP(rec, req)
	return rec
}

type registryAnswer struct {
	Registry, Read, Push, Trust, ExpiresAt string
}

func (l *listener) registry(runner string) (int, registryAnswer) {
	rec := l.post(registry.RegistryPath, identity.Token(testIdentityKey, runner))
	var a registryAnswer
	if rec.Code == http.StatusOK {
		dec := json.NewDecoder(rec.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			l.t.Fatalf("registry body: %v", err)
		}
	}
	return rec.Code, a
}

const regHost = "123456789012.dkr.ecr.eu-west-1.amazonaws.com"

// provisionAndRun queues job jobID in workflow run jobID*10 described by run,
// provisions it and has its own runner pick it up; it returns that runner.
func (l *listener) provisionAndRun(jobID int64, run githubapp.WorkflowRun) string {
	l.t.Helper()
	l.gh.queue(jobID, "queued", "self-hosted", "lux")
	run.ID = jobID * 10
	l.gh.describeRun(run)
	l.pass()
	name := "lux-gha-t1-1001-" + strconv.FormatInt(jobID, 10) + "-0"
	if err := l.gh.pick(name, jobID); err != nil {
		l.t.Fatal(err)
	}
	return name
}

func str(s string) *string { return &s }

func headRepo(id int64) *githubapp.HeadRepository { return &githubapp.HeadRepository{ID: id} }

// One case per row of the trust table, decided from the workflow run of the
// job the runner executes. The run's head commit is fakeSHA(70); acme/widgets
// has branches main (default), trunk, release/x, feature, releasex/1 and fix.
func TestRegistryTrustTable(t *testing.T) {
	read := regHost + "/acme/widgets"
	run := func(event, branch string, head *githubapp.HeadRepository) githubapp.WorkflowRun {
		return githubapp.WorkflowRun{Event: event, HeadBranch: str(branch), HeadRepository: head}
	}
	same, fork := headRepo(1001), headRepo(2002)
	tagAtHead := func(name string) func(*listener) { return func(l *listener) { l.gh.tag(name, fakeSHA(70)) } }
	for _, tc := range []struct {
		name  string
		run   githubapp.WorkflowRun
		setup func(*listener)
		trust string
		push  string
	}{
		// 1. read-only: no head repository, or another one, for every event.
		{"fork pull_request", run("pull_request", "main", fork), nil, "read-only", ""},
		{"fork pull_request_review", run("pull_request_review", "fix", fork), nil, "read-only", ""},
		{"fork pull_request_review_comment", run("pull_request_review_comment", "main", fork), nil, "read-only", ""},
		{"fork pull_request_target", run("pull_request_target", "main", fork), nil, "read-only", ""},
		{"fork workflow_run on main", run("workflow_run", "main", fork), nil, "read-only", ""},
		{"fork workflow_run on release/x", run("workflow_run", "release/x", fork), nil, "read-only", ""},
		{"pull_request, head repository gone", run("pull_request", "main", nil), nil, "read-only", ""},
		{"push, head repository gone", run("push", "main", nil), nil, "read-only", ""},
		// 3. trusted: trusted event on the default branch or a trustedRefs ref.
		{"push on main", run("push", "main", same), nil, "trusted", read},
		{"push on release/x", run("push", "release/x", same), nil, "trusted", read},
		{"push of tag v1", run("push", "v1", same), tagAtHead("v1"), "trusted", read},
		{"release v1", run("release", "v1", same), tagAtHead("v1"), "trusted", read},
		{"dispatch on main", run("workflow_dispatch", "main", same), nil, "trusted", read},
		{"dispatch on tag v1", run("workflow_dispatch", "v1", same), tagAtHead("v1"), "trusted", read},
		{"schedule", run("schedule", "main", same), nil, "trusted", read},
		{"same-repo workflow_run on main", run("workflow_run", "main", same), nil, "trusted", read},
		{"contract trustedRefs, push on main", run("push", "main", same), func(l *listener) {
			l.cfg.Registry.TrustedRefs = []string{"refs/heads/release/*", "refs/tags/v*"}
		}, "trusted", read},
		{"empty trustedRefs, push on main", run("push", "main", same), func(l *listener) { l.cfg.Registry.TrustedRefs = []string{} }, "trusted", read},
		// 2 and 4. untrusted.
		{"same-repo pull_request", run("pull_request", "feature", same), nil, "untrusted", read + "/pr"},
		{"same-repo pull_request from release/x", run("pull_request", "release/x", same), nil, "untrusted", read + "/pr"},
		{"same-repo pull_request_target", run("pull_request_target", "main", same), nil, "untrusted", read + "/pr"},
		{"merge_group", run("merge_group", "gh-readonly-queue/main/pr-1-abc", same), nil, "untrusted", read + "/pr"},
		{"issue_comment on main", run("issue_comment", "main", same), nil, "untrusted", read + "/pr"},
		{"push on feature branch", run("push", "feature", same), nil, "untrusted", read + "/pr"},
		{"push on releasex", run("push", "releasex/1", same), nil, "untrusted", read + "/pr"},
		{"dispatch on branch", run("workflow_dispatch", "feature", same), nil, "untrusted", read + "/pr"},
		{"branch named v1, no tag", run("push", "v1", same), func(l *listener) { l.gh.branch(true, "v1") }, "untrusted", read + "/pr"},
		{"branch v1, tag v1 at another commit", run("push", "v1", same), func(l *listener) {
			l.gh.branch(true, "v1")
			l.gh.tag("v1", fakeSHA(1))
		}, "untrusted", read + "/pr"},
		{"tag v1 at another commit, no branch", run("push", "v1", same), func(l *listener) { l.gh.tag("v1", fakeSHA(1)) }, "untrusted", read + "/pr"},
		{"tag named main", run("push", "main", same), tagAtHead("main"), "untrusted", read + "/pr"},
		{"dispatch on tag named main", run("workflow_dispatch", "main", same), tagAtHead("main"), "untrusted", read + "/pr"},
		{"tag named release/x, no such branch", run("push", "release/x", same), func(l *listener) {
			l.gh.branch(false, "release/x")
			l.gh.tag("release/x", fakeSHA(70))
		}, "untrusted", read + "/pr"},
		{"v1 both a tag and a branch", run("push", "v1", same), func(l *listener) {
			l.gh.branch(true, "v1")
			l.gh.tag("v1", fakeSHA(70))
		}, "untrusted", read + "/pr"},
		{"neither tag nor branch", run("push", "gone", same), nil, "untrusted", read + "/pr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newListener(t)
			if tc.setup != nil {
				tc.setup(l)
			}
			runner := l.provisionAndRun(7, tc.run)
			code, a := l.registry(runner)
			if code != http.StatusOK {
				t.Fatalf("status %d", code)
			}
			if a.Trust != tc.trust || a.Push != tc.push || a.Read != read || a.Registry != regHost {
				t.Fatalf("answer %+v", a)
			}
			if exp, err := time.Parse(time.RFC3339, a.ExpiresAt); err != nil || !exp.Equal(l.clock.Now().Add(registry.SessionDuration)) {
				t.Fatalf("expiresAt %q", a.ExpiresAt)
			}
			reqs := l.sts.requests()
			if len(reqs) != 1 {
				t.Fatalf("sts requests %d", len(reqs))
			}
			if got := tagValue(reqs[0], "lux-trust"); got != tc.trust {
				t.Fatalf("trust tag %q", got)
			}
			want, err := registry.SessionPolicy(l.cfg.Registry, "acme", "widgets", tc.trust)
			if err != nil || aws.ToString(reqs[0].Policy) != want {
				t.Fatalf("session policy %s, want %s (%v)", aws.ToString(reqs[0].Policy), want, err)
			}
		})
	}
}

// The ref of a run is resolved once per repository, name and commit for
// the repository TTL, and again after it; GitHub failing to answer is 503,
// never a guess.
func TestRegistryRefResolutionIsCachedAndFailsClosed(t *testing.T) {
	l := newListener(t)
	l.gh.tag("v1", fakeSHA(70))
	a := l.provisionAndRun(7, githubapp.WorkflowRun{ID: 70, Event: "push", HeadBranch: str("v1"), HeadSHA: fakeSHA(70), HeadRepository: headRepo(1001)})
	l.gh.takeReads()
	reads := func() (tags, branches int) {
		l.gh.mu.Lock()
		defer l.gh.mu.Unlock()
		return l.gh.opCount[opTags], l.gh.opCount[opBranch]
	}
	if _, got := l.registry(a); got.Trust != "trusted" {
		t.Fatalf("first: %+v", got)
	}
	// A second job of the same run, on the same tag and commit.
	l.gh.queueIn(widgets, 70, 8, "queued", "self-hosted", "lux")
	l.pass()
	b := "lux-gha-t1-1001-8-0"
	if err := l.gh.pick(b, 8); err != nil {
		t.Fatal(err)
	}
	l.pass()
	if _, got := l.registry(b); got.Trust != "trusted" {
		t.Fatalf("second: %+v", got)
	}
	if tags, branches := reads(); tags != 1 || branches != 1 {
		t.Fatalf("ref reads within the TTL: tags %d branches %d", tags, branches)
	}

	// After the TTL a third job reads again, and a GitHub failure is 503.
	l.clock.Add(10 * time.Minute)
	l.gh.queueIn(widgets, 70, 9, "queued", "self-hosted", "lux")
	l.pass()
	c := "lux-gha-t1-1001-9-0"
	if err := l.gh.pick(c, 9); err != nil {
		t.Fatal(err)
	}
	l.pass()
	for _, op := range []string{opTags, opBranch} {
		l.gh.fail = func(o, _ string, _ int) error {
			return map[bool]error{true: &githubapp.APIError{StatusCode: http.StatusBadGateway}}[o == op]
		}
		if code, _ := l.registry(c); code != http.StatusServiceUnavailable {
			t.Fatalf("%s down: %d", op, code)
		}
	}
	l.gh.fail = nil
	if code, got := l.registry(c); code != http.StatusOK || got.Trust != "trusted" {
		t.Fatalf("after recovery: %d %+v", code, got)
	}
}

// The path and the lux-owner and lux-repo tags are the repository's current
// GitHub names, lower-cased.
func TestRegistryNamesAreLowerCased(t *testing.T) {
	l := newListener(t)
	l.gh.repo(widgets).fullName = "ACME-Corp/My_Widgets.Go"
	runner := l.provisionAndRun(7, githubapp.WorkflowRun{Event: "push", HeadBranch: str("main"), HeadRepository: headRepo(1001)})
	code, a := l.registry(runner)
	want := regHost + "/acme-corp/my_widgets.go"
	if code != http.StatusOK || a.Read != want || a.Push != want || a.Trust != "trusted" {
		t.Fatalf("status %d answer %+v", code, a)
	}
	reqs := l.sts.requests()
	if len(reqs) != 1 || tagValue(reqs[0], "lux-owner") != "acme-corp" || tagValue(reqs[0], "lux-repo") != "my_widgets.go" {
		t.Fatalf("sts requests %+v", reqs)
	}
}

// A repository name that is not a registry path component after
// lower-casing gets no credentials on either route: 401, one log line, no
// STS call.
func TestRegistryRefusesNamesOutsideTheRegistryCharacterSet(t *testing.T) {
	for _, full := range []string{"acme/wídgets", "acme/wid gets", "ac+me/widgets", "acme/.github", "acme/a--b", "acme/widgets-", "acme"} {
		t.Run(full, func(t *testing.T) {
			l := newListener(t)
			l.gh.repo(widgets).fullName = full
			runner := l.provisionAndRun(7, githubapp.WorkflowRun{Event: "push", HeadBranch: str("main"), HeadRepository: headRepo(1001)})
			token := identity.Token(testIdentityKey, runner)
			for _, path := range []string{registry.RegistryPath, registry.CredentialsPath, registry.RegistryPath} {
				if rec := l.post(path, token); rec.Code != http.StatusUnauthorized {
					t.Fatalf("%s: %d", path, rec.Code)
				}
			}
			if n := len(l.sts.requests()); n != 0 {
				t.Fatalf("sts calls %d", n)
			}
			if got := strings.Count(l.logs.String(), "outside [a-z0-9._-]"); got != 1 {
				t.Fatalf("refusal logged %d times: %s", got, l.logs)
			}
		})
	}
}

// A non-default default branch is what the default-branch pattern means.
func TestRegistryDefaultBranchComesFromTheRepository(t *testing.T) {
	l := newListener(t)
	l.gh.repo(widgets).defaultBranch = "trunk"
	runner := l.provisionAndRun(7, githubapp.WorkflowRun{Event: "push", HeadBranch: str("main"), HeadRepository: headRepo(1001)})
	if _, a := l.registry(runner); a.Trust != "untrusted" {
		t.Fatalf("push on main with default trunk: %+v", a)
	}
	l.clock.Add(searchGap)
	other := l.provisionAndRun(8, githubapp.WorkflowRun{Event: "push", HeadBranch: str("trunk"), HeadRepository: headRepo(1001)})
	if _, a := l.registry(other); a.Trust != "trusted" {
		t.Fatalf("push on trunk: %+v", a)
	}
}

func tagValue(in *sts.AssumeRoleInput, key string) string {
	for _, t := range in.Tags {
		if aws.ToString(t.Key) == key {
			return aws.ToString(t.Value)
		}
	}
	return ""
}

// The Run was provisioned for job A (a push on main); its runner executes
// job B (a pull request from a branch). The trust is B's, both through a
// pass's listing and through a signed in_progress delivery, and the other
// way round.
func TestRegistryTrustIsTheExecutingJobsNotTheTriggeringJobs(t *testing.T) {
	for _, via := range []string{"pass", "delivery", "search"} {
		t.Run(via, func(t *testing.T) {
			l := newListener(t)
			l.gh.queue(7, "queued", "self-hosted", "lux")
			l.gh.queue(8, "queued", "self-hosted", "lux")
			l.gh.describeRun(githubapp.WorkflowRun{ID: 70, Event: "push", HeadBranch: str("main"), HeadRepository: headRepo(1001)})
			l.gh.describeRun(githubapp.WorkflowRun{ID: 80, Event: "pull_request", HeadBranch: str("feature"), HeadRepository: headRepo(1001)})
			l.pass()
			a, b := "lux-gha-t1-1001-7-0", "lux-gha-t1-1001-8-0"
			if err := l.gh.pick(a, 8); err != nil {
				t.Fatal(err)
			}
			if err := l.gh.pick(b, 7); err != nil {
				t.Fatal(err)
			}
			switch via {
			case "pass":
				l.pass()
			case "delivery":
				hook, err := l.rec.Webhook([]byte("s"))
				if err != nil {
					t.Fatal(err)
				}
				hook.OnWorkflowJob(context.Background(), webhookEvent("in_progress", a, 8))
				hook.OnWorkflowJob(context.Background(), webhookEvent("in_progress", b, 7))
			}
			if _, got := l.registry(a); got.Trust != "untrusted" || got.Push != regHost+"/acme/widgets/pr" {
				t.Fatalf("runner provisioned for the main push, running the PR: %+v", got)
			}
			if _, got := l.registry(b); got.Trust != "trusted" {
				t.Fatalf("runner provisioned for the PR, running the main push: %+v", got)
			}
			for _, req := range l.sts.requests() {
				if !strings.HasPrefix(aws.ToString(req.RoleSessionName), "lux-gha-1001-") {
					t.Fatalf("session name %q", aws.ToString(req.RoleSessionName))
				}
			}
			if s := aws.ToString(l.sts.requests()[0].RoleSessionName); s != "lux-gha-1001-8-1" {
				t.Fatalf("session for runner A names job %q, want job 8", s)
			}
		})
	}
}

func TestRegistryRefusesWhatItCannotAttribute(t *testing.T) {
	l := newListener(t)
	runner := l.provisionAndRun(7, githubapp.WorkflowRun{Event: "push", HeadBranch: str("main"), HeadRepository: headRepo(1001)})
	l.gh.queue(8, "queued", "self-hosted", "lux")
	l.pass()
	idle := "lux-gha-t1-1001-8-0" // registered with a live Run, no job yet

	for name, tc := range map[string]struct {
		token string
		want  int
	}{
		"no token":           {"", http.StatusUnauthorized},
		"bad hmac":           {identity.Token([]byte("some-other-key-some-other-key-xx"), runner), http.StatusUnauthorized},
		"truncated token":    {identity.Token(testIdentityKey, runner)[:40], http.StatusUnauthorized},
		"other instance":     {identity.Token(testIdentityKey, "lux-gha-t2-1001-7-0"), http.StatusUnauthorized},
		"not a runner name":  {identity.Token(testIdentityKey, "admin"), http.StatusUnauthorized},
		"no live run":        {identity.Token(testIdentityKey, "lux-gha-t1-1001-99-0"), http.StatusUnauthorized},
		"job not picked yet": {identity.Token(testIdentityKey, idle), http.StatusConflict},
	} {
		for _, path := range []string{registry.RegistryPath, registry.CredentialsPath} {
			if rec := l.post(path, tc.token); rec.Code != tc.want {
				t.Errorf("%s %s: %d, want %d", name, path, rec.Code, tc.want)
			}
		}
	}
	if len(l.sts.requests()) != 0 {
		t.Fatal("STS called for a refused request")
	}
	// The listener serves nothing else.
	for _, req := range []*http.Request{httptest.NewRequest(http.MethodGet, registry.RegistryPath, nil), httptest.NewRequest(http.MethodPost, "/status", nil)} {
		req.Header.Set("Authorization", "Bearer "+identity.Token(testIdentityKey, runner))
		rec := httptest.NewRecorder()
		l.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s: %d", req.Method, req.URL.Path, rec.Code)
		}
	}
	if code, _ := l.registry(runner); code != http.StatusOK {
		t.Fatalf("the executing runner: %d", code)
	}
}

// Once the job completes, or the Run ends, both routes refuse, cached
// credentials included. A signed completed delivery naming the runner
// refuses at once, inside the job re-read's TTL.
func TestRegistryRefusesAfterTheJobOrRunEnds(t *testing.T) {
	for _, end := range []string{"job completes", "run ends", "completed delivery"} {
		t.Run(end, func(t *testing.T) {
			l := newListener(t)
			runner := l.provisionAndRun(7, githubapp.WorkflowRun{Event: "push", HeadBranch: str("main"), HeadRepository: headRepo(1001)})
			token := identity.Token(testIdentityKey, runner)
			if rec := l.post(registry.CredentialsPath, token); rec.Code != http.StatusOK {
				t.Fatalf("before: %d", rec.Code)
			}
			switch end {
			case "job completes":
				l.gh.editJob(7, func(j *fakeJob) { j.status, j.conclusion = "completed", "success" })
			case "run ends":
				l.lux.end(l.runID("7"), "succeeded; resumePolicy never")
			case "completed delivery":
				hook, err := l.rec.Webhook([]byte("s"))
				if err != nil {
					t.Fatal(err)
				}
				// A delivery naming another instance's runner is ignored.
				for _, name := range []string{"lux-gha-t2-1001-7-0", runner} {
					if err := hook.OnWorkflowJob(context.Background(), webhookEvent("completed", name, 7)); err != nil {
						t.Fatal(err)
					}
				}
				if !l.rec.RunnerCompleted(runner) || l.rec.RunnerCompleted("lux-gha-t2-1001-7-0") {
					t.Fatal("completed delivery not recorded for this instance's runner only")
				}
			}
			if end != "completed delivery" {
				l.clock.Add(time.Minute)
			}
			for _, path := range []string{registry.CredentialsPath, registry.RegistryPath} {
				if rec := l.post(path, token); rec.Code != http.StatusUnauthorized {
					t.Fatalf("%s after the end: %d", path, rec.Code)
				}
			}
			if len(l.sts.requests()) != 1 {
				t.Fatalf("sts calls %d", len(l.sts.requests()))
			}
		})
	}
}

func TestRegistryUpstreamDownIs503(t *testing.T) {
	for _, down := range []string{"lux", "github job", "github run", "github repository", "sts"} {
		t.Run(down, func(t *testing.T) {
			l := newListener(t)
			runner := l.provisionAndRun(7, githubapp.WorkflowRun{Event: "push", HeadBranch: str("main"), HeadRepository: headRepo(1001)})
			l.pass()
			unavailable := &githubapp.APIError{StatusCode: http.StatusBadGateway}
			switch down {
			case "lux":
				l.lux.failRunnerList = 1
			case "github job":
				l.gh.fail = func(op, _ string, _ int) error { return map[bool]error{true: unavailable}[op == opGetJob] }
			case "github run":
				l.gh.fail = func(op, _ string, _ int) error { return map[bool]error{true: unavailable}[op == opGetRun] }
			case "github repository":
				l.gh.fail = func(op, _ string, _ int) error { return map[bool]error{true: unavailable}[op == "repo"] }
			case "sts":
				l.sts.fail = true
			}
			if code, _ := l.registry(runner); code != http.StatusServiceUnavailable {
				t.Fatalf("status %d", code)
			}
			// Recovered upstreams serve the same runner.
			l.lux.failRunnerList, l.gh.fail, l.sts.fail = 0, nil, false
			if code, _ := l.registry(runner); code != http.StatusOK {
				t.Fatalf("after recovery: %d", code)
			}
			if !strings.Contains(l.logs.String(), "registry:") || strings.Contains(l.logs.String(), "SECRET-ACCESS-KEY") {
				t.Fatalf("logs: %s", l.logs)
			}
		})
	}
}

const untrustedWidgetsPolicy = `{"Version":"2012-10-17","Statement":[` +
	`{"Effect":"Allow","Action":["ecr:GetAuthorizationToken"],"Resource":"*"},` +
	`{"Effect":"Allow","Action":["ecr:BatchCheckLayerAvailability","ecr:BatchGetImage","ecr:GetDownloadUrlForLayer"],"Resource":"arn:aws:ecr:eu-west-1:123456789012:repository/acme/widgets/*"},` +
	`{"Effect":"Allow","Action":["ecr:CompleteLayerUpload","ecr:InitiateLayerUpload","ecr:PutImage","ecr:UploadLayerPart"],"Resource":"arn:aws:ecr:eu-west-1:123456789012:repository/acme/widgets/pr/*"}]}`

// STS gets the role, a 900 s session, the session tags, the job's session
// policy and a session name; the credentials are cached per runner until five minutes before
// they expire, then minted again. Neither credentials nor tokens are logged.
func TestRegistryCredentialsAreTaggedCachedAndRefreshed(t *testing.T) {
	l := newListener(t)
	runner := l.provisionAndRun(7, githubapp.WorkflowRun{Event: "pull_request", HeadBranch: str("feature"), HeadRepository: headRepo(1001)})
	token := identity.Token(testIdentityKey, runner)
	creds := func() map[string]string {
		t.Helper()
		rec := l.post(registry.CredentialsPath, token)
		if rec.Code != http.StatusOK {
			t.Fatalf("credentials: %d", rec.Code)
		}
		var raw map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(slices.Sorted(maps.Keys(raw)), []string{"AccessKeyId", "Expiration", "SecretAccessKey", "SessionToken", "Version"}) || raw["Version"] != float64(1) {
			t.Fatalf("credential_process body %s", rec.Body)
		}
		out := map[string]string{}
		for k, v := range raw {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
		return out
	}
	first := creds()
	reqs := l.sts.requests()
	if len(reqs) != 1 {
		t.Fatalf("sts calls %d", len(reqs))
	}
	in := reqs[0]
	tags := map[string]string{}
	for _, tag := range in.Tags {
		tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	if aws.ToString(in.RoleArn) != "arn:aws:iam::123456789012:role/lux-gha-registry" || aws.ToInt32(in.DurationSeconds) != 900 ||
		!maps.Equal(tags, map[string]string{"lux-owner": "acme", "lux-repo": "widgets", "lux-trust": "untrusted"}) ||
		aws.ToString(in.RoleSessionName) != "lux-gha-1001-7-1" || in.TransitiveTagKeys != nil || aws.ToString(in.Policy) != untrustedWidgetsPolicy {
		t.Fatalf("AssumeRole input %+v tags %v", in, tags)
	}
	if first["Expiration"] != l.clock.Now().Add(15*time.Minute).UTC().Format(time.RFC3339) {
		t.Fatalf("expiration %q", first["Expiration"])
	}

	// Cached across both routes until 5 minutes before expiry.
	l.clock.Add(9 * time.Minute)
	if again := creds(); again["AccessKeyId"] != first["AccessKeyId"] || len(l.sts.requests()) != 1 {
		t.Fatalf("not cached: %v, %d calls", again["AccessKeyId"], len(l.sts.requests()))
	}
	if code, a := l.registry(runner); code != http.StatusOK || a.ExpiresAt != first["Expiration"] {
		t.Fatalf("registry route %d %+v", code, a)
	}
	l.clock.Add(time.Minute + time.Second)
	refreshed := creds()
	if refreshed["AccessKeyId"] == first["AccessKeyId"] || len(l.sts.requests()) != 2 ||
		refreshed["Expiration"] != l.clock.Now().Add(15*time.Minute).UTC().Format(time.RFC3339) {
		t.Fatalf("not refreshed: %v, %d calls", refreshed, len(l.sts.requests()))
	}
	for _, secret := range []string{first["SecretAccessKey"], first["SessionToken"], refreshed["SecretAccessKey"], token} {
		if strings.Contains(l.logs.String(), secret) {
			t.Fatalf("log holds a secret: %s", l.logs)
		}
	}
}

// A hook retrying through GitHub's attribution lag sees 409 until the job
// shows its runner, then 200; the repository's fallback search runs at most
// once per searchGap.
func TestRegistryResolvesAfterAttributionLag(t *testing.T) {
	l := newListener(t)
	l.gh.queue(7, "queued", "self-hosted", "lux")
	l.gh.describeRun(githubapp.WorkflowRun{ID: 70, Event: "push", HeadBranch: str("main"), HeadRepository: headRepo(1001)})
	l.pass()
	a := "lux-gha-t1-1001-7-0"
	if code, _ := l.registry(a); code != http.StatusConflict {
		t.Fatalf("before pickup: %d", code)
	}
	if err := l.gh.pick(a, 7); err != nil {
		t.Fatal(err)
	}
	if code, _ := l.registry(a); code != http.StatusConflict {
		t.Fatalf("within the search gap: %d", code)
	}
	l.clock.Add(searchGap)
	if code, got := l.registry(a); code != http.StatusOK || got.Trust != "trusted" {
		t.Fatalf("after pickup: %d %+v", code, got)
	}
}

// The per-runner bucket (30, refilled at 0.5/s) is charged only by a
// request that calls an upstream: a runner whose candidate GitHub keeps
// contradicting is refused with 503 after exactly 30 requests, while a
// resolved runner answered from its cache is never limited.
func TestRegistryRateLimitsUpstreamRequestsPerToken(t *testing.T) {
	l := newListener(t)
	l.gh.queue(7, "queued", "self-hosted", "lux")
	l.gh.queue(8, "queued", "self-hosted", "lux")
	l.gh.describeRun(githubapp.WorkflowRun{ID: 70, Event: "push", HeadBranch: str("main"), HeadRepository: headRepo(1001)})
	l.pass()
	a, b := "lux-gha-t1-1001-7-0", "lux-gha-t1-1001-8-0"
	if err := l.gh.pick(a, 7); err != nil {
		t.Fatal(err)
	}
	l.pass()
	if code, _ := l.registry(a); code != http.StatusOK {
		t.Fatalf("a: %d", code)
	}
	// A delivery names b on job 8, which GitHub still shows queued: every
	// request for b reads the job.
	hook, err := l.rec.Webhook([]byte("s"))
	if err != nil {
		t.Fatal(err)
	}
	if err := hook.OnWorkflowJob(context.Background(), webhookEvent("in_progress", b, 8)); err != nil {
		t.Fatal(err)
	}
	codes := map[int]int{}
	for range 40 {
		code, _ := l.registry(b)
		codes[code]++
	}
	if codes[http.StatusConflict] != 30 || codes[http.StatusServiceUnavailable] != 10 {
		t.Fatalf("b: %v", codes)
	}
	for range 40 {
		if code, _ := l.registry(a); code != http.StatusOK {
			t.Fatalf("cached runner limited: %d", code)
		}
	}
	l.clock.Add(2 * time.Second) // one token
	if code, _ := l.registry(b); code != http.StatusConflict {
		t.Fatalf("after one token: %d", code)
	}
	if code, _ := l.registry(b); code != http.StatusServiceUnavailable {
		t.Fatalf("token spent: %d", code)
	}
}

// ghCalls counts the fake GitHub's calls per operation since the last reset.
func (l *listener) ghCalls() map[string]int {
	l.gh.mu.Lock()
	defer l.gh.mu.Unlock()
	out := maps.Clone(l.gh.opCount)
	clear(l.gh.opCount)
	return out
}

// One search per repository serves every runner it sees: a second runner
// whose job no pass or delivery named resolves from the first runner's
// search with one job read, inside the search gap.
func TestRegistrySearchIsPerRepositoryAndPublishesEveryRunner(t *testing.T) {
	l := newListener(t)
	for _, id := range []int64{7, 8} {
		l.gh.queue(id, "queued", "self-hosted", "lux")
		l.gh.describeRun(githubapp.WorkflowRun{ID: id * 10, Event: "push", HeadBranch: str("main"), HeadRepository: headRepo(1001)})
	}
	l.pass()
	a, b := "lux-gha-t1-1001-7-0", "lux-gha-t1-1001-8-0"
	for _, p := range [][2]any{{a, int64(7)}, {b, int64(8)}} {
		if err := l.gh.pick(p[0].(string), p[1].(int64)); err != nil {
			t.Fatal(err)
		}
	}
	l.ghCalls()
	if code, _ := l.registry(a); code != http.StatusOK {
		t.Fatalf("a: %d", code)
	}
	if calls := l.ghCalls(); calls[opRuns] != 1 || calls[opJobs] != 2 {
		t.Fatalf("search for a: %v", calls)
	}
	if code, _ := l.registry(b); code != http.StatusOK {
		t.Fatalf("b: %d", code)
	}
	if calls := l.ghCalls(); calls[opRuns] != 0 || calls[opJobs] != 0 || calls[opGetJob] != 1 {
		t.Fatalf("b resolved with %v, want one job read", calls)
	}
}

// A search reads at most searchPageBudget pages of runs and jobs together.
func TestRegistrySearchIsBounded(t *testing.T) {
	l := newListener(t)
	l.gh.queue(7, "queued", "self-hosted", "lux")
	l.pass()
	// 40 other in_progress runs listed before run 70, each with an
	// in_progress job on another tool's runner, at 2 per page.
	for i := int64(0); i < 40; i++ {
		l.gh.queueIn(widgets, 1+i, 100+i, "queued", "other")
		l.gh.pickElsewhere(widgets, 100+i, 9000+i, "elsewhere-"+strconv.FormatInt(i, 10))
	}
	a := "lux-gha-t1-1001-7-0"
	if err := l.gh.pick(a, 7); err != nil {
		t.Fatal(err)
	}
	l.ghCalls()
	if code, _ := l.registry(a); code != http.StatusConflict {
		t.Fatalf("status %d", code)
	}
	if calls := l.ghCalls(); calls[opRuns]+calls[opJobs] != searchPageBudget {
		t.Fatalf("search pages %v, want %d", calls, searchPageBudget)
	}
}

// While GitHub rate-limits the installation the listener answers 503 and
// calls nothing; a rate limit the listener meets pauses the reconciler's
// passes too.
func TestRegistryHonoursTheSharedGitHubRateLimit(t *testing.T) {
	l := newListener(t)
	l.gh.queue(7, "queued", "self-hosted", "lux")
	l.gh.describeRun(githubapp.WorkflowRun{ID: 70, Event: "push", HeadBranch: str("main"), HeadRepository: headRepo(1001)})
	l.pass()
	a := "lux-gha-t1-1001-7-0"
	if err := l.gh.pick(a, 7); err != nil {
		t.Fatal(err)
	}

	// The listener meets the limit: 503, and the next pass does not run.
	l.gh.rateLimitUntil = l.clock.Now().Add(time.Minute)
	if code, _ := l.registry(a); code != http.StatusServiceUnavailable {
		t.Fatalf("rate-limited search: %d", code)
	}
	if until := l.rec.RateLimitedUntil(); !until.Equal(l.gh.rateLimitUntil) {
		t.Fatalf("limit not fed back: %v", until)
	}
	l.ghCalls()
	if st := l.pass(); st.Error != ErrRateLimited {
		t.Fatalf("pass during the listener's limit: %+v", st)
	}
	l.clock.Add(searchGap)
	if code, _ := l.registry(a); code != http.StatusServiceUnavailable {
		t.Fatalf("during the limit: %d", code)
	}
	if calls := l.ghCalls(); len(calls) != 0 {
		t.Fatalf("GitHub called during the limit: %v", calls)
	}

	l.clock.Add(time.Minute)
	if code, got := l.registry(a); code != http.StatusOK || got.Trust != "trusted" {
		t.Fatalf("after the limit: %d %+v", code, got)
	}
}

// A delivery's candidate survives a pass that still lists its job queued
// (GitHub's lag) and is dropped when a later pass lists the job completed,
// when the runner's Run ends, or after assignmentTTL.
func TestDeliveryCandidatesOutliveLaggingPasses(t *testing.T) {
	for _, end := range []string{"job completed", "run ended", "ttl"} {
		t.Run(end, func(t *testing.T) {
			e := newRegistryEnv(t)
			e.gh.queue(7, "queued", "self-hosted", "lux")
			e.pass()
			a := "lux-gha-t1-1001-7-0"
			hook, err := e.rec.Webhook([]byte("s"))
			if err != nil {
				t.Fatal(err)
			}
			e.clock.Add(time.Second)
			if err := hook.OnWorkflowJob(context.Background(), webhookEvent("in_progress", a, 7)); err != nil {
				t.Fatal(err)
			}
			e.clock.Add(time.Second)
			e.pass() // job 7 still listed queued
			if as, ok := e.rec.RunnerAssignment(a); !ok || as.JobID != 7 {
				t.Fatalf("delivery candidate dropped by a lagging pass: %+v %v", as, ok)
			}
			switch end {
			case "job completed":
				// A second job keeps run 70 in_progress, so the pass lists it.
				e.gh.queueIn(widgets, 70, 71, "queued", "other")
				e.gh.editJob(7, func(j *fakeJob) { j.status, j.conclusion, j.runner = "completed", "success", a })
				e.clock.Add(time.Second)
				e.pass()
			case "run ended":
				e.lux.end(e.runID("7"), "succeeded; resumePolicy never")
				e.clock.Add(time.Second)
				e.pass()
			case "ttl":
				e.clock.Add(assignmentTTL + time.Second)
			}
			if as, ok := e.rec.RunnerAssignment(a); ok {
				t.Fatalf("candidate kept after %s: %+v", end, as)
			}
		})
	}
}
