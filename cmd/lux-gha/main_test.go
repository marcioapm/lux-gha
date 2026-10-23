package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux-gha/config"
	"github.com/marcioapm/lux-gha/luxclient"
)

const webhookSecret = "cmd-test-webhook-secret"

// fakeLux serves GET /openapi.json, reporting version, and an empty
// GET /v1/runs for the reconciler; any other request, such as a Run
// submission, fails the test. Its Resources schema declares size.
func fakeLux(t *testing.T, version string) string {
	return fakeLuxSchemas(t, version, `,"Resources":{"type":"object","properties":{"size":{"type":"string"}}}`)
}

// fakeLuxSchemas is fakeLux with extra appended to components.schemas
// after RunSpec.
func fakeLuxSchemas(t *testing.T, version, extra string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/runs" {
			io.WriteString(w, `{"runs":[]}`)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/openapi.json" {
			t.Errorf("unexpected Lux request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"openapi":"3.1.0","info":{"version":"`+version+`"},
			"components":{"schemas":{"RunSpec":{"type":"object","properties":{"resumePolicy":{"type":"string"}}}`+extra+`}}}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// withSizeRoute adds route "m-2", which asks for a size, after the raw one.
func withSizeRoute(c map[string]any) {
	c["routes"] = append(c["routes"].([]map[string]any), map[string]any{
		"name": "m-2", "labels": []string{"lux-m-2-arm64"}, "pool": "shaped", "size": "m-2-arm64",
		"image": "ghcr.io/acme/runner@sha256:abc", "timeout": "90m",
	})
}

// fakeGitHub serves an installation token, acme/widgets with no workflow
// runs or runners, and sends on passes each time a reconciler pass lists the
// repository's runners, its last GitHub read.
func fakeGitHub(t *testing.T) (string, chan struct{}) {
	url, passes, _ := fakeInstallation(t)
	return url, passes
}

// fakeInstallation is fakeGitHub that also lists acme/widgets as the
// installation's one repository, sending on listings each time.
func fakeInstallation(t *testing.T) (string, chan struct{}, chan struct{}) {
	t.Helper()
	passes := make(chan struct{}, 16)
	listings := make(chan struct{}, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			io.WriteString(w, `{"total_count":1,"repositories":[{"id":1001,"full_name":"acme/widgets"}]}`)
			listings <- struct{}{}
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets":
			io.WriteString(w, `{"id":1001,"full_name":"acme/widgets"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/actions/runs":
			io.WriteString(w, `{"total_count":0,"workflow_runs":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/actions/runners":
			io.WriteString(w, `{"total_count":0,"runners":[]}`)
			passes <- struct{}{}
		default:
			t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, passes, listings
}

// closedGitHub is a loopback URL nothing listens on: every pass fails at GitHub.
const closedGitHub = "http://127.0.0.1:1"

// writeConfig writes a valid configuration and its secret files to a temp
// directory.
func writeConfig(t *testing.T, luxURL, githubURL string, edits ...func(map[string]any)) string {
	t.Helper()
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"app.pem":        pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
		"webhook-secret": []byte(webhookSecret + "\n"),
		"lux-api-key":    []byte("lux-test-key\n"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := map[string]any{
		"listen": "127.0.0.1:0", "instance": "test", "maxRunners": 4,
		"github": map[string]any{
			"appID": "Iv1.test", "installationID": 42, "runnerGroupID": 1,
			"apiURL":         githubURL,
			"privateKeyFile": filepath.Join(dir, "app.pem"), "webhookSecretFile": filepath.Join(dir, "webhook-secret"),
		},
		"lux":          map[string]any{"url": luxURL, "apiKeyFile": filepath.Join(dir, "lux-api-key")},
		"reconcile":    map[string]any{"interval": "60s"},
		"repositories": []string{"acme/widgets"},
		"routes": []map[string]any{{
			"name": "default", "labels": []string{"self-hosted", "linux", "lux"}, "pool": "gha",
			"image": "ghcr.io/acme/runner@sha256:abc", "timeout": "90m",
		}},
	}
	for _, edit := range edits {
		edit(cfg)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The release build's ldflags, taken from scripts/dist.sh so a wrong -X path
// there fails here, set the version --version prints, with exit 0 and nothing
// else read: there is no config at the default path here.
func TestVersionFlagPrintsLinkedVersion(t *testing.T) {
	printFlags := exec.Command("bash", filepath.Join("..", "..", "scripts", "dist.sh"), "--print-ldflags")
	printFlags.Env = append(os.Environ(), "VERSION=v9.8.7")
	ldflags, err := printFlags.Output()
	if err != nil {
		t.Fatalf("dist.sh --print-ldflags: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "lux-gha")
	build := exec.Command("go", "build", "-trimpath", "-o", bin,
		"-ldflags", strings.TrimSpace(string(ldflags)), ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, "--version", "-config", filepath.Join(t.TempDir(), "absent.json"))
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("--version: %v (stderr %q)", err, stderr.String())
	}
	if stdout.String() != "v9.8.7\n" || stderr.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

// servingHandler reports the bound address from run's "serving" record.
type servingHandler struct{ addr chan string }

func (h servingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h servingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h servingHandler) WithGroup(string) slog.Handler            { return h }
func (h servingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "serving" {
		return nil
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "listen" {
			h.addr <- a.Value.String()
			return false
		}
		return true
	})
	return nil
}

func TestRunRefusesIncompatibleLuxBeforeServing(t *testing.T) {
	for _, version := range []string{"dev", "v0.1.13"} {
		t.Run(version, func(t *testing.T) {
			path := writeConfig(t, fakeLux(t, version), closedGitHub)
			served := make(chan string, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := run(ctx, path, slog.New(servingHandler{served}))
			if !errors.Is(err, luxclient.ErrIncompatible) {
				t.Fatalf("err=%v, want ErrIncompatible", err)
			}
			select {
			case addr := <-served:
				t.Fatalf("served on %s despite incompatible Lux", addr)
			default:
			}
		})
	}
}

func TestRunRefusesSizeRouteOnLuxWithoutSizes(t *testing.T) {
	path := writeConfig(t, fakeLuxSchemas(t, "v0.1.23", ""), closedGitHub, withSizeRoute)
	served := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := run(ctx, path, slog.New(servingHandler{served}))
	want := `lux compatibility: route "m-2" asks for a size; luxd v0.1.23 predates sizes (v0.1.24)`
	if err == nil || err.Error() != want {
		t.Fatalf("err=%v, want %q", err, want)
	}
	select {
	case addr := <-served:
		t.Fatalf("served on %s despite a size route Lux cannot take", addr)
	default:
	}
}

// startsServing runs the bridge until it serves, then shuts it down.
func startsServing(t *testing.T, path string) {
	t.Helper()
	served := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, path, slog.New(servingHandler{served})) }()
	select {
	case <-served:
	case err := <-done:
		t.Fatalf("run returned before serving: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("run did not start serving")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown err=%v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
}

func TestRunStartsRawRoutesOnLuxWithoutSizes(t *testing.T) {
	startsServing(t, writeConfig(t, fakeLuxSchemas(t, "v0.1.23", ""), closedGitHub))
}

func TestRunStartsSizeRouteOnLuxWithSizes(t *testing.T) {
	startsServing(t, writeConfig(t, fakeLux(t, "v0.1.24"), closedGitHub, withSizeRoute))
}

func awaitPass(t *testing.T, passes <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-passes:
	case <-time.After(15 * time.Second):
		t.Fatalf("no pass %s", what)
	}
}

type endpointStatus struct {
	Time  time.Time `json:"time"`
	Error string    `json:"error"`
}

func getStatus(t *testing.T, client *http.Client, base string) endpointStatus {
	t.Helper()
	resp, err := client.Get(base + "/status")
	if err != nil {
		t.Fatal(err)
	}
	var st endpointStatus
	err = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status endpoint: %d %v", resp.StatusCode, err)
	}
	return st
}

func TestRunReconcilesAtStartupAndOnMatchingQueuedJobs(t *testing.T) {
	githubURL, passes := fakeGitHub(t)
	path := writeConfig(t, fakeLux(t, "v0.1.14"), githubURL)
	served := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, path, slog.New(servingHandler{served})) }()
	var base string
	select {
	case addr := <-served:
		base = "http://" + addr
	case err := <-done:
		t.Fatalf("run returned before serving: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("run did not start serving")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	awaitPass(t, passes, "at startup")
	started := time.Now()

	// The status endpoint publishes the pass once it ends.
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := getStatus(t, client, base)
		if !st.Time.IsZero() {
			if st.Error != "" {
				t.Fatalf("status after a pass: %+v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("status never showed a pass")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Deliveries that cannot create demand do not wake the reconciler. A
	// woken pass would follow within the 5s minimum gap; the hourly tick
	// (at 60s) would not.
	completed := workflowJob("completed", `["self-hosted","lux"]`)
	unmatched := workflowJob("queued", `["ubuntu-latest"]`)
	for _, body := range [][]byte{completed, unmatched} {
		postSigned(t, client, base, body, http.StatusAccepted)
	}
	select {
	case <-passes:
		t.Fatal("a pass followed a delivery that cannot create demand")
	case <-time.After(time.Until(started.Add(6500 * time.Millisecond))):
	}
	postSigned(t, client, base, workflowJob("queued", `["self-hosted","lux"]`), http.StatusAccepted)
	awaitPass(t, passes, "after a queued matching job")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown err=%v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
}

// Without a repository list the installation's repositories are listed at
// startup, and an installation_repositories delivery leads to a new listing.
func TestRunDiscoversRepositoriesAndRefreshesOnInstallationChanges(t *testing.T) {
	githubURL, passes, listings := fakeInstallation(t)
	path := writeConfig(t, fakeLux(t, "v0.1.14"), githubURL, func(c map[string]any) { delete(c, "repositories") })
	served := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, path, slog.New(servingHandler{served})) }()
	var base string
	select {
	case addr := <-served:
		base = "http://" + addr
	case err := <-done:
		t.Fatalf("run returned before serving: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("run did not start serving")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	awaitPass(t, listings, "listing at startup")
	awaitPass(t, passes, "reading the listed repository")

	body := []byte(`{"action":"added","installation":{"id":42},"repositories_added":[{"id":1002,"full_name":"acme/gadgets"}]}`)
	m := hmac.New(sha256.New, []byte(webhookSecret))
	m.Write(body)
	req, _ := http.NewRequest(http.MethodPost, base+"/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "installation_repositories")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("installation_repositories status %d", resp.StatusCode)
	}
	// The refresh comes with the woken pass, within the 5 s minimum gap.
	awaitPass(t, listings, "after installation_repositories")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown err=%v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
}

func workflowJob(action, labels string) []byte {
	return []byte(`{"action":"` + action + `","installation":{"id":42},"repository":{"full_name":"acme/widgets"},
		"workflow_job":{"id":7,"run_id":5,"run_attempt":1,"status":"queued","labels":` + labels + `,"runner_id":null,"runner_name":null}}`)
}

func postSigned(t *testing.T, client *http.Client, base string, body []byte, want int) {
	t.Helper()
	m := hmac.New(sha256.New, []byte(webhookSecret))
	m.Write(body)
	req, _ := http.NewRequest(http.MethodPost, base+"/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-GitHub-Delivery", "d-1")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("webhook status %d, want %d", resp.StatusCode, want)
	}
}

func TestRunServesHealthAndSignedWebhookThenShutsDown(t *testing.T) {
	path := writeConfig(t, fakeLux(t, "v0.1.14"), closedGitHub)
	served := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, path, slog.New(servingHandler{served})) }()

	var base string
	select {
	case addr := <-served:
		base = "http://" + addr
	case err := <-done:
		t.Fatalf("run returned before serving: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("run did not start serving")
	}
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status %d", resp.StatusCode)
	}

	job := []byte(`{"action":"queued","installation":{"id":42},"repository":{"full_name":"acme/widgets"},
		"workflow_job":{"id":7,"run_id":5,"run_attempt":1,"status":"queued","labels":["self-hosted","lux"],"runner_id":null,"runner_name":null}}`)
	for _, tc := range []struct {
		event string
		body  []byte
		sig   string
		want  int
	}{
		{"ping", []byte(`{"zen":"z","hook_id":1}`), "", http.StatusNoContent},
		{"workflow_job", job, "", http.StatusAccepted},
		{"workflow_job", job, "sha256=" + hex.EncodeToString(make([]byte, sha256.Size)), http.StatusUnauthorized},
	} {
		sig := tc.sig
		if sig == "" {
			m := hmac.New(sha256.New, []byte(webhookSecret))
			m.Write(tc.body)
			sig = "sha256=" + hex.EncodeToString(m.Sum(nil))
		}
		req, _ := http.NewRequest(http.MethodPost, base+"/webhook", bytes.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GitHub-Event", tc.event)
		req.Header.Set("X-GitHub-Delivery", "d-1")
		req.Header.Set("X-Hub-Signature-256", sig)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: status %d, want %d", tc.event, resp.StatusCode, tc.want)
		}
	}

	getStatus(t, client, base)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown err=%v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
	if _, err := client.Get(base + "/healthz"); err == nil {
		t.Fatal("still serving after shutdown")
	}
}

// listeners records the address of every "serving*" record by message.
type listeners struct{ addrs chan [2]string }

func (h listeners) Enabled(context.Context, slog.Level) bool { return true }
func (h listeners) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h listeners) WithGroup(string) slog.Handler            { return h }
func (h listeners) Handle(_ context.Context, r slog.Record) error {
	if !strings.HasPrefix(r.Message, "serving") {
		return nil
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "listen" {
			h.addrs <- [2]string{r.Message, a.Value.String()}
		}
		return true
	})
	return nil
}

// runUntilServing starts run and returns the addresses it logged by message
// once the main listener serves, and a stop function.
func runUntilServing(t *testing.T, path string) (map[string]string, func()) {
	t.Helper()
	// No AWS profile of the host's user is read; nothing calls AWS.
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	h := listeners{addrs: make(chan [2]string, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, path, slog.New(h)) }()
	got := map[string]string{}
	for got["serving"] == "" {
		select {
		case a := <-h.addrs:
			got[a[0]] = a[1]
		case err := <-done:
			t.Fatalf("run returned before serving: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("run did not start serving")
		}
	}
	return got, func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("shutdown err=%v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("run did not return after cancellation")
		}
	}
}

func withRegistry(dir string) func(map[string]any) {
	return func(c map[string]any) {
		c["registry"] = map[string]any{
			"roleArn": "arn:aws:iam::123456789012:role/lux-gha-registry", "region": "eu-west-1",
			"host": "123456789012.dkr.ecr.eu-west-1.amazonaws.com", "runnerListen": "127.0.0.1:0",
			"runnerEndpoint": "https://gha.example.com", "identityKeyFile": filepath.Join(dir, "identity-key"),
		}
	}
}

// With a registry block the runner listener is a server of its own that
// serves only the runner routes; the main listener does not serve them.
func TestRunServesTheRunnerListenerOnlyWithARegistry(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "identity-key"), []byte(strings.Repeat("k", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	addrs, stop := runUntilServing(t, writeConfig(t, fakeLux(t, "v0.1.14"), closedGitHub, withRegistry(dir)))
	defer stop()
	runner := addrs["serving runner listener"]
	if runner == "" || runner == addrs["serving"] {
		t.Fatalf("listeners %v", addrs)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for _, tc := range []struct {
		method, base, path string
		want               int
	}{
		{"POST", runner, "/runner/v1/registry", http.StatusUnauthorized},
		{"POST", runner, "/runner/v1/aws-credentials", http.StatusUnauthorized},
		{"GET", runner, "/healthz", http.StatusNotFound},
		{"GET", runner, "/status", http.StatusNotFound},
		{"POST", runner, "/webhook", http.StatusNotFound},
		{"POST", addrs["serving"], "/runner/v1/registry", http.StatusNotFound},
		{"GET", addrs["serving"], "/healthz", http.StatusOK},
	} {
		req, _ := http.NewRequest(tc.method, "http://"+tc.base+tc.path, nil)
		req.Header.Set("Authorization", "Bearer v1.lux-gha-test-1001-7-0.AAAA")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s%s: %d, want %d", tc.method, tc.base, tc.path, resp.StatusCode, tc.want)
		}
	}
}

func TestRunWithoutRegistryHasNoRunnerListener(t *testing.T) {
	addrs, stop := runUntilServing(t, writeConfig(t, fakeLux(t, "v0.1.14"), closedGitHub))
	stop()
	if len(addrs) != 1 {
		t.Fatalf("listeners %v", addrs)
	}
}

func TestRunRefusesARegistryWithoutItsIdentityKey(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, fakeLux(t, "v0.1.14"), closedGitHub, withRegistry(dir))
	err := run(context.Background(), path, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "registry.identityKeyFile") {
		t.Fatalf("err=%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "identity-key"), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), path, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "at least 32 bytes") {
		t.Fatalf("short key err=%v", err)
	}
}

// Only a loopback runnerListen starts without the plain-HTTP warning.
func TestExposedRunnerListenIsWarnedAbout(t *testing.T) {
	for listen, warned := range map[string]bool{
		"127.0.0.1:8081": false, "[::1]:8081": false, "localhost:8081": false, "127.0.0.2:0": false,
		"10.0.0.5:8081": true, "0.0.0.0:8081": true, ":8081": true, "[::]:8081": true, "bridge.example:8081": true,
	} {
		var buf strings.Builder
		warnExposedRunnerListen(&config.Registry{RunnerListen: listen}, slog.New(slog.NewTextHandler(&buf, nil)))
		if got := strings.Contains(buf.String(), "level=WARN") && strings.Contains(buf.String(), "must not be exposed directly"); got != warned {
			t.Errorf("%s: warned=%v, log %q", listen, got, buf.String())
		}
	}
}
