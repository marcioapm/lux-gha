package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const validConfig = `{
  "listen": "127.0.0.1:8080",
  "instance": "prod-1",
  "maxRunners": 10,
  "github": {"appID": "Iv1.abc", "installationID": 42, "runnerGroupID": 1,
             "privateKeyFile": "/k", "webhookSecretFile": "/w"},
  "lux": {"url": "https://lux.example.com", "apiKeyFile": "/l"},
  "repositories": ["acme/widgets"],
  "routes": [
    {"name": "default", "labels": ["self-hosted", "linux", "lux"], "pool": "gha",
     "image": "ghcr.io/acme/runner@sha256:abc", "cpus": 2, "memoryMiB": 4096, "diskGiB": 20,
     "timeout": "90m", "egressHosts": ["github.com", "*.actions.githubusercontent.com"]},
    {"name": "docker", "labels": ["self-hosted", "linux", "lux-docker"], "pool": "gha",
     "image": "ghcr.io/acme/runner-docker@sha256:def", "timeout": "2h", "nestedContainers": true,
     "maxRunners": 3, "command": ["/opt/runner/start"]}
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
	if got := c.Reconcile; got.Interval.Duration != DefaultInterval || got.IdleGrace.Duration != DefaultIdleGrace ||
		got.MaxJobsPerPass != DefaultMaxJobsPerPass || got.MaxGensPerJob != DefaultMaxGensPerJob {
		t.Fatalf("reconcile defaults not applied: %+v", got)
	}
	if !slices.Equal(c.Routes[0].Command, DefaultCommand) || !slices.Equal(c.Routes[1].Command, []string{"/opt/runner/start"}) || c.Routes[1].MaxRunners != 3 {
		t.Fatalf("route command/maxRunners: %+v", c.Routes)
	}
	tuned, err := Parse(mutate(`"maxRunners": 10,`, `"maxRunners": 10, "reconcile": {"interval": "15s", "idleGrace": "5m", "maxJobsPerPass": 50, "maxGensPerJob": 2},`))
	if err != nil || tuned.Reconcile.Interval.Duration != 15*time.Second || tuned.Reconcile.IdleGrace.Duration != 5*time.Minute ||
		tuned.Reconcile.MaxJobsPerPass != 50 || tuned.Reconcile.MaxGensPerJob != 2 {
		t.Fatalf("reconcile overrides: %+v err=%v", tuned, err)
	}
	if _, err := Parse([]byte(validConfig + "\n\t \n")); err != nil {
		t.Fatalf("trailing whitespace rejected: %v", err)
	}
}

func TestParseRejectsInvalidInputs(t *testing.T) {
	cases := map[string][]byte{
		"empty":              []byte(`{}`),
		"unknown field":      mutate(`"listen"`, `"secret": "x", "listen"`),
		"bad duration":       mutate(`"90m"`, `"ninety"`),
		"zero timeout":       mutate(`"90m"`, `"0s"`),
		"http lux":           mutate(`https://lux.example.com`, `http://lux.example.com`),
		"lux url creds":      mutate(`https://lux.example.com`, `https://u:p@lux.example.com`),
		"bad repo":           mutate(`"acme/widgets"`, `"acme"`),
		"dotdot repo":        mutate(`"acme/widgets"`, `"acme/.."`),
		"duplicate repo":     mutate(`"acme/widgets"`, `"acme/widgets", "ACME/Widgets"`),
		"no installation":    mutate(`"installationID": 42`, `"installationID": 0`),
		"no runner group":    mutate(`"runnerGroupID": 1`, `"runnerGroupID": 0`),
		"duplicate name":     mutate(`"name": "docker"`, `"name": "default"`),
		"identical labels":   mutate(`"lux-docker"`, `"lux"`),
		"subset labels":      mutate(`"self-hosted", "linux", "lux-docker"`, `"self-hosted", "linux"`),
		"duplicate label":    mutate(`"lux-docker"`, `"LINUX"`),
		"no pool":            mutate(`"lux-docker"], "pool": "gha"`, `"lux-docker"], "pool": ""`),
		"trailing data":      []byte(validConfig + `{}`),
		"trailing brace":     []byte(validConfig + "\n}"),
		"trailing bracket":   []byte(validConfig + "]"),
		"trailing garbage":   []byte(validConfig + " x"),
		"negative resource":  mutate(`"cpus": 2`, `"cpus": -1`),
		"no instance":        mutate(`"instance": "prod-1",`, ``),
		"uppercase instance": mutate(`"prod-1"`, `"Prod"`),
		"long instance":      mutate(`"prod-1"`, `"abcdefghijklmnopqrstu"`),
		"trailing dash":      mutate(`"prod-1"`, `"prod-"`),
		"no maxRunners":      mutate(`"maxRunners": 10,`, ``),
		"negative route cap": mutate(`"maxRunners": 3`, `"maxRunners": -1`),
		"empty command":      mutate(`["/opt/runner/start"]`, `[""]`),
		"negative interval":  mutate(`"maxRunners": 10,`, `"maxRunners": 10, "reconcile": {"interval": "-1s"},`),
		"negative gens":      mutate(`"maxRunners": 10,`, `"maxRunners": 10, "reconcile": {"maxGensPerJob": -1},`),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(input); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// sizeRoute is a route on a shaped pool selected by its size label.
