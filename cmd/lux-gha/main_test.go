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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcioapm/lux-gha/luxclient"
)

const webhookSecret = "cmd-test-webhook-secret"

// fakeLux serves GET /openapi.json, reporting version, and an empty
// GET /v1/runs for the reconciler; any other request, such as a Run
// submission, fails the test.
func fakeLux(t *testing.T, version string) string {
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
			"components":{"schemas":{"RunSpec":{"type":"object","properties":{"resumePolicy":{"type":"string"}}}}}}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// writeConfig writes a valid configuration and its secret files to a temp
// directory. GitHub's API URL points at a closed loopback port, so every
// reconciler pass fails at GitHub and provisions nothing.
func writeConfig(t *testing.T, luxURL string) string {
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
			"apiURL":         "http://127.0.0.1:1",
			"privateKeyFile": filepath.Join(dir, "app.pem"), "webhookSecretFile": filepath.Join(dir, "webhook-secret"),
		},
		"lux":          map[string]any{"url": luxURL, "apiKeyFile": filepath.Join(dir, "lux-api-key")},
		"repositories": []string{"acme/widgets"},
		"routes": []map[string]any{{
			"name": "default", "labels": []string{"self-hosted", "linux", "lux"}, "pool": "gha",
			"image": "ghcr.io/acme/runner@sha256:abc", "timeout": "90m",
		}},
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
			path := writeConfig(t, fakeLux(t, version))
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

func TestRunServesHealthAndSignedWebhookThenShutsDown(t *testing.T) {
	path := writeConfig(t, fakeLux(t, "v0.1.14"))
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

	resp, err = client.Get(base + "/status")
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Time  time.Time `json:"time"`
		Error string    `json:"error"`
	}
	err = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status endpoint: %d %v", resp.StatusCode, err)
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
	if _, err := client.Get(base + "/healthz"); err == nil {
		t.Fatal("still serving after shutdown")
	}
}
