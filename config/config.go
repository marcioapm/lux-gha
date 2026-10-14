// Package config loads and validates the bridge configuration. Secrets are
// referenced by file path and are never part of the parsed Config value.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/marcioapm/lux-gha/luxclient"
)

type Config struct {
	Listen string `json:"listen"`
	// Instance names this bridge in Run labels and runner names.
	Instance   string    `json:"instance"`
	MaxRunners int       `json:"maxRunners"`
	Reconcile  Reconcile `json:"reconcile"`
	GitHub     GitHub    `json:"github"`
	Lux        Lux       `json:"lux"`
	// Repositories is nil when the field is absent: the repositories are
	// then discovered from the installation.
	Repositories []string `json:"repositories"`
	Routes       []Route  `json:"routes"`
}

// Reconcile tunes the reconciler; zero fields take the defaults below.
type Reconcile struct {
	Interval       Duration `json:"interval"`
	IdleGrace      Duration `json:"idleGrace"`
	MaxJobsPerPass int      `json:"maxJobsPerPass"`
	MaxGensPerJob  int      `json:"maxGensPerJob"`
	// The three below apply only when repositories are discovered.
	RepositoryRefresh Duration `json:"repositoryRefresh"`
	HotWindow         Duration `json:"hotWindow"`
	ColdSweep         Duration `json:"coldSweep"`
	// StopGrace is each runner Run's Lux workload.grace: the time between
	// SIGTERM and SIGKILL in which the runner cancels its job and uploads
	// its logs.
	StopGrace Duration `json:"stopGrace"`
	// StuckJobGrace is how long a job may stay in_progress after its
	// runner's Run ended before its workflow run is force-cancelled.
	StuckJobGrace Duration `json:"stuckJobGrace"`
	// HostLossReruns bounds the attempts of a workflow run that lux-gha
	// re-runs after a job lost its Run's host; 0 turns it off. A pointer,
	// since an absent field takes DefaultHostLossReruns; validate sets it.
	HostLossReruns *int `json:"hostLossReruns"`
}

// Reruns is HostLossReruns, or its default before validation.
func (rc Reconcile) Reruns() int {
	if rc.HostLossReruns == nil {
		return DefaultHostLossReruns
	}
	return *rc.HostLossReruns
}

const (
	DefaultInterval          = 60 * time.Second
	DefaultIdleGrace         = 2 * time.Minute
	DefaultMaxJobsPerPass    = 200
	DefaultMaxGensPerJob     = 5
	DefaultRepositoryRefresh = 10 * time.Minute
	DefaultHotWindow         = 2 * time.Minute
	DefaultColdSweep         = 10 * time.Minute
	DefaultStopGrace         = 60 * time.Second
	DefaultStuckJobGrace     = 2 * time.Minute
	DefaultHostLossReruns    = 2
	MaxHostLossReruns        = 5
)

// Bounds of StopGrace and StuckJobGrace. Below MinStopGrace the runner
// cannot cancel a step (it escalates SIGINT to SIGTERM after 7.5 s) and
// still upload logs; above MaxStopGrace every Lux stop, a surplus terminate
// included, holds its host that long.
const (
	MinStopGrace     = 15 * time.Second
	MaxStopGrace     = 10 * time.Minute
	MinStuckJobGrace = 30 * time.Second
	MaxStuckJobGrace = time.Hour
)

type GitHub struct {
	// AppID is the JWT issuer: the App's client ID (preferred by GitHub) or numeric app ID.
	AppID          string `json:"appID"`
	InstallationID int64  `json:"installationID"`
	// RunnerGroupID is required by generate-jitconfig; the repository default
	// group id must be read from GitHub by the operator, not assumed.
	RunnerGroupID     int64  `json:"runnerGroupID"`
	APIURL            string `json:"apiURL,omitempty"`
	PrivateKeyFile    string `json:"privateKeyFile"`
	WebhookSecretFile string `json:"webhookSecretFile"`
}

type Lux struct {
	URL        string `json:"url"`
	APIKeyFile string `json:"apiKeyFile"`
}

