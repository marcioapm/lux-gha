package luxclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "key_testsecretvalue0000000000000000000000"

func newTestClient(t *testing.T, h http.HandlerFunc, mod ...func(*Config)) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := Config{BaseURL: srv.URL, APIKey: testKey, RequestTimeout: 5 * time.Second}
	for _, m := range mod {
		m(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func jitSpec() RunSpec {
	return RunSpec{
		Name:      "gha-job-42",
		Labels:    map[string]string{"lux-gha/job": "42", "lux-gha/attempt": "1"},
		ImageRef:  "ghcr.io/acme/runner@sha256:abc",
		Command:   []string{"/entrypoint", "--jit-file", "/run/secrets/jit"},
		Workdir:   "/home/runner",
		User:      "runner",
		Env:       map[string]string{"RUNNER_ALLOW_RUNASROOT": "0"},
		Secrets:   []FileSecret{{Name: "jit", Value: "JITCONFIG-SECRET-VALUE", Path: "/run/secrets/jit"}},
		Resources: Resources{CPUs: 2, Memory: 4 << 30, Disk: 20 << 30, Pids: 2048},
		Timeout:   90 * time.Minute,
		Grace:     90 * time.Second,
		Pool:      "gha",
		Egress:    []EgressRule{{Host: "github.com"}, {Host: "*.actions.githubusercontent.com"}, {CIDR: "140.82.112.0/20"}},
		Sandbox:   Sandbox{NestedContainers: true},
	}
}

// The exact body Lux's strict decoder receives for jitSpec.
const jitSpecWire = `{
	"name": "gha-job-42",
	"labels": {"lux-gha/attempt": "1", "lux-gha/job": "42"},
	"image": {"ref": "ghcr.io/acme/runner@sha256:abc"},
	"workload": {"adapter": "generic", "command": ["/entrypoint", "--jit-file", "/run/secrets/jit"], "workdir": "/home/runner", "user": "runner", "grace": "1m30s"},
	"env": {"RUNNER_ALLOW_RUNASROOT": "0"},
	"secrets": [{"name": "jit", "value": "JITCONFIG-SECRET-VALUE", "as": "file", "path": "/run/secrets/jit"}],
	"resources": {"cpus": 2, "memory": 4294967296, "disk": 21474836480, "pids": 2048},
	"timeout": "1h30m0s",
	"placement": {"pool": "gha"},
	"network": {"egress": [{"host": "github.com"}, {"host": "*.actions.githubusercontent.com"}, {"cidr": "140.82.112.0/20"}]},
	"sandbox": {"nestedContainers": true},
	"resumePolicy": "never"
}`

const createdRun = `{"id":"run_abcdefghijklmnop","tenant":"ci","labels":{"lux-gha/job":"42"},"state":"submitted","resumable":false,
	"epoch":0,"spec":{"image":{"ref":"x"},"workload":{"adapter":"generic"},"resumePolicy":"never","secrets":[{"name":"jit","as":"file","path":"/run/secrets/jit"}]},
	"secrets":[{"name":"jit","fingerprint":"00"}],"createdAt":"2026-10-09T10:00:00Z","runtimeSeconds":0}`

func jsonEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("body mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestCreateSendsExactRequest(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runs" || r.URL.RawQuery != "" {
			t.Errorf("request %s %s", r.Method, r.URL)
		}
		for h, want := range map[string]string{
			"Authorization":   "Bearer " + testKey,
			"Idempotency-Key": "gha/123/job/456/attempt/1",
			"Content-Type":    "application/json",
		} {
			if got := r.Header.Get(h); got != want {
				t.Errorf("%s = %q, want %q", h, got, want)
			}
		}
		body, _ := io.ReadAll(r.Body)
		jsonEqual(t, body, jitSpecWire)
		writeJSON(w, http.StatusCreated, createdRun)
	})
	run, err := c.Create(context.Background(), "gha/123/job/456/attempt/1", jitSpec())
	if err != nil {
		t.Fatal(err)
	}
	want := &Run{
		ID: "run_abcdefghijklmnop", Labels: map[string]string{"lux-gha/job": "42"}, State: "submitted",
		Spec: RunSpecEcho{ResumePolicy: "never"}, CreatedAt: time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC),
	}
	if !reflect.DeepEqual(run, want) {
		t.Fatalf("run = %+v, want %+v", run, want)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestValidateAcceptsLabelsAtLuxLimits(t *testing.T) {
	s := jitSpec()
	s.Labels["A/z0._-9"] = strings.Repeat("é", MaxLabelValue/2)
	s.Labels[strings.Repeat("a", 64)] = "x"
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateMinimalSpecOmitsOptionalFields(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		jsonEqual(t, body, `{"image":{"ref":"img"},"workload":{"adapter":"generic","command":["run"]},
			"resources":{},"placement":{},"network":{},"sandbox":{},"resumePolicy":"never"}`)
		writeJSON(w, http.StatusCreated, createdRun)
	})
	if _, err := c.Create(context.Background(), "k", RunSpec{ImageRef: "img", Command: []string{"run"}}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateNetworkWire(t *testing.T) {
	cases := map[string]struct {
		spec RunSpec
		want string
	}{
		"host rules": {
			RunSpec{ImageRef: "img", Command: []string{"run"}, Egress: []EgressRule{{Host: "github.com"}}},
			`{"image":{"ref":"img"},"workload":{"adapter":"generic","command":["run"]},
				"resources":{},"placement":{},"network":{"egress":[{"host":"github.com"}]},"sandbox":{},"resumePolicy":"never"}`,
		},
		"unrestricted": {
			RunSpec{ImageRef: "img", Command: []string{"run"}, UnrestrictedEgress: true},
			`{"image":{"ref":"img"},"workload":{"adapter":"generic","command":["run"]},
				"resources":{},"placement":{},"network":{"unrestricted":true},"sandbox":{},"resumePolicy":"never"}`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				jsonEqual(t, body, tc.want)
				writeJSON(w, http.StatusCreated, createdRun)
			})
			if _, err := c.Create(context.Background(), "k", tc.spec); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCreateIdempotentReplayIsAccepted(t *testing.T) {
	keys := map[string]int{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		keys[r.Header.Get("Idempotency-Key")]++
		status := http.StatusCreated
		if keys[r.Header.Get("Idempotency-Key")] > 1 {
			status = http.StatusOK
		}
		writeJSON(w, status, createdRun)
	})
	a, err := c.Create(context.Background(), "same", jitSpec())
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Create(context.Background(), "same", jitSpec())
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || keys["same"] != 2 {
		t.Fatalf("ids %s %s, keys %v", a.ID, b.ID, keys)
	}
}

func TestCreateRequiresNeverEcho(t *testing.T) {
	for name, specJSON := range map[string]string{
		"absent": `{}`,
		"auto":   `{"resumePolicy":"auto"}`,
		"empty":  `{"resumePolicy":""}`,
		"case":   `{"resumePolicy":"Never"}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusCreated, `{"id":"run_x","state":"submitted","spec":`+specJSON+`,"createdAt":"2026-10-09T10:00:00Z"}`)
			})
			run, err := c.Create(context.Background(), "k", jitSpec())
			if !errors.Is(err, ErrResumePolicyNotEchoed) {
				t.Fatalf("err = %v", err)
			}
			if run == nil || run.ID != "run_x" {
				t.Fatalf("run = %+v; the created Run must be returned for termination", run)
			}
		})
	}
}

func TestCreateRejectsMissingID(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, `{"spec":{"resumePolicy":"never"}}`)
	})
	if run, err := c.Create(context.Background(), "k", jitSpec()); err == nil || run != nil {
		t.Fatalf("run %+v err %v", run, err)
	}
}

func TestCreateIsNeverRetried(t *testing.T) {
	t.Run("server error", func(t *testing.T) {
		var calls atomic.Int32
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			writeJSON(w, http.StatusServiceUnavailable, `{"error":{"code":"unavailable","message":"x"}}`)
		})
		_, err := c.Create(context.Background(), "k", jitSpec())
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != 503 || ae.Code != "unavailable" || calls.Load() != 1 {
			t.Fatalf("err %v calls %d", err, calls.Load())
		}
	})
	t.Run("connection dropped", func(t *testing.T) {
		var posts atomic.Int32
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				writeJSON(w, http.StatusOK, `{"id":"run_warm"}`)
				return
			}
			posts.Add(1)
			_, _ = io.ReadAll(r.Body)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		})
		// The GET leaves a reused keep-alive connection: the case in which
		// net/http replays a request carrying Idempotency-Key whose body it
		// can rewind.
		if _, err := c.Get(context.Background(), "run_warm"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Create(context.Background(), "k", jitSpec()); err == nil {
			t.Fatal("expected error")
		}
		if posts.Load() != 1 {
			t.Fatalf("POST sent %d times", posts.Load())
		}
	})
}

func TestCreateSizeSendsOnlySize(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		jsonEqual(t, body, `{"image":{"ref":"img"},"workload":{"adapter":"generic","command":["run"]},
			"resources":{"size":"m-2-arm64","pids":512},"placement":{"pool":"shaped"},"network":{},"sandbox":{},"resumePolicy":"never"}`)
		writeJSON(w, http.StatusCreated, createdRun)
	})
	spec := RunSpec{ImageRef: "img", Command: []string{"run"}, Pool: "shaped", Resources: Resources{Size: "m-2-arm64", Pids: 512}}
	if _, err := c.Create(context.Background(), "k", spec); err != nil {
		t.Fatal(err)
	}
}

// The names a shape's ladder can have: luxd's internal/sizing builds
// <family>-<cpus>-<arch> and accepts no other spelling.
func TestValidSizeName(t *testing.T) {
	long := strings.Repeat("f", maxSizeName-len("-1-arm64"))
	for _, name := range []string{"m-05-arm64", "m-1-arm64", "m-2-arm64", "m-8-amd64", "c-1024-arm64", "gp-2-4-arm64",
		"0-1-arm64", long + "-1-arm64", "m-9007199254740992-arm64"} {
		if !ValidSizeName(name) {
			t.Errorf("%q refused", name)
		}
	}
	for _, name := range []string{"", "m-2", "m2-arm64", "M-2-arm64", "m-2-ARM64", "m-2-x86_64", "m-2-arm", "lux-m-2", "-m-2-arm64",
		"m--2-arm64", "m- -2-arm64", "m-0.5-arm64", "m-005-arm64", "m-02-arm64", "m-0-arm64", "m-3-arm64", "m-6-arm64", "m-2.0-arm64",
		"m-+2-arm64", " m-2-arm64", "m-2-arm64\n", "m_x-2-arm64", long + "f-1-arm64", "m-18014398509481984-arm64"} {
		if ValidSizeName(name) {
			t.Errorf("%q accepted", name)
		}
	}
}

func TestCreateRefusesBeforeSending(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	leak := func(f func(*RunSpec)) RunSpec { s := jitSpec(); f(&s); return s }
	cases := map[string]struct {
		key  string
		spec RunSpec
	}{
		"empty key":           {"", jitSpec()},
		"key with newline":    {"a\nb", jitSpec()},
		"no image":            {"k", leak(func(s *RunSpec) { s.ImageRef = "" })},
		"no command":          {"k", leak(func(s *RunSpec) { s.Command = nil })},
		"relative path":       {"k", leak(func(s *RunSpec) { s.Secrets[0].Path = "jit" })},
		"empty secret":        {"k", leak(func(s *RunSpec) { s.Secrets[0].Value = "" })},
		"reserved name":       {"k", leak(func(s *RunSpec) { s.Secrets[0].Name = "lux-jit" })},
		"duplicate secret":    {"k", leak(func(s *RunSpec) { s.Secrets = append(s.Secrets, s.Secrets[0]) })},
		"secret in label":     {"k", leak(func(s *RunSpec) { s.Labels["jit"] = "JITCONFIG-SECRET-VALUE" })},
		"secret in label key": {"k", leak(func(s *RunSpec) { s.Labels["JITCONFIG-SECRET-VALUE"] = "present" })},
		"secret in env":       {"k", leak(func(s *RunSpec) { s.Env["JIT"] = "JITCONFIG-SECRET-VALUE" })},
		"secret in command":   {"k", leak(func(s *RunSpec) { s.Command = append(s.Command, "--jit=JITCONFIG-SECRET-VALUE") })},
		"secret in name":      {"k", leak(func(s *RunSpec) { s.Name = "JITCONFIG-SECRET-VALUE" })},
		"egress both":         {"k", leak(func(s *RunSpec) { s.Egress = []EgressRule{{Host: "a", CIDR: "1.2.3.0/24"}} })},
		"egress bad cidr":     {"k", leak(func(s *RunSpec) { s.Egress = []EgressRule{{CIDR: "nope"}} })},
		"egress unrestricted": {"k", leak(func(s *RunSpec) { s.UnrestrictedEgress = true })},
		"negative resources":  {"k", leak(func(s *RunSpec) { s.Resources.CPUs = -1 })},
		"negative grace":      {"k", leak(func(s *RunSpec) { s.Grace = -time.Second })},
		"reserved env":        {"k", leak(func(s *RunSpec) { s.Env["LUX_X"] = "1" })},
		"label key space":     {"k", leak(func(s *RunSpec) { s.Labels["lux-gha/KEY SENTINEL"] = "x" })},
		"label key empty":     {"k", leak(func(s *RunSpec) { s.Labels[""] = "x" })},
		"label key ends -":    {"k", leak(func(s *RunSpec) { s.Labels["x-"] = "x" })},
		"label key starts -":  {"k", leak(func(s *RunSpec) { s.Labels["-x"] = "x" })},
		"label key 65":        {"k", leak(func(s *RunSpec) { s.Labels["KEY-SENTINEL"+strings.Repeat("a", 65-len("KEY-SENTINEL"))] = "x" })},
		"label value 257":     {"k", leak(func(s *RunSpec) { s.Labels["lux-gha/x"] = "VALUE-SENTINEL" + strings.Repeat("é", 128) })},
		"size with cpus":      {"k", leak(func(s *RunSpec) { s.Resources = Resources{Size: "m-2-arm64", CPUs: 2} })},
		"size with memory":    {"k", leak(func(s *RunSpec) { s.Resources = Resources{Size: "m-2-arm64", Memory: 1} })},
		"size with disk":      {"k", leak(func(s *RunSpec) { s.Resources = Resources{Size: "m-2-arm64", Disk: 1} })},
		"size bad name":       {"k", leak(func(s *RunSpec) { s.Resources = Resources{Size: "M-2-arm64"} })},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := c.Create(context.Background(), tc.key, tc.spec)
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), "JITCONFIG-SECRET-VALUE") {
				t.Fatalf("error leaks secret: %v", err)
			}
			// Every label sentinel (KEY SENTINEL, KEY-SENTINEL, VALUE-SENTINEL) holds this.
			if strings.Contains(err.Error(), "SENTINEL") {
				t.Fatalf("error quotes a label: %v", err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("%d requests sent", calls.Load())
	}
}

func TestGetAndTerminateEscapeID(t *testing.T) {
	const run = `{"id":"run_1","labels":{},"state":"terminated","stateReason":"succeeded; resumePolicy never","exitCode":0,
		"spec":{"resumePolicy":"never"},"createdAt":"2026-10-09T10:00:00Z","firstStartedAt":"2026-10-09T10:01:00Z","finishedAt":"2026-10-09T10:05:00Z"}`
	var seen []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.EscapedPath()+" "+r.URL.RawQuery)
		if r.Header.Get("Authorization") != "Bearer "+testKey || r.Header.Get("Idempotency-Key") != "" {
			t.Errorf("headers %v", r.Header)
		}
		if r.Method == http.MethodPost {
			if b, _ := io.ReadAll(r.Body); len(b) != 0 {
				t.Errorf("terminate body %q", b)
			}
			writeJSON(w, http.StatusAccepted, run)
			return
		}
		writeJSON(w, http.StatusOK, run)
	})
	got, err := c.Get(context.Background(), "run/../x?y#z")
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	started := time.Date(2026, 10, 9, 10, 1, 0, 0, time.UTC)
	finished := time.Date(2026, 10, 9, 10, 5, 0, 0, time.UTC)
	want := &Run{ID: "run_1", Labels: map[string]string{}, State: "terminated", StateReason: "succeeded; resumePolicy never",
		ExitCode: &zero, Spec: RunSpecEcho{ResumePolicy: "never"}, CreatedAt: time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC),
		FirstStartedAt: &started, FinishedAt: &finished}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if _, err := c.Terminate(context.Background(), "run 1"); err != nil {
		t.Fatal(err)
	}
	wantSeen := []string{"GET /v1/runs/run%2F..%2Fx%3Fy%23z ", "POST /v1/runs/run%201/terminate "}
	if !reflect.DeepEqual(seen, wantSeen) {
		t.Fatalf("seen %q", seen)
	}
	for _, id := range []string{"", ".", ".."} {
		if _, err := c.Get(context.Background(), id); err == nil {
			t.Fatalf("Get(%q) accepted", id)
		}
		if _, err := c.Terminate(context.Background(), id); err == nil {
			t.Fatalf("Terminate(%q) accepted", id)
		}
	}
}

func TestTerminateRequires202(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, `{"error":{"code":"not_found","message":"not found"}}`)
	})
	_, err := c.Terminate(context.Background(), "run_gone")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 404 || ae.Code != "not_found" || ae.Op != "terminate run" {
		t.Fatalf("err %#v", err)
	}
}

func TestListPagesWithLabelsAndCursor(t *testing.T) {
	var queries []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		queries = append(queries, fmt.Sprintf("label=%q sort=%s limit=%s next=%s", q["label"], q.Get("sort"), q.Get("limit"), q.Get("next")))
		switch q.Get("next") {
		case "":
			writeJSON(w, 200, `{"runs":[{"id":"run_3","state":"running","spec":{"resumePolicy":"never"}},{"id":"run_2"}],"next":"c+/=1","page":"p"}`)
		case "c+/=1":
			writeJSON(w, 200, `{"runs":[{"id":"run_2"},{"id":"run_1"}],"next":"c2","prev":"x"}`)
		case "c2":
			writeJSON(w, 200, `{"runs":[],"prev":"y"}`)
		default:
			t.Errorf("unexpected cursor %q", q.Get("next"))
		}
	})
	runs, err := c.List(context.Background(), map[string]string{"lux-gha/repo": "acme/api", "a": "b=c&d"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	if !reflect.DeepEqual(ids, []string{"run_3", "run_2", "run_1"}) {
		t.Fatalf("ids %v", ids)
	}
	if runs[0].State != "running" || runs[0].Spec.ResumePolicy != "never" {
		t.Fatalf("run %+v", runs[0])
	}
	labels := `["a=b=c&d" "lux-gha/repo=acme/api"]`
	want := []string{
		"label=" + labels + " sort=created limit=200 next=",
		"label=" + labels + " sort=created limit=200 next=c+/=1",
		"label=" + labels + " sort=created limit=200 next=c2",
	}
	if !reflect.DeepEqual(queries, want) {
		t.Fatalf("queries\n%q\nwant\n%q", queries, want)
	}
}

func TestListCreatedSinceStopsAtTheFirstOlderRun(t *testing.T) {
	var cursors []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		cursors = append(cursors, q.Get("next"))
		if q.Get("state") != "terminated,failed" || q.Get("sort") != "created" {
			t.Errorf("query %v", q)
		}
		switch q.Get("next") {
		case "":
			writeJSON(w, 200, `{"runs":[{"id":"run_4","createdAt":"2026-10-09T12:00:00Z"},{"id":"run_3","createdAt":"2026-10-09T11:00:00Z"}],"next":"c1"}`)
		case "c1":
			writeJSON(w, 200, `{"runs":[{"id":"run_2","createdAt":"2026-10-09T10:00:00Z"},{"id":"run_1","createdAt":"2026-10-09T09:59:59Z"}],"next":"c2"}`)
		default:
			t.Errorf("paged past the cutoff: %q", q.Get("next"))
		}
	})
	since := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	runs, err := c.ListCreatedSince(context.Background(), map[string]string{"lux-gha/managed": "t1"}, since, "terminated", "failed")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	if !reflect.DeepEqual(ids, []string{"run_4", "run_3", "run_2"}) || !reflect.DeepEqual(cursors, []string{"", "c1"}) {
		t.Fatalf("ids %v cursors %q", ids, cursors)
	}
	if _, err := c.ListCreatedSince(context.Background(), nil, time.Time{}); err == nil {
		t.Fatal("accepted a zero since")
	}
}

func TestListSendsStateFilterOnEveryPage(t *testing.T) {
	var queries []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		queries = append(queries, fmt.Sprintf("state=%q label=%q next=%s", q["state"], q["label"], q.Get("next")))
		switch q.Get("next") {
		case "":
			writeJSON(w, 200, `{"runs":[{"id":"run_2","state":"running"}],"next":"c1"}`)
		case "c1":
			writeJSON(w, 200, `{"runs":[{"id":"run_1","state":"submitted"}],"next":"c2"}`)
		default:
			writeJSON(w, 200, `{"runs":[]}`)
		}
	})
	runs, err := c.List(context.Background(), map[string]string{"lux-gha/repo": "acme/api"}, "submitted", "running", "stopping")
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs %v err %v", runs, err)
	}
	want := []string{
		`state=["submitted,running,stopping"] label=["lux-gha/repo=acme/api"] next=`,
		`state=["submitted,running,stopping"] label=["lux-gha/repo=acme/api"] next=c1`,
		`state=["submitted,running,stopping"] label=["lux-gha/repo=acme/api"] next=c2`,
	}
	if !reflect.DeepEqual(queries, want) {
		t.Fatalf("queries\n%q\nwant\n%q", queries, want)
	}
}

func TestListRejectsInvalidStateBeforeRequest(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	for _, states := range [][]string{{"bogus"}, {"running", ""}, {"running,stopped"}, {"Running"}} {
		if _, err := c.List(context.Background(), nil, states...); err == nil {
			t.Errorf("states %q accepted", states)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("%d requests sent", calls.Load())
	}
}

func TestListEmptyMatch(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("label") || r.URL.Query().Has("state") {
			t.Errorf("unexpected label or state")
		}
		writeJSON(w, 200, `{"runs":[]}`)
	})
	runs, err := c.List(context.Background(), nil)
	if err != nil || len(runs) != 0 {
		t.Fatalf("runs %v err %v", runs, err)
	}
}

func TestListFailsClosed(t *testing.T) {
	t.Run("repeated cursor", func(t *testing.T) {
		var calls atomic.Int32
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.URL.Query().Get("next") == "" {
				writeJSON(w, 200, `{"runs":[{"id":"run_1"}],"next":"a"}`)
				return
			}
			writeJSON(w, 200, `{"runs":[{"id":"run_2"}],"next":"a"}`)
		})
		runs, err := c.List(context.Background(), nil)
		if !errors.Is(err, ErrRepeatedCursor) || runs != nil || calls.Load() != 2 {
			t.Fatalf("runs %v err %v calls %d", runs, err, calls.Load())
		}
	})
	t.Run("page bound", func(t *testing.T) {
		var calls atomic.Int32
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			writeJSON(w, 200, fmt.Sprintf(`{"runs":[{"id":"run_%d"}],"next":"c%d"}`, n, n))
		}, func(cfg *Config) { cfg.MaxPages = 3 })
		runs, err := c.List(context.Background(), nil)
		if !errors.Is(err, ErrTooManyPages) || runs != nil || calls.Load() != 3 {
			t.Fatalf("runs %v err %v calls %d", runs, err, calls.Load())
		}
	})
	t.Run("error mid-way", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("next") == "" {
				writeJSON(w, 200, `{"runs":[{"id":"run_1"}],"next":"a"}`)
				return
			}
			writeJSON(w, 400, `{"error":{"code":"bad_request","message":"next: not a cursor"}}`)
		})
		runs, err := c.List(context.Background(), nil)
		var ae *APIError
		if !errors.As(err, &ae) || ae.Code != "bad_request" || runs != nil {
			t.Fatalf("runs %v err %v", runs, err)
		}
	})
	t.Run("invalid selector", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("request sent") })
		for _, k := range []string{"", "a=b"} {
			if _, err := c.List(context.Background(), map[string]string{k: "v"}); err == nil {
				t.Fatalf("key %q accepted", k)
			}
		}
	})
}

func openAPIDoc(version, runSpecProps string) string {
	return `{"openapi":"3.1.0","info":{"title":"lux","version":"` + version + `"},
		"components":{"schemas":{"RunSpec":{"type":"object","properties":` + runSpecProps + `}}}}`
}

const withResumePolicy = `{"image":{"$ref":"#/components/schemas/Image"},"resumePolicy":{"type":"string","description":"..."}}`

func TestCheckCompatibility(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		ok     bool
	}{
		{"minimum", 200, openAPIDoc("v0.1.14", withResumePolicy), true},
		{"newer patch", 200, openAPIDoc("v0.1.16", withResumePolicy), true},
		{"newer minor", 200, openAPIDoc("v0.2.0", withResumePolicy), true},
		{"newer major", 200, openAPIDoc("v1.0.0", withResumePolicy), true},
		{"multi-digit", 200, openAPIDoc("v0.1.140", withResumePolicy), true},
		{"older", 200, openAPIDoc("v0.1.13", withResumePolicy), false},
		{"older multi-digit", 200, openAPIDoc("v0.0.99", withResumePolicy), false},
		{"dev build", 200, openAPIDoc("dev", withResumePolicy), false},
		{"git describe", 200, openAPIDoc("v0.1.16-1-g417e6e6", withResumePolicy), false},
		{"dirty", 200, openAPIDoc("v0.1.16-dirty", withResumePolicy), false},
		{"prerelease", 200, openAPIDoc("v0.1.15-rc.1", withResumePolicy), false},
		{"build metadata", 200, openAPIDoc("v0.1.15+meta", withResumePolicy), false},
		{"no v prefix", 200, openAPIDoc("0.1.14", withResumePolicy), false},
		{"leading zero", 200, openAPIDoc("v0.01.14", withResumePolicy), false},
		{"short", 200, openAPIDoc("v0.1", withResumePolicy), false},
		{"overflow", 200, openAPIDoc("v99999999999999999999.0.0", withResumePolicy), false},
		{"empty version", 200, openAPIDoc("", withResumePolicy), false},
		{"no info", 200, `{"components":{"schemas":{"RunSpec":{"properties":` + withResumePolicy + `}}}}`, false},
		{"no resumePolicy", 200, openAPIDoc("v0.1.16", `{"image":{}}`), false},
		{"resumePolicy not a schema", 200, openAPIDoc("v0.1.16", `{"resumePolicy":true}`), false},
		{"no RunSpec", 200, `{"info":{"version":"v0.1.16"},"components":{"schemas":{}}}`, false},
		{"not JSON", 200, `<html>`, false},
		{"not found", 404, `{"error":{"code":"not_found","message":"x"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/openapi.json" {
					t.Errorf("%s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "" {
					t.Error("API key sent to the unauthenticated spec endpoint")
				}
				writeJSON(w, tc.status, tc.body)
			})
			got, err := c.CheckCompatibility(context.Background())
			if tc.ok != (err == nil) {
				t.Fatalf("compatibility %+v err %v", got, err)
			}
			if tc.status == 200 && !tc.ok && !errors.Is(err, ErrIncompatible) && !strings.Contains(err.Error(), "malformed") {
				t.Fatalf("err %v", err)
			}
		})
	}
	// Sizes is reported, never required: an older luxd stays compatible.
	sizeCases := []struct {
		name      string
		resources string
		want      Compatibility
	}{
		{"no Resources schema", "", Compatibility{Version: "v0.1.23"}},
		{"Resources without size", `,"Resources":{"type":"object","properties":{"cpus":{"type":"number"}}}`, Compatibility{Version: "v0.1.23"}},
		{"size not a schema", `,"Resources":{"type":"object","properties":{"size":true}}`, Compatibility{Version: "v0.1.23"}},
		{"Resources with size", `,"Resources":{"type":"object","properties":{"cpus":{"type":"number"},"size":{"type":"string"}}}`, Compatibility{Version: "v0.1.23", Sizes: true}},
		{"size as $ref", `,"Resources":{"type":"object","properties":{"size":{"$ref":"#/components/schemas/SizeName"}}}`, Compatibility{Version: "v0.1.23", Sizes: true}},
		// luxd v0.1.24's own shape of the property.
		{"size with description", `,"Resources":{"additionalProperties":false,"properties":{"cpus":{"format":"double","type":"number"},` +
			`"size":{"description":"A size of a shaped pool's ladder (e.g. m-2-arm64; GET /v1/pools lists each pool's sizes).","type":"string"}},"type":"object"}`,
			Compatibility{Version: "v0.1.23", Sizes: true}},
	}
	for _, tc := range sizeCases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, 200, `{"openapi":"3.1.0","info":{"version":"v0.1.23"},
					"components":{"schemas":{"RunSpec":{"type":"object","properties":`+withResumePolicy+`}`+tc.resources+`}}}`)
			})
			got, err := c.CheckCompatibility(context.Background())
			if err != nil || got != tc.want {
				t.Fatalf("compatibility %+v err %v, want %+v", got, err, tc.want)
			}
		})
	}
}

