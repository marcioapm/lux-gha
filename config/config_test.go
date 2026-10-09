package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `{
  "listen": "127.0.0.1:8080",
  "github": {"appID": "Iv1.abc", "installationID": 42, "runnerGroupID": 1,
             "privateKeyFile": "/k", "webhookSecretFile": "/w"},
  "lux": {"url": "https://lux.example.com", "apiKeyFile": "/l"},
  "repositories": ["acme/widgets"],
  "routes": [
    {"name": "default", "labels": ["self-hosted", "linux", "lux"], "pool": "gha",
     "image": "ghcr.io/acme/runner@sha256:abc", "cpus": 2, "memoryMiB": 4096, "diskGiB": 20,
     "timeout": "90m", "egressHosts": ["github.com", "*.actions.githubusercontent.com"]},
    {"name": "docker", "labels": ["self-hosted", "linux", "lux-docker"], "pool": "gha",
     "image": "ghcr.io/acme/runner-docker@sha256:def", "timeout": "2h", "nestedContainers": true}
  ]
}`

func mutate(old, new string) []byte { return []byte(strings.Replace(validConfig, old, new, 1)) }

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Routes[0].Timeout.Minutes() != 90 || !c.Routes[1].NestedContainers || c.GitHub.RunnerGroupID != 1 {
		t.Fatalf("unexpected parse: %+v", c)
	}
}

func TestParseRejectsInvalidInputs(t *testing.T) {
	cases := map[string][]byte{
		"empty":             []byte(`{}`),
		"unknown field":     mutate(`"listen"`, `"secret": "x", "listen"`),
		"bad duration":      mutate(`"90m"`, `"ninety"`),
		"zero timeout":      mutate(`"90m"`, `"0s"`),
		"http lux":          mutate(`https://lux.example.com`, `http://lux.example.com`),
		"lux url creds":     mutate(`https://lux.example.com`, `https://u:p@lux.example.com`),
		"bad repo":          mutate(`"acme/widgets"`, `"acme"`),
		"dotdot repo":       mutate(`"acme/widgets"`, `"acme/.."`),
		"duplicate repo":    mutate(`"acme/widgets"`, `"acme/widgets", "ACME/Widgets"`),
		"no installation":   mutate(`"installationID": 42`, `"installationID": 0`),
		"no runner group":   mutate(`"runnerGroupID": 1`, `"runnerGroupID": 0`),
		"duplicate name":    mutate(`"name": "docker"`, `"name": "default"`),
		"identical labels":  mutate(`"lux-docker"`, `"lux"`),
		"subset labels":     mutate(`"self-hosted", "linux", "lux-docker"`, `"self-hosted", "linux"`),
		"duplicate label":   mutate(`"lux-docker"`, `"LINUX"`),
		"no pool":           mutate(`"lux-docker"], "pool": "gha"`, `"lux-docker"], "pool": ""`),
		"trailing data":     []byte(validConfig + `{}`),
		"negative resource": mutate(`"cpus": 2`, `"cpus": -1`),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(input); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestMatchRouteRequiresAllJobLabels(t *testing.T) {
	c, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.MatchRoute([]string{"self-hosted", "LUX"})
	if err != nil || r.Name != "default" {
		t.Fatalf("got %v, %v", r, err)
	}
	// "lux" alone is a route label, but "gpu" is not: the job must not be served.
	if _, err := c.MatchRoute([]string{"lux", "gpu"}); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("expected ErrNoRoute, got %v", err)
	}
	// Both routes contain these labels; no single route can be chosen.
	if _, err := c.MatchRoute([]string{"self-hosted", "linux"}); !errors.Is(err, ErrAmbiguousRoute) {
		t.Fatalf("expected ErrAmbiguousRoute, got %v", err)
	}
	if _, err := c.MatchRoute(nil); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("expected ErrNoRoute for no labels, got %v", err)
	}
}

func TestAllowsRepositoryIsCaseInsensitive(t *testing.T) {
	c, _ := Parse([]byte(validConfig))
	if !c.AllowsRepository("Acme/Widgets") || c.AllowsRepository("acme/other") {
		t.Fatal("repository allow-list mismatch")
	}
}

func TestLoadSecretsTrimsNewlineAndRedacts(t *testing.T) {
	dir := t.TempDir()
	write := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c, _ := Parse([]byte(validConfig))
	c.GitHub.PrivateKeyFile = write("key", "PEM-DATA\n")
	c.GitHub.WebhookSecretFile = write("hook", "hook-secret\n")
	c.Lux.APIKeyFile = write("lux", "lux-key-value\n")
	s, err := c.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if string(s.WebhookSecret) != "hook-secret" || s.LuxAPIKey != "lux-key-value" {
		t.Fatalf("secrets not trimmed: %q", s.WebhookSecret)
	}
	for _, rendered := range []string{
		s.String(),
		fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s),
	} {
		if strings.Contains(rendered, "lux-key-value") || strings.Contains(rendered, "hook-secret") {
			t.Fatalf("secret leaked in %q", rendered)
		}
	}

	c.Lux.APIKeyFile = write("empty", "\n")
	if _, err := c.LoadSecrets(); err == nil || !strings.Contains(err.Error(), "lux.apiKeyFile") {
		t.Fatalf("expected empty-file error naming field, got %v", err)
	}
}
