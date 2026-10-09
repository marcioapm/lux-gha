package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testKey = func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}()

func pkcs1PEM(k *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}
func pkcs8PEM(t *testing.T, k *rsa.PrivateKey) []byte {
	b, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b})
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// fakeGitHub verifies app JWTs against the public key and issues sequential installation tokens.
type fakeGitHub struct {
	t          *testing.T
	clock      *clock
	tokenCalls atomic.Int32
	tokenDelay chan struct{}
	mu         sync.Mutex
	api        func(w http.ResponseWriter, r *http.Request)
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != APIVersion || r.Header.Get("User-Agent") == "" {
		http.Error(w, "bad headers", 400)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens" {
		if err := f.verifyJWT(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); err != nil {
			http.Error(w, err.Error(), 401)
			return
		}
		if f.tokenDelay != nil {
			<-f.tokenDelay
		}
		n := f.tokenCalls.Add(1)
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"token":"ghs_token%d","expires_at":%q}`, n, f.clock.now().Add(time.Hour).Format(time.RFC3339))
		return
	}
	f.mu.Lock()
	api := f.api
	f.mu.Unlock()
	api(w, r)
}

func (f *fakeGitHub) verifyJWT(jwt string) error {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return errors.New("malformed")
	}
	var header struct {
		Alg string `json:"alg"`
	}
	var claims struct {
		Iss      string `json:"iss"`
		Iat, Exp int64
	}
	for i, dst := range []any{&header, &claims} {
		b, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil || json.Unmarshal(b, dst) != nil {
			return errors.New("bad segment")
		}
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testKey.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		return err
	}
	now := f.clock.now().Unix()
	if header.Alg != "RS256" || claims.Iss != "Iv1.client" || claims.Iat > now || claims.Exp <= now || claims.Exp-claims.Iat > 600 {
		return fmt.Errorf("bad claims %+v %+v", header, claims)
	}
	return nil
}

func newTestClient(t *testing.T, key []byte, api http.HandlerFunc) (*Client, *fakeGitHub, *clock) {
	t.Helper()
	clk := &clock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	fake := &fakeGitHub{t: t, clock: clk, api: api}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := New(Config{AppID: "Iv1.client", PrivateKey: key, BaseURL: srv.URL, RequestTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c.now = clk.now
	return c, fake, clk
}

var repo = Repository{InstallationID: 42, Owner: "octo-org", Name: "app.repo"}

func requireInstallationToken(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Header.Get("Authorization") != "Bearer "+want {
		http.Error(w, "wrong token", 401)
		return false
	}
	return true
}

func TestInstallationTokenUsesVerifiableJWTForPKCS1AndPKCS8Keys(t *testing.T) {
	for name, key := range map[string][]byte{"pkcs1": pkcs1PEM(testKey), "pkcs8": pkcs8PEM(t, testKey)} {
		t.Run(name, func(t *testing.T) {
			c, _, _ := newTestClient(t, key, nil)
			token, err := c.InstallationToken(context.Background(), 42)
			if err != nil || token != "ghs_token1" {
				t.Fatalf("token=%q err=%v", token, err)
			}
		})
	}
}

func TestJWTRejectedWhenClockIsAheadOfIssuedAtWindow(t *testing.T) {
	c, _, clk := newTestClient(t, pkcs1PEM(testKey), nil)
	serverNow := clk.now()
	c.now = func() time.Time { return serverNow.Add(2 * time.Minute) }
	var apiErr *APIError
	if _, err := c.InstallationToken(context.Background(), 42); !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("err=%v", err)
	}
}

func TestParsePrivateKeyRejectsNonRSAAndShortKeys(t *testing.T) {
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	for name, data := range map[string][]byte{"garbage": []byte("nope"), "short": pkcs1PEM(small), "trailing": append(pkcs1PEM(testKey), []byte("junk")...)} {
		if _, err := ParsePrivateKey(data); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestConcurrentCallersShareOneTokenAndRefreshNearExpiry(t *testing.T) {
	c, fake, clk := newTestClient(t, pkcs1PEM(testKey), nil)
	fake.tokenDelay = make(chan struct{})
	var wg sync.WaitGroup
	tokens := make([]string, 20)
	errs := make([]error, 20)
	for i := range tokens {
		wg.Add(1)
		go func(i int) { defer wg.Done(); tokens[i], errs[i] = c.InstallationToken(context.Background(), 42) }(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(fake.tokenDelay)
	wg.Wait()
	for i := range tokens {
		if errs[i] != nil || tokens[i] != "ghs_token1" {
			t.Fatalf("caller %d: %q %v", i, tokens[i], errs[i])
		}
	}
	if got := fake.tokenCalls.Load(); got != 1 {
		t.Fatalf("token calls=%d", got)
	}

	clk.add(58 * time.Minute)
	if tok, _ := c.InstallationToken(context.Background(), 42); tok != "ghs_token1" {
		t.Fatalf("refreshed too early: %s", tok)
	}
	clk.add(90 * time.Second)
	if tok, _ := c.InstallationToken(context.Background(), 42); tok != "ghs_token2" {
		t.Fatalf("not refreshed inside expiry margin: %s", tok)
	}
}

func TestCancelledWaiterDoesNotFailSharedTokenFetch(t *testing.T) {
	c, fake, _ := newTestClient(t, pkcs1PEM(testKey), nil)
	fake.tokenDelay = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := c.InstallationToken(ctx, 42); first <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first err=%v", err)
	}
	close(fake.tokenDelay)
	if tok, err := c.InstallationToken(context.Background(), 42); err != nil || tok != "ghs_token1" {
		t.Fatalf("tok=%q err=%v", tok, err)
	}
	if got := fake.tokenCalls.Load(); got != 1 {
		t.Fatalf("token calls=%d", got)
	}
}

func TestUnauthorizedResponseEvictsCachedToken(t *testing.T) {
	var calls atomic.Int32
	c, fake, _ := newTestClient(t, pkcs1PEM(testKey), func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "revoked", 401)
			return
		}
		if requireInstallationToken(w, r, "ghs_token2") {
			w.Write([]byte(`{"id":7}`))
		}
	})
	if _, err := c.GetWorkflowJob(context.Background(), repo, 7); err == nil {
		t.Fatal("expected 401")
	}
	job, err := c.GetWorkflowJob(context.Background(), repo, 7)
	if err != nil || job.ID != 7 || fake.tokenCalls.Load() != 2 {
		t.Fatalf("job=%+v err=%v calls=%d", job, err, fake.tokenCalls.Load())
	}
}

func TestGenerateRepoJITConfig(t *testing.T) {
	c, _, _ := newTestClient(t, pkcs1PEM(testKey), func(w http.ResponseWriter, r *http.Request) {
		if !requireInstallationToken(w, r, "ghs_token1") {
			return
		}
		if r.Method != "POST" || r.URL.EscapedPath() != "/repos/octo-org/app.repo/actions/runners/generate-jitconfig" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "route", 404)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "lux-1" || body["runner_group_id"] != float64(1) || fmt.Sprint(body["labels"]) != "[self-hosted lux]" {
			http.Error(w, "body", 422)
			return
		}
		w.WriteHeader(201)
		w.Write([]byte(`{"runner":{"id":23,"name":"lux-1","os":"linux","status":"offline","busy":false,"labels":[{"name":"lux"}]},"encoded_jit_config":"SECRET-JIT"}`))
	})
	cfg, err := c.GenerateRepoJITConfig(context.Background(), repo, JITConfigRequest{Name: "lux-1", RunnerGroupID: 1, Labels: []string{"self-hosted", "lux"}})
	if err != nil || cfg.Runner.ID != 23 || cfg.EncodedJITConfig != "SECRET-JIT" {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
	if s := fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg); strings.Contains(s, "SECRET-JIT") {
		t.Fatalf("formatted config leaks secret: %s", s)
	}
	if _, err := c.GenerateRepoJITConfig(context.Background(), repo, JITConfigRequest{Name: "x", RunnerGroupID: 1}); err == nil {
		t.Fatal("accepted empty labels")
	}
}

func TestGetWorkflowJobDecodesNullableRunnerAttribution(t *testing.T) {
	c, _, _ := newTestClient(t, pkcs1PEM(testKey), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octo-org/app.repo/actions/jobs/99" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"id":99,"run_id":5,"run_attempt":2,"status":"completed","conclusion":"success","labels":["lux"],"runner_id":23,"runner_name":"lux-1","runner_group_id":null,"started_at":"2026-10-09T12:00:00Z","completed_at":null}`))
	})
	job, err := c.GetWorkflowJob(context.Background(), repo, 99)
	if err != nil || job.RunAttempt != 2 || *job.RunnerID != 23 || *job.RunnerName != "lux-1" || job.RunnerGroupID != nil || job.CompletedAt != nil || *job.Conclusion != "success" {
		t.Fatalf("job=%+v err=%v", job, err)
	}
}