func TestHTTPErrorsCarryOnlyStatusAndCode(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{"lux envelope", 422, `{"error":{"code":"invalid_spec","message":"invalid spec: JITCONFIG-SECRET-VALUE","details":["JITCONFIG-SECRET-VALUE"]}}`, "invalid_spec"},
		{"unauthorized", 401, `{"error":{"code":"unauthorized","message":"missing API key"}}`, "unauthorized"},
		{"quota", 429, `{"error":{"code":"quota_exceeded","message":"quota"}}`, "quota_exceeded"},
		{"proxy html", 502, `<html>JITCONFIG-SECRET-VALUE</html>`, ""},
		{"hostile code", 400, `{"error":{"code":"JITCONFIG-SECRET-VALUE"}}`, ""},
		{"huge body", 500, `{"error":{"code":"internal","message":"` + strings.Repeat("x", 1<<20) + `"}}`, ""},
		{"redirect", 307, ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.status == 307 {
					w.Header().Set("Location", "http://127.0.0.1:1/steal")
				}
				writeJSON(w, tc.status, tc.body)
			})
			_, err := c.Create(context.Background(), "k", jitSpec())
			var ae *APIError
			if !errors.As(err, &ae) || ae.Status != tc.status || ae.Code != tc.code {
				t.Fatalf("err %#v", err)
			}
			for _, s := range []string{"JITCONFIG-SECRET-VALUE", testKey, "message", "127.0.0.1"} {
				if strings.Contains(err.Error(), s) {
					t.Fatalf("error %q contains %q", err, s)
				}
			}
		})
	}
}