const sizeRoute = `{"name": "m-2", "labels": ["lux-m-2-arm64"], "pool": "shaped",
     "image": "ghcr.io/acme/runner@sha256:abc", "size": "m-2-arm64", "timeout": "1h"},
    {"name": "docker"`

func TestParseSizeRoute(t *testing.T) {
	c, err := Parse(mutate(`{"name": "docker"`, sizeRoute))
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.MatchRoute([]string{"LUX-M-2-ARM64"})
	if err != nil || r.Name != "m-2" || r.Size != "m-2-arm64" || r.CPUs != 0 || r.MemoryMiB != 0 || r.DiskGiB != 0 {
		t.Fatalf("route %+v err=%v", r, err)
	}
	// Raw routes are unchanged and carry no size.
	if c.Routes[0].Size != "" || c.Routes[0].CPUs != 2 {
		t.Fatalf("raw route %+v", c.Routes[0])
	}
}

func TestParseRejectsSizeWithRawResources(t *testing.T) {
	for _, raw := range []string{`"cpus": 2`, `"memoryMiB": 4096`, `"diskGiB": 20`} {
		_, err := Parse(mutate(`{"name": "docker"`, strings.Replace(sizeRoute, `"size": "m-2-arm64"`, `"size": "m-2-arm64", `+raw, 1)))
		want := `route "m-2": size and cpus, memoryMiB or diskGiB are mutually exclusive`
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", raw, err, want)
		}
	}
}

func TestParseRejectsBadSizeName(t *testing.T) {
	for _, name := range []string{"M-2-arm64", "m-2", "lux-m-2", "m-3-arm64", "m-0.5-arm64", "m-2-x86_64", strings.Repeat("m", 56) + "-2-arm64"} {
		_, err := Parse(mutate(`{"name": "docker"`, strings.Replace(sizeRoute, `"m-2-arm64"`, `"`+name+`"`, 1)))
		want := `route "m-2": size "` + name + `" is not a Lux size name`
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", name, err, want)
		}
	}
}

func TestParseSizeRoutesAreCheckedForAmbiguity(t *testing.T) {
	two := strings.Replace(sizeRoute, `{"name": "docker"`, `{"name": "m-4", "labels": ["lux-m-2-arm64", "big"], "pool": "shaped",
     "image": "ghcr.io/acme/runner@sha256:abc", "size": "m-4-arm64", "timeout": "1h"},
    {"name": "docker"`, 1)
	_, err := Parse(mutate(`{"name": "docker"`, two))
	want := `routes "m-2" and "m-4" are ambiguous`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestParseRejectsUnrestrictedEgressWithHosts(t *testing.T) {
	_, err := Parse(mutate(`"egressHosts": [`, `"unrestrictedEgress": true, "egressHosts": [`))
	want := `route "default": unrestrictedEgress and egressHosts are mutually exclusive`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want it to contain %q", err, want)
	}
}