// Route maps a set of runner labels to a Lux Run shape. A job uses the route
// only when every label it requests is in Labels. Size names a shaped pool's
// size and excludes CPUs, MemoryMiB and DiskGiB.
type Route struct {
	Name             string   `json:"name"`
	Labels           []string `json:"labels"`
	Pool             string   `json:"pool"`
	Image            string   `json:"image"`
	Size             string   `json:"size"`
	CPUs             float64  `json:"cpus"`
	MemoryMiB        int64    `json:"memoryMiB"`
	DiskGiB          int64    `json:"diskGiB"`
	Timeout          Duration `json:"timeout"`
	EgressHosts      []string `json:"egressHosts"`
	NestedContainers bool     `json:"nestedContainers"`
	// MaxRunners caps this route's live Runs; zero leaves only the global cap.
	MaxRunners int `json:"maxRunners"`
	// Command overrides DefaultCommand, the runner image's entrypoint.
	Command []string `json:"command"`
	// UnrestrictedEgress turns Lux's egress filtering off for the route's
	// Runs. Without it, a Run reaches only EgressHosts, and nothing if empty.
	UnrestrictedEgress bool `json:"unrestrictedEgress"`
}

var DefaultCommand = []string{"/usr/local/bin/lux-gha-runner"}

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("duration must be a string such as \"90m\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	d.Duration = v
	return nil
}

// Secrets holds credential material read from the files named in Config.
type Secrets struct {
	AppPrivateKey []byte
	WebhookSecret []byte
	LuxAPIKey     string
}

func (Secrets) String() string   { return "config.Secrets{redacted}" }
func (Secrets) GoString() string { return "config.Secrets{redacted}" }

const maxConfigBytes = 1 << 20

var (
	repoPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}/[A-Za-z0-9._-]{1,100}$`)
	// Runner names are lux-gha-<instance>-<repo id>-<job id>-<gen>; 20
	// characters keeps them well inside GitHub's runner name limit.
	instancePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,18}[a-z0-9])?$`)
	namePattern     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	labelPattern    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