func TestOversizedAndMalformedResponses(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/runs/big" {
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"id":"`)
			_, _ = io.Copy(w, io.LimitReader(zeros{}, maxBody+10))
			return
		}
		writeJSON(w, 200, `{"id": JITCONFIG-SECRET-VALUE}`)
	})
	if _, err := c.Get(context.Background(), "big"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err %v", err)
	}
	_, err := c.Get(context.Background(), "bad")
	if err == nil || strings.Contains(err.Error(), "JITCONFIG") {
		t.Fatalf("err %v", err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestContextCancellation(t *testing.T) {
	t.Run("already cancelled", func(t *testing.T) {
		var calls atomic.Int32
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := c.Create(ctx, "k", jitSpec()); !errors.Is(err, context.Canceled) {
			t.Fatalf("create err %v", err)
		}
		if _, err := c.List(ctx, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("list err %v", err)
		}
		if _, err := c.CheckCompatibility(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("compat err %v", err)
		}
		if calls.Load() != 0 {
			t.Fatalf("%d requests sent", calls.Load())
		}
	})
	t.Run("cancelled in flight", func(t *testing.T) {
		release := make(chan struct{})
		arrived := make(chan struct{})
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			close(arrived)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		})
		defer close(release)
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-arrived; cancel() }()
		if _, err := c.Terminate(ctx, "run_1"); !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("request timeout", func(t *testing.T) {
		release := make(chan struct{})
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}, func(cfg *Config) { cfg.RequestTimeout = 50 * time.Millisecond })
		defer close(release)
		if _, err := c.Get(context.Background(), "run_1"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err %v", err)
		}
	})
}