func TestListAndDeleteRunners(t *testing.T) {
	c, _, _ := newTestClient(t, pkcs1PEM(testKey), func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/octo-org/app.repo/actions/runners":
			if r.URL.Query().Get("page") != "2" || r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("name") != "lux-1" {
				http.Error(w, "query", 400)
				return
			}
			w.Header().Set("Link", `<https://api.github.com/x?page=3>; rel="next", <https://api.github.com/x?page=9>; rel="last"`)
			w.Write([]byte(`{"total_count":1,"runners":[{"id":23,"name":"lux-1","status":"online","busy":true,"ephemeral":true,"labels":[{"name":"lux","type":"custom"}]}]}`))
		case r.Method == "DELETE" && r.URL.Path == "/repos/octo-org/app.repo/actions/runners/23":
			w.WriteHeader(204)
		case r.Method == "DELETE":
			http.Error(w, `{"message":"busy"}`, 422)
		default:
			http.NotFound(w, r)
		}
	})
	page, err := c.ListRepoRunners(context.Background(), repo, RunnerListOptions{PageOptions{Page: 2, PerPage: 100}, "lux-1"})
	if err != nil || !page.HasNext || len(page.Runners) != 1 || !page.Runners[0].Busy || !page.Runners[0].Ephemeral {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if err := c.DeleteRepoRunner(context.Background(), repo, 23); err != nil {
		t.Fatal(err)
	}
	var apiErr *APIError
	if err := c.DeleteRepoRunner(context.Background(), repo, 24); !errors.As(err, &apiErr) || apiErr.StatusCode != 422 {
		t.Fatalf("err=%v", err)
	}
	if _, err := c.ListRepoRunners(context.Background(), repo, RunnerListOptions{PageOptions: PageOptions{PerPage: 101}}); err == nil {
		t.Fatal("accepted per_page 101")
	}
}