func TestParseEgressModes(t *testing.T) {
	c, err := Parse(mutate(`"nestedContainers": true,`, `"nestedContainers": true, "unrestrictedEgress": true,`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Routes[0].UnrestrictedEgress || len(c.Routes[0].EgressHosts) != 2 || !c.Routes[1].UnrestrictedEgress || len(c.Routes[1].EgressHosts) != 0 {
		t.Fatalf("routes %+v", c.Routes)
	}
	// Neither field: valid, and the route's Runs get no network.
	c, err = Parse([]byte(validConfig))
	if err != nil || c.Routes[1].UnrestrictedEgress || len(c.Routes[1].EgressHosts) != 0 {
		t.Fatalf("route without egress: %+v err=%v", c.Routes[1], err)
	}
	if _, err := Parse(mutate(`"egressHosts": [`, `"unrestrictedEgress": false, "egressHosts": [`)); err != nil {
		t.Fatalf("unrestrictedEgress false with hosts rejected: %v", err)
	}
	// An empty host list is no hosts: compatible with unrestricted.
	c, err = Parse(mutate(`"nestedContainers": true,`, `"nestedContainers": true, "unrestrictedEgress": true, "egressHosts": [],`))
	if err != nil || !c.Routes[1].UnrestrictedEgress || len(c.Routes[1].EgressHosts) != 0 {
		t.Fatalf("unrestrictedEgress with empty egressHosts: %+v err=%v", c.Routes[1], err)
	}
}

func TestRouteCacheURL(t *testing.T) {
	parse := func(raw string) (*Config, error) {
		return Parse(mutate(`"nestedContainers": true,`, `"nestedContainers": true, "cacheURL": "`+raw+`",`))
	}
	valid := map[string]string{
		"https://cache.example.com":       "https://cache.example.com",
		"https://cache.example.com/":      "https://cache.example.com",
		"https://cache.example.com:8443/": "https://cache.example.com:8443",
		"http://127.0.0.1:8090":           "http://127.0.0.1:8090",
		"http://localhost:8090/":          "http://localhost:8090",
		"http://[::1]:8090":               "http://[::1]:8090",
	}
	for raw, want := range valid {
		c, err := parse(raw)
		if err != nil {
			t.Errorf("%q: %v", raw, err)
			continue
		}
		if c.Routes[1].CacheURL != want || c.Routes[0].CacheURL != "" {
			t.Errorf("%q: cacheURL %q, want %q", raw, c.Routes[1].CacheURL, want)
		}
	}
	for _, raw := range []string{
		"http://cache.example.com", "http://10.0.0.5:8090", "ftp://cache.example.com",
		"https://u:p@cache.example.com", "https://u@cache.example.com",
		"https://cache.example.com?a=1", "https://cache.example.com/?", "https://cache.example.com#f", "https://cache.example.com/#",
		"/cache", "cache.example.com", "https:///x",
	} {
		_, err := parse(raw)
		want := `route "docker": cacheURL`
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want it to contain %q", raw, err, want)
		}
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

func TestStopGraceAndStuckJobGrace(t *testing.T) {
	c, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Reconcile.StopGrace.Duration != time.Minute || c.Reconcile.StuckJobGrace.Duration != 2*time.Minute {
		t.Fatalf("defaults: %+v", c.Reconcile)
	}
	with := func(fields string) []byte {
		return mutate(`"maxRunners": 10,`, `"maxRunners": 10, "reconcile": {`+fields+`},`)
	}
	tuned, err := Parse(with(`"stopGrace": "15s", "stuckJobGrace": "1h"`))
	if err != nil || tuned.Reconcile.StopGrace.Duration != 15*time.Second || tuned.Reconcile.StuckJobGrace.Duration != time.Hour {
		t.Fatalf("bounds accepted: %+v err=%v", tuned, err)
	}
	tuned, err = Parse(with(`"stopGrace": "10m", "stuckJobGrace": "30s"`))
	if err != nil || tuned.Reconcile.StopGrace.Duration != 10*time.Minute || tuned.Reconcile.StuckJobGrace.Duration != 30*time.Second {
		t.Fatalf("bounds accepted: %+v err=%v", tuned, err)
	}
	for fields, want := range map[string]string{
		`"stopGrace": "14s"`:      "reconcile.stopGrace 14s must be between 15s and 10m0s",
		`"stopGrace": "11m"`:      "reconcile.stopGrace 11m0s must be between 15s and 10m0s",
		`"stopGrace": "-1m"`:      "reconcile.stopGrace -1m0s must be between",
		`"stuckJobGrace": "29s"`:  "reconcile.stuckJobGrace 29s must be between 30s and 1h0m0s",
		`"stuckJobGrace": "61m"`:  "reconcile.stuckJobGrace 1h1m0s must be between",
		`"stuckJobGrace": "-30s"`: "reconcile.stuckJobGrace -30s must be between",
	} {
		if _, err := Parse(with(fields)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", fields, err, want)
		}
	}
}

func TestHostLossReruns(t *testing.T) {
	c, err := Parse([]byte(validConfig))
	if err != nil || c.Reconcile.Reruns() != DefaultHostLossReruns || *c.Reconcile.HostLossReruns != 2 {
		t.Fatalf("default: %+v err=%v", c, err)
	}
	if (Reconcile{}).Reruns() != 2 {
		t.Fatal("unvalidated default")
	}
	with := func(n string) []byte {
		return mutate(`"maxRunners": 10,`, `"maxRunners": 10, "reconcile": {"hostLossReruns": `+n+`},`)
	}
	for _, n := range []int{0, 1, 5} {
		c, err := Parse(with(strconv.Itoa(n)))
		if err != nil || c.Reconcile.Reruns() != n {
			t.Errorf("%d: got %v err=%v", n, c, err)
		}
	}
	for n, want := range map[string]string{
		"-1":  "reconcile.hostLossReruns -1 must be between 0 and 5",
		"6":   "reconcile.hostLossReruns 6 must be between 0 and 5",
		`"2"`: "hostLossReruns",
		"1.5": "hostLossReruns",
	} {
		if _, err := Parse(with(n)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", n, err, want)
		}
	}
}

func TestRepositoriesAreOptional(t *testing.T) {
	explicit, err := Parse([]byte(validConfig))
	if err != nil || explicit.DiscoverRepositories() {
		t.Fatalf("explicit list: discover=%v err=%v", explicit.DiscoverRepositories(), err)
	}
	discovered, err := Parse(mutate(`"repositories": ["acme/widgets"],`, ``))
	if err != nil || !discovered.DiscoverRepositories() || discovered.AllowsRepository("acme/widgets") {
		t.Fatalf("absent list: discover=%v err=%v", discovered.DiscoverRepositories(), err)
	}
	rc := discovered.Reconcile
	if rc.RepositoryRefresh.Duration != DefaultRepositoryRefresh || rc.HotWindow.Duration != DefaultHotWindow || rc.ColdSweep.Duration != DefaultColdSweep {
		t.Fatalf("discovery defaults not applied: %+v", rc)
	}
	tuned, err := Parse(mutate(`"maxRunners": 10,`, `"maxRunners": 10, "reconcile": {"repositoryRefresh": "5m", "hotWindow": "30m", "coldSweep": "20m"},`))
	if err != nil || tuned.Reconcile.RepositoryRefresh.Duration != 5*time.Minute || tuned.Reconcile.HotWindow.Duration != 30*time.Minute ||
		tuned.Reconcile.ColdSweep.Duration != 20*time.Minute {
		t.Fatalf("discovery overrides: %+v err=%v", tuned, err)
	}
	for name, input := range map[string][]byte{
		"empty list":                 mutate(`["acme/widgets"]`, `[]`),
		"negative repositoryRefresh": mutate(`"maxRunners": 10,`, `"maxRunners": 10, "reconcile": {"repositoryRefresh": "-1m"},`),
		"negative hotWindow":         mutate(`"maxRunners": 10,`, `"maxRunners": 10, "reconcile": {"hotWindow": "-1m"},`),
		"negative coldSweep":         mutate(`"maxRunners": 10,`, `"maxRunners": 10, "reconcile": {"coldSweep": "-1m"},`),
	} {
		if _, err := Parse(input); err == nil {
			t.Fatalf("%s accepted", name)
		}
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
		for _, secret := range []string{"PEM-DATA", "lux-key-value", "hook-secret"} {
			if strings.Contains(rendered, secret) {
				t.Fatalf("secret %q leaked in formatted value", secret)
			}
		}
	}

	c.Lux.APIKeyFile = write("empty", "\n")
	if _, err := c.LoadSecrets(); err == nil || !strings.Contains(err.Error(), "lux.apiKeyFile") {
		t.Fatalf("expected empty-file error naming field, got %v", err)
	}
}

const registryBlock = `"registry": {"roleArn": "arn:aws:iam::123456789012:role/lux-gha-registry", "region": "eu-west-1",
    "host": "123456789012.dkr.ecr.eu-west-1.amazonaws.com", "runnerListen": "127.0.0.1:8081",
    "runnerEndpoint": "https://gha.example.com/", "identityKeyFile": "/i"},
  "repositories"`

func withRegistry(old, new string) []byte {
	return []byte(strings.Replace(strings.Replace(validConfig, `"repositories"`, registryBlock, 1), old, new, 1))
}

func TestRegistryIsOptionalAndDefaulted(t *testing.T) {
	c, err := Parse([]byte(validConfig))
	if err != nil || c.Registry != nil {
		t.Fatalf("absent block: %+v err=%v", c.Registry, err)
	}
	c, err = Parse(withRegistry("", ""))
	if err != nil {
		t.Fatal(err)
	}
	g := c.Registry
	if !slices.Equal(g.TrustedRefs, []string{"refs/heads/release/*", "refs/tags/v*"}) ||
		g.RunnerEndpoint != "https://gha.example.com" {
		t.Fatalf("defaults: %+v", g)
	}
	c, err = Parse(withRegistry(`"region"`, `"trustedRefs": ["refs/heads/main"], "region"`))
	if err != nil || !slices.Equal(c.Registry.TrustedRefs, []string{"refs/heads/main"}) {
		t.Fatalf("overrides: %+v err=%v", c.Registry, err)
	}
	contract := `"trustedRefs": ["refs/heads/release/*", "refs/tags/v*"], "region"`
	if c, err = Parse(withRegistry(`"region"`, contract)); err != nil || !slices.Equal(c.Registry.TrustedRefs, []string{"refs/heads/release/*", "refs/tags/v*"}) {
		t.Fatalf("contract list: %+v err=%v", c.Registry, err)
	}
	for _, loop := range []string{"http://127.0.0.1:8081", "http://localhost:8081", "http://[::1]:8081"} {
		if c, err = Parse(withRegistry(`"https://gha.example.com/"`, `"`+loop+`"`)); err != nil || c.Registry.RunnerEndpoint != loop {
			t.Fatalf("loopback http %s: %+v err=%v", loop, c.Registry, err)
		}
	}
}

func TestRegistryValidation(t *testing.T) {
	for name, tc := range map[string]struct{ old, new, want string }{
		"bad role":          {`role/lux-gha-registry`, `user/x`, "registry.roleArn"},
		"bad region":        {`"region": "eu-west-1"`, `"region": "Europe"`, "registry.region"},
		"non-AWS region":    {`"region": "eu-west-1"`, `"region": "auto"`, "registry.region"},
		"host other region": {`dkr.ecr.eu-west-1`, `dkr.ecr.us-east-1`, "registry.host"},
		"host with scheme":  {`"host": "`, `"host": "https://`, "registry.host"},
		"removed prefix":    {`"region"`, `"repositoryPrefix": "gha", "region"`, `unknown field "repositoryPrefix"`},
		"bad ref":           {`"region"`, `"trustedRefs": ["main"], "region"`, "registry.trustedRefs"},
		"sentinel removed":  {`"region"`, `"trustedRefs": ["<default branch>"], "region"`, "registry.trustedRefs"},
		"bad glob":          {`"region"`, `"trustedRefs": ["refs/heads/[x"], "region"`, "registry.trustedRefs"},
		"bad listen":        {`"127.0.0.1:8081",`, `"127.0.0.1",`, "registry.runnerListen"},
		"same listen":       {`"127.0.0.1:8081",`, `"127.0.0.1:8080",`, "registry.runnerListen must differ"},
		"endpoint path":     {`example.com/"`, `example.com/runner"`, "registry.runnerEndpoint"},
		"endpoint scheme":   {`"https://gha`, `"ftp://gha`, "registry.runnerEndpoint"},
		"plain http":        {`"https://gha`, `"http://gha`, "must be https unless its host is loopback"},
		"plain http to ip":  {`"https://gha.example.com/"`, `"http://192.0.2.10:8081"`, "must be https unless its host is loopback"},
		"no key file":       {`"identityKeyFile": "/i"`, `"identityKeyFile": ""`, "registry.identityKeyFile"},
		"unknown field":     {`"region"`, `"identityKey": "x", "region"`, "unknown field"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(withRegistry(tc.old, tc.new))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRegistryIdentityKeyIsLoadedOnlyWithTheBlock(t *testing.T) {
	dir := t.TempDir()
	write := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c, _ := Parse([]byte(validConfig))
	c.GitHub.PrivateKeyFile, c.GitHub.WebhookSecretFile, c.Lux.APIKeyFile = write("k", "k"), write("w", "w"), write("l", "l")
	s, err := c.LoadSecrets()
	if err != nil || s.RegistryIdentityKey != nil {
		t.Fatalf("without registry: %v err=%v", s.RegistryIdentityKey != nil, err)
	}
	r, err := Parse(withRegistry("", ""))
	if err != nil {
		t.Fatal(err)
	}
	r.GitHub, r.Lux = c.GitHub, c.Lux
	key := strings.Repeat("k", 32)
	r.Registry.IdentityKeyFile = write("id", key+"\n")
	s, err = r.LoadSecrets()
	if err != nil || string(s.RegistryIdentityKey) != key || strings.Contains(fmt.Sprintf("%+v %#v", s, s), key) {
		t.Fatalf("identity key: err=%v", err)
	}
	r.Registry.IdentityKeyFile = write("short", strings.Repeat("k", 31))
	if _, err := r.LoadSecrets(); err == nil || !strings.Contains(err.Error(), "at least 32 bytes") {
		t.Fatalf("short key: %v", err)
	}
	r.Registry.IdentityKeyFile = filepath.Join(dir, "absent")
	if _, err := r.LoadSecrets(); err == nil || !strings.Contains(err.Error(), "registry.identityKeyFile") {
		t.Fatalf("missing key: %v", err)
	}
}

// The cache and registry blocks parse side by side, each with its own
// defaults, and an error in each is reported under its own prefix.
func TestCacheAndRegistryTogether(t *testing.T) {
	both := strings.Replace(string(withRegistry("", "")), `"listen"`, cacheConfig, 1)
	c, err := Parse([]byte(both))
	if err != nil {
		t.Fatal(err)
	}
	if c.Cache == nil || c.Cache.TTL.Duration != 7*24*time.Hour || c.Cache.S3.Region != "eu-west-1" ||
		c.Registry == nil || c.Registry.RunnerEndpoint != "https://gha.example.com" ||
		!slices.Equal(c.Registry.TrustedRefs, DefaultTrustedRefs) {
		t.Fatalf("cache %+v registry %+v", c.Cache, c.Registry)
	}
	bad := strings.Replace(strings.Replace(both, `"bucket": "cache-bucket"`, `"bucket": "X"`, 1), `"region": "eu-west-1",`, `"region": "Europe",`, 1)
	_, err = Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "cache.s3.bucket") || !strings.Contains(err.Error(), "registry.region") {
		t.Fatalf("err = %v", err)
	}
}