func Load(path string) (*Config, error) {
	b, err := readBounded(path, maxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	// More() is false before a closing delimiter, so require end of input.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("config: trailing data after JSON object")
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		add("listen: %q is not host:port", c.Listen)
	}
	if !instancePattern.MatchString(c.Instance) {
		add("instance %q must match %s", c.Instance, instancePattern)
	}
	if c.MaxRunners <= 0 {
		add("maxRunners must be positive")
	}
	rc := &c.Reconcile
	if rc.Interval.Duration < 0 || rc.IdleGrace.Duration < 0 || rc.MaxJobsPerPass < 0 || rc.MaxGensPerJob < 0 ||
		rc.RepositoryRefresh.Duration < 0 || rc.HotWindow.Duration < 0 || rc.ColdSweep.Duration < 0 {
		add("reconcile: values must not be negative")
	}
	if rc.RepositoryRefresh.Duration == 0 {
		rc.RepositoryRefresh.Duration = DefaultRepositoryRefresh
	}
	if rc.HotWindow.Duration == 0 {
		rc.HotWindow.Duration = DefaultHotWindow
	}
	if rc.ColdSweep.Duration == 0 {
		rc.ColdSweep.Duration = DefaultColdSweep
	}
	if rc.Interval.Duration == 0 {
		rc.Interval.Duration = DefaultInterval
	}
	if rc.IdleGrace.Duration == 0 {
		rc.IdleGrace.Duration = DefaultIdleGrace
	}
	if rc.MaxJobsPerPass == 0 {
		rc.MaxJobsPerPass = DefaultMaxJobsPerPass
	}
	if rc.MaxGensPerJob == 0 {
		rc.MaxGensPerJob = DefaultMaxGensPerJob
	}
	if rc.StopGrace.Duration == 0 {
		rc.StopGrace.Duration = DefaultStopGrace
	}
	if g := rc.StopGrace.Duration; g < MinStopGrace || g > MaxStopGrace {
		add("reconcile.stopGrace %s must be between %s and %s", g, MinStopGrace, MaxStopGrace)
	}
	if rc.StuckJobGrace.Duration == 0 {
		rc.StuckJobGrace.Duration = DefaultStuckJobGrace
	}
	if g := rc.StuckJobGrace.Duration; g < MinStuckJobGrace || g > MaxStuckJobGrace {
		add("reconcile.stuckJobGrace %s must be between %s and %s", g, MinStuckJobGrace, MaxStuckJobGrace)
	}
	if n := rc.Reruns(); n < 0 || n > MaxHostLossReruns {
		add("reconcile.hostLossReruns %d must be between 0 and %d", n, MaxHostLossReruns)
	}
	n := rc.Reruns()
	rc.HostLossReruns = &n
	g := c.GitHub
	if strings.TrimSpace(g.AppID) == "" {
		add("github.appID is required")
	}
	if g.InstallationID <= 0 {
		add("github.installationID must be positive")
	}
	if g.RunnerGroupID <= 0 {
		add("github.runnerGroupID must be positive")
	}
	if g.APIURL != "" {
		if err := checkURL(g.APIURL); err != nil {
			add("github.apiURL: %v", err)
		}
	}
	if g.PrivateKeyFile == "" || g.WebhookSecretFile == "" {
		add("github.privateKeyFile and github.webhookSecretFile are required")
	}
	if err := checkURL(c.Lux.URL); err != nil {
		add("lux.url: %v", err)
	}
	if c.Lux.APIKeyFile == "" {
		add("lux.apiKeyFile is required")
	}

	// An absent field means discovery; an empty list is more likely a
	// mistake than a request for every repository.
	if c.Repositories != nil && len(c.Repositories) == 0 {
		add("repositories: list at least one owner/name, or omit the field to serve every repository of the installation")
	}
	seenRepo := map[string]bool{}
	for _, r := range c.Repositories {
		if !repoPattern.MatchString(r) || strings.HasSuffix(r, "/.") || strings.HasSuffix(r, "/..") {
			add("repositories: %q is not owner/name", r)
			continue
		}
		key := strings.ToLower(r)
		if seenRepo[key] {
			add("repositories: duplicate %q", r)
		}
		seenRepo[key] = true
	}

	if len(c.Routes) == 0 {
		add("routes: at least one route is required")
	}
	seenName := map[string]bool{}
	for i := range c.Routes {
		r := &c.Routes[i]
		if !namePattern.MatchString(r.Name) {
			add("routes[%d].name %q must match %s", i, r.Name, namePattern)
		} else if seenName[r.Name] {
			add("routes: duplicate name %q", r.Name)
		}
		seenName[r.Name] = true
		if len(r.Labels) == 0 {
			add("route %q: labels are required", r.Name)
		}
		for j, l := range r.Labels {
			if !labelPattern.MatchString(l) {
				add("route %q: invalid label %q", r.Name, l)
			}
			// GitHub compares runner labels case-insensitively.
			r.Labels[j] = strings.ToLower(l)
		}
		slices.Sort(r.Labels)
		if len(slices.Compact(slices.Clone(r.Labels))) != len(r.Labels) {
			add("route %q: duplicate label", r.Name)
		}
		if r.Pool == "" || r.Image == "" {
			add("route %q: pool and image are required", r.Name)
		}
		if r.CPUs < 0 || r.MemoryMiB < 0 || r.DiskGiB < 0 {
			add("route %q: resources must not be negative", r.Name)
		}
		if r.Size != "" {
			if r.CPUs != 0 || r.MemoryMiB != 0 || r.DiskGiB != 0 {
				add("route %q: size and cpus, memoryMiB or diskGiB are mutually exclusive", r.Name)
			}
			if !luxclient.ValidSizeName(r.Size) {
				add("route %q: size %q is not a Lux size name (<family>-<cpus>-<arch>, e.g. m-2-arm64)", r.Name, r.Size)
			}
		}
		if r.Timeout.Duration <= 0 {
			add("route %q: timeout must be positive", r.Name)
		}
		if r.MaxRunners < 0 {
			add("route %q: maxRunners must not be negative", r.Name)
		}
		if len(r.Command) == 0 {
			r.Command = slices.Clone(DefaultCommand)
		} else if r.Command[0] == "" {
			add("route %q: command[0] must not be empty", r.Name)
		}
		for _, h := range r.EgressHosts {
			if h == "" || strings.ContainsAny(h, " /:") {
				add("route %q: invalid egress host %q", r.Name, h)
			}
		}
		if r.UnrestrictedEgress && len(r.EgressHosts) > 0 {
			add("route %q: unrestrictedEgress and egressHosts are mutually exclusive", r.Name)
		}
	}
	// A job requesting the smaller set would satisfy both routes.
	for i := range c.Routes {
		for j := i + 1; j < len(c.Routes); j++ {
			a, b := c.Routes[i], c.Routes[j]
			if subset(a.Labels, b.Labels) || subset(b.Labels, a.Labels) {
				add("routes %q and %q are ambiguous: one label set contains the other", a.Name, b.Name)
			}
		}
	}
	return errors.Join(errs...)
}

