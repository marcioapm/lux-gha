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
// job the runner executes.
func TestRegistryTrustTable(t *testing.T) {
	read := regHost + "/acme/widgets"
	for _, tc := range []struct {
		name  string
		run   githubapp.WorkflowRun
		tag   bool
		trust string
		push  string
	}{
		{"fork pull_request", githubapp.WorkflowRun{Event: "pull_request", HeadBranch: str("main"), HeadRepository: headRepo(2002)}, false, "read-only", ""},
		{"fork pull_request_review", githubapp.WorkflowRun{Event: "pull_request_review", HeadBranch: str("fix"), HeadRepository: headRepo(2002)}, false, "read-only", ""},
		{"same-repo pull_request", githubapp.WorkflowRun{Event: "pull_request", HeadBranch: str("feature"), HeadRepository: headRepo(1001)}, false, "untrusted", read + "/pr"},
		{"pull_request_target", githubapp.WorkflowRun{Event: "pull_request_target", HeadBranch: str("main"), HeadRepository: headRepo(2002)}, false, "untrusted", read + "/pr"},
		{"merge_group", githubapp.WorkflowRun{Event: "merge_group", HeadBranch: str("gh-readonly-queue/main/pr-1-abc"), HeadRepository: headRepo(1001)}, false, "untrusted", read + "/pr"},
		{"push on main", githubapp.WorkflowRun{Event: "push", HeadBranch: str("main"), HeadRepository: headRepo(1001)}, false, "trusted", read},
		{"push on release/x", githubapp.WorkflowRun{Event: "push", HeadBranch: str("release/x"), HeadRepository: headRepo(1001)}, false, "trusted", read},
		{"tag v1", githubapp.WorkflowRun{Event: "push", HeadBranch: str("v1"), HeadRepository: headRepo(1001)}, true, "trusted", read},
		{"branch named v1, no tag", githubapp.WorkflowRun{Event: "push", HeadBranch: str("v1"), HeadRepository: headRepo(1001)}, false, "untrusted", read + "/pr"},
		{"push on feature branch", githubapp.WorkflowRun{Event: "push", HeadBranch: str("feature"), HeadRepository: headRepo(1001)}, false, "untrusted", read + "/pr"},
		{"push on releasex", githubapp.WorkflowRun{Event: "push", HeadBranch: str("releasex/1"), HeadRepository: headRepo(1001)}, false, "untrusted", read + "/pr"},
		{"dispatch on main", githubapp.WorkflowRun{Event: "workflow_dispatch", HeadBranch: str("main"), HeadRepository: headRepo(1001)}, false, "trusted", read},
		{"dispatch on branch", githubapp.WorkflowRun{Event: "workflow_dispatch", HeadBranch: str("feature"), HeadRepository: headRepo(1001)}, false, "untrusted", read + "/pr"},
		{"schedule", githubapp.WorkflowRun{Event: "schedule", HeadBranch: str("main"), HeadRepository: headRepo(1001)}, false, "trusted", read},
		{"issue_comment on main", githubapp.WorkflowRun{Event: "issue_comment", HeadBranch: str("main"), HeadRepository: headRepo(1001)}, false, "untrusted", read + "/pr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newListener(t)
			if tc.tag {
				l.gh.tag("v1", fakeSHA(70))
			}
			// A tag at another commit does not make the run a tag run.
			l.gh.tag("v1-old", fakeSHA(1))
			runner := l.provisionAndRun(7, tc.run)
			code, a := l.registry(runner)
			if code != http.StatusOK {
				t.Fatalf("status %d", code)
			}
			if a.Trust != tc.trust || a.Push != tc.push || a.Read != read || a.Registry != regHost {
				t.Fatalf("answer %+v", a)
			}
			if exp, err := time.Parse(time.RFC3339, a.ExpiresAt); err != nil || !exp.Equal(l.clock.Now().Add(time.Hour)) {
				t.Fatalf("expiresAt %q", a.ExpiresAt)
			}
			reqs := l.sts.requests()
			if len(reqs) != 1 || tagValue(reqs[0], "lux-trust") != tc.trust {
				t.Fatalf("sts requests %d, trust tag %q", len(reqs), tagValue(reqs[0], "lux-trust"))
			}
		})
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
// credentials included.
func TestRegistryRefusesAfterTheJobOrRunEnds(t *testing.T) {
	for _, end := range []string{"job completes", "run ends"} {
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
			}
			l.clock.Add(time.Minute)
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

// STS gets the role, a 3600 s session, the session tags and a session
// name; the credentials are cached per runner until five minutes before
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
	if aws.ToString(in.RoleArn) != "arn:aws:iam::123456789012:role/lux-gha-registry" || aws.ToInt32(in.DurationSeconds) != 3600 ||
		!maps.Equal(tags, map[string]string{"lux-owner": "acme", "lux-repo": "widgets", "lux-trust": "untrusted"}) ||
		aws.ToString(in.RoleSessionName) != "lux-gha-1001-7-1" || in.TransitiveTagKeys != nil || in.Policy != nil {
		t.Fatalf("AssumeRole input %+v tags %v", in, tags)
	}
	if first["Expiration"] != l.clock.Now().Add(time.Hour).UTC().Format(time.RFC3339) {
		t.Fatalf("expiration %q", first["Expiration"])
	}

	// Cached across both routes until 5 minutes before expiry.
	l.clock.Add(54 * time.Minute)
	if again := creds(); again["AccessKeyId"] != first["AccessKeyId"] || len(l.sts.requests()) != 1 {
		t.Fatalf("not cached: %v, %d calls", again["AccessKeyId"], len(l.sts.requests()))
	}
	if code, a := l.registry(runner); code != http.StatusOK || a.ExpiresAt != first["Expiration"] {
		t.Fatalf("registry route %d %+v", code, a)
	}
	l.clock.Add(time.Minute + time.Second)
	refreshed := creds()
	if refreshed["AccessKeyId"] == first["AccessKeyId"] || len(l.sts.requests()) != 2 ||
		refreshed["Expiration"] != l.clock.Now().Add(time.Hour).UTC().Format(time.RFC3339) {
		t.Fatalf("not refreshed: %v, %d calls", refreshed, len(l.sts.requests()))
	}
	for _, secret := range []string{first["SecretAccessKey"], first["SessionToken"], refreshed["SecretAccessKey"], token} {
		if strings.Contains(l.logs.String(), secret) {
			t.Fatalf("log holds a secret: %s", l.logs)
		}
	}
}

// A hook retrying through GitHub's attribution lag sees 409 until the job
// shows its runner, then 200; a token beyond its bucket is refused with 503
// while another runner is served.
func TestRegistryResolvesAfterAttributionLagAndRateLimitsPerToken(t *testing.T) {
	l := newListener(t)
	l.gh.queue(7, "queued", "self-hosted", "lux")
	l.gh.describeRun(githubapp.WorkflowRun{ID: 70, Event: "push", HeadBranch: str("main"), HeadRepository: headRepo(1001)})
	l.gh.queue(8, "queued", "self-hosted", "lux")
	l.pass()
	a, b := "lux-gha-t1-1001-7-0", "lux-gha-t1-1001-8-0"
	if code, _ := l.registry(a); code != http.StatusConflict {
		t.Fatalf("before pickup: %d", code)
	}
	if err := l.gh.pick(a, 7); err != nil {
		t.Fatal(err)
	}
	// The fallback search runs at most every 5 s per runner.
	if code, _ := l.registry(a); code != http.StatusConflict {
		t.Fatalf("within the search gap: %d", code)
	}
	l.clock.Add(5 * time.Second)
	if code, got := l.registry(a); code != http.StatusOK || got.Trust != "trusted" {
		t.Fatalf("after pickup: %d %+v", code, got)
	}
	limited := 0
	for range 40 {
		if code, _ := l.registry(a); code == http.StatusServiceUnavailable {
			limited++
		}
	}
	if limited == 0 || limited > 40-27 {
		t.Fatalf("limited %d of 40", limited)
	}
	if code, _ := l.registry(b); code != http.StatusConflict {
		t.Fatalf("another runner during the first's limit: %d", code)
	}
	l.clock.Add(10 * time.Second)
	if code, _ := l.registry(a); code != http.StatusOK {
		t.Fatalf("after refill: %d", code)
	}
}