func TestNewValidatesConfig(t *testing.T) {
	for name, cfg := range map[string]Config{
		"plain http remote": {BaseURL: "http://lux.example.com", APIKey: "k"},
		"path":              {BaseURL: "https://lux.example.com/api", APIKey: "k"},
		"query":             {BaseURL: "https://lux.example.com?x=1", APIKey: "k"},
		"userinfo":          {BaseURL: "https://u:p@lux.example.com", APIKey: "k"},
		"no host":           {BaseURL: "https://", APIKey: "k"},
		"no key":            {BaseURL: "https://lux.example.com"},
		"key with newline":  {BaseURL: "https://lux.example.com", APIKey: "k\r\nX: y"},
		"negative timeout":  {BaseURL: "https://lux.example.com", APIKey: "k", RequestTimeout: -1},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "k\r\n") {
			t.Errorf("%s: error leaks key", name)
		}
	}
	for _, base := range []string{"https://lux.example.com", "https://lux.example.com/", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		if _, err := New(Config{BaseURL: base, APIKey: "k"}); err != nil {
			t.Errorf("%s: %v", base, err)
		}
	}
}

func TestRunSpecMarshalValidates(t *testing.T) {
	s := jitSpec()
	s.Labels["x"] = s.Secrets[0].Value
	b, err := json.Marshal(s)
	if err == nil || bytes.Contains(b, []byte("JITCONFIG")) {
		t.Fatalf("marshal %s err %v", b, err)
	}
}