func TestListWorkflowRunsAndJobs(t *testing.T) {
	c, _, _ := newTestClient(t, pkcs1PEM(testKey), func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/repos/octo-org/app.repo/actions/runs":
			if q.Get("status") != "queued" || q.Get("exclude_pull_requests") != "true" {
				http.Error(w, "query", 400)
				return
			}
			w.Write([]byte(`{"total_count":1,"workflow_runs":[{"id":5,"run_attempt":2,"status":"queued","conclusion":null,"head_sha":"abc"}]}`))
		case "/repos/octo-org/app.repo/actions/runs/5/jobs":
			if q.Get("filter") != "all" {
				http.Error(w, "query", 400)
				return
			}
			w.Write([]byte(`{"total_count":2,"jobs":[{"id":1,"run_attempt":1},{"id":2,"run_attempt":2}]}`))
		case "/repos/octo-org/app.repo/actions/runs/5/attempts/2/jobs":
			if q.Has("filter") {
				http.Error(w, "query", 400)
				return
			}
			w.Write([]byte(`{"total_count":1,"jobs":[{"id":2,"run_attempt":2}]}`))
		default:
			http.NotFound(w, r)
		}
	})
	runs, err := c.ListWorkflowRuns(context.Background(), repo, WorkflowRunListOptions{Status: "queued"})
	if err != nil || runs.HasNext || len(runs.Runs) != 1 || runs.Runs[0].RunAttempt != 2 || *runs.Runs[0].Status != "queued" || runs.Runs[0].Conclusion != nil {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	all, err := c.ListWorkflowRunJobs(context.Background(), repo, 5, WorkflowJobListOptions{Filter: "all"})
	if err != nil || len(all.Jobs) != 2 {
		t.Fatalf("all=%+v err=%v", all, err)
	}
	attempt, err := c.ListWorkflowRunJobs(context.Background(), repo, 5, WorkflowJobListOptions{Attempt: 2})
	if err != nil || len(attempt.Jobs) != 1 || attempt.Jobs[0].ID != 2 {
		t.Fatalf("attempt=%+v err=%v", attempt, err)
	}
	if _, err := c.ListWorkflowRunJobs(context.Background(), repo, 5, WorkflowJobListOptions{Attempt: 2, Filter: "all"}); err == nil {
		t.Fatal("accepted filter with attempt")
	}
}