// DiscoverRepositories reports whether the repository set comes from the
// installation rather than from Repositories.
func (c *Config) DiscoverRepositories() bool { return c.Repositories == nil }

// AllowsRepository reports whether owner/name is configured (case-insensitive).
func (c *Config) AllowsRepository(fullName string) bool {
	for _, r := range c.Repositories {
		if strings.EqualFold(r, fullName) {
			return true
		}
	}
	return false
}

var (
	ErrNoRoute        = errors.New("no route supports all requested labels")
	ErrAmbiguousRoute = errors.New("more than one route supports all requested labels")
)

// MatchRoute returns the single route whose label set contains every
// requested label. Overlapping routes that both match fail closed.
func (c *Config) MatchRoute(jobLabels []string) (*Route, error) {
	if len(jobLabels) == 0 {
		return nil, ErrNoRoute
	}
	want := make([]string, len(jobLabels))
	for i, l := range jobLabels {
		want[i] = strings.ToLower(l)
	}
	var found *Route
	for i := range c.Routes {
		if !subset(want, c.Routes[i].Labels) {
			continue
		}
		if found != nil {
			return nil, ErrAmbiguousRoute
		}
		found = &c.Routes[i]
	}
	if found == nil {
		return nil, ErrNoRoute
	}
	return found, nil
}

// LoadSecrets reads the credential files. Errors name the config field, never
// file contents.
func (c *Config) LoadSecrets() (Secrets, error) {
	var s Secrets
	var err error
	if s.AppPrivateKey, err = readSecret("github.privateKeyFile", c.GitHub.PrivateKeyFile); err != nil {
		return Secrets{}, err
	}
	if s.WebhookSecret, err = readSecret("github.webhookSecretFile", c.GitHub.WebhookSecretFile); err != nil {
		return Secrets{}, err
	}
	key, err := readSecret("lux.apiKeyFile", c.Lux.APIKeyFile)
	if err != nil {
		return Secrets{}, err
	}
	s.LuxAPIKey = string(key)
	return s, nil
}

func readSecret(field, path string) ([]byte, error) {
	b, err := readBounded(path, 64<<10)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", field, err)
	}
	// Files written with echo/editors end in a newline that is not part of the secret.
	b = bytes.TrimRight(b, "\r\n")
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, fmt.Errorf("config: %s: file is empty", field)
	}
	return b, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, limit)
	}
	return b, nil
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q must be an absolute URL without credentials, query or fragment", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		if ip := net.ParseIP(u.Hostname()); (ip != nil && ip.IsLoopback()) || u.Hostname() == "localhost" {
			return nil
		}
	}
	return fmt.Errorf("%q must use https (http only for loopback)", raw)
}

func subset(small, big []string) bool {
	for _, s := range small {
		if !slices.Contains(big, s) {
			return false
		}
	}
	return true
}