func TestRateLimitErrorsCarryRetryTimeAndOmitBody(t *testing.T) {
	reset := time.Date(2026, 10, 9, 12, 30, 0, 0, time.UTC)
	cases := map[string]struct {
		status  int
		headers map[string]string
		want    time.Time
	}{
		"primary":   {403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": fmt.Sprint(reset.Unix())}, reset},
		"secondary": {429, map[string]string{"Retry-After": "120"}, time.Date(2026, 10, 9, 12, 2, 0, 0, time.UTC)},
		"fallback":  {429, nil, time.Date(2026, 10, 9, 12, 1, 0, 0, time.UTC)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, _, _ := newTestClient(t, pkcs1PEM(testKey), func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(`{"message":"leaked ghs_token1 SECRET"}`))
			})
			_, err := c.GetWorkflowJob(context.Background(), repo, 1)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || !apiErr.RateLimited || !apiErr.RetryAt.Equal(tc.want) || apiErr.StatusCode != tc.status {
				t.Fatalf("err=%#v", err)
			}
			if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "ghs_") {
				t.Fatalf("error leaks body: %v", err)
			}
		})
	}
	c, _, _ := newTestClient(t, pkcs1PEM(testKey), func(w http.ResponseWriter, r *http.Request) { http.Error(w, "forbidden", 403) })
	var apiErr *APIError
	if _, err := c.GetWorkflowJob(context.Background(), repo, 1); !errors.As(err, &apiErr) || apiErr.RateLimited {
		t.Fatalf("plain 403 marked rate limited: %v", err)
	}
}

func TestRedirectsAreNotFollowedWithCredentials(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
	}))
	defer other.Close()
	c, _, _ := newTestClient(t, pkcs1PEM(testKey), func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) })
	var apiErr *APIError
	if _, err := c.GetWorkflowJob(context.Background(), repo, 1); !errors.As(err, &apiErr) || apiErr.StatusCode != 302 {
		t.Fatalf("err=%v", err)
	}
	if leaked.Load() {
		t.Fatal("redirect target received credentials")
	}
}

func TestOversizedAndMalformedResponsesFail(t *testing.T) {
	c, _, _ := newTestClient(t, pkcs1PEM(testKey), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/octo-org/app.repo/actions/jobs/1" {
			w.Write([]byte(`{"id":1,"name":"`))
			io.Copy(w, io.LimitReader(filler{}, maxBody))
			w.Write([]byte(`"}`))
			return
		}
		w.Write([]byte(`{"id":`))
	})
	if _, err := c.GetWorkflowJob(context.Background(), repo, 1); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err=%v", err)
	}
	if _, err := c.GetWorkflowJob(context.Background(), repo, 2); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("err=%v", err)
	}
}

type filler struct{}

func (filler) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestRequestsHonourContextAndTimeout(t *testing.T) {
	release := make(chan struct{})
	c, _, _ := newTestClient(t, pkcs1PEM(testKey), func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	if _, err := c.InstallationToken(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.GetWorkflowJob(ctx, repo, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	c.timeout = 50 * time.Millisecond
	start := time.Now()
	if _, err := c.GetWorkflowJob(context.Background(), repo, 1); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("err=%v elapsed=%s", err, time.Since(start))
	}
}

func TestBaseURLValidation(t *testing.T) {
	key := pkcs1PEM(testKey)
	c, err := New(Config{AppID: "1", PrivateKey: key})
	if err != nil || c.base != "https://api.github.com" {
		t.Fatalf("default base=%v err=%v", c, err)
	}
	for _, bad := range []string{"http://api.github.com", "http://example.com:8080", "https://user:pw@ghe.example", "https://ghe.example/api/v3?x=1", "ftp://x"} {
		if _, err := New(Config{AppID: "1", PrivateKey: key, BaseURL: bad}); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := New(Config{AppID: "1", PrivateKey: key, BaseURL: "http://127.0.0.1:1234"}); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryPathRejectsTraversal(t *testing.T) {
	c, _, _ := newTestClient(t, pkcs1PEM(testKey), func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected request %s", r.URL) })
	for _, r := range []Repository{{42, "..", "x"}, {42, "o", "a/b"}, {0, "o", "r"}, {42, "o", "r?x"}} {
		if _, err := c.GetWorkflowJob(context.Background(), r, 1); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
}
