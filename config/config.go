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
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/marcioapm/lux-gha/cache"
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
	// Cache is nil when absent: the cache server is off.
	Cache *Cache `json:"cache"`
	// Registry is nil when the block is absent: no runner listener, no
	// identity secret and no registry env in Runs.
	Registry *Registry `json:"registry,omitempty"`
}

// Cache configures the GitHub Actions cache server, served on its own
// listener. Zero optional fields take the defaults below.
type Cache struct {
	Listen string `json:"listen"`
	// PublicURL is the base URL runners use as ACTIONS_RESULTS_URL.
	PublicURL          string   `json:"publicURL"`
	S3                 CacheS3  `json:"s3"`
	SigningKeyFile     string   `json:"signingKeyFile"`
	Issuer             string   `json:"issuer"`
	UpstreamResultsURL string   `json:"upstreamResultsURL"`
	DownloadURLTTL     Duration `json:"downloadURLTTL"`
	TTL                Duration `json:"ttl"`
	// MaxBytes caps entries (90 %) and uploads in progress (10 %)
	// together; 0 is no cap.
	MaxBytes      int64    `json:"maxBytes"`
	MaxEntryBytes int64    `json:"maxEntryBytes"`
	ReapInterval  Duration `json:"reapInterval"`
	// A runtime token is served only when its repository_id is in
	// RepositoryIDs or its repository owner ID is in RepositoryOwnerIDs;
	// at least one list is required.
	RepositoryOwnerIDs []int64 `json:"repositoryOwnerIDs"`
	RepositoryIDs      []int64 `json:"repositoryIDs"`
}

type CacheS3 struct {
	Bucket string `json:"bucket"`
	Region string `json:"region"`
	Prefix string `json:"prefix"`
	// Endpoint is an S3-compatible server, addressed path-style.
	Endpoint string `json:"endpoint"`
}

// The cache defaults are the cache package's own.
const (
	// A presigned S3 URL lives at most 7 days.
	MaxCacheDownloadURLTTL = 7 * 24 * time.Hour
	// S3's object size limit.
	MaxCacheEntryBytes = 5 << 40
)

var (
	bucketPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionPattern      = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	cachePrefixPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*$`)
)

// Registry configures the in-region image registry credentials that the
// runner listener hands to Runs.
type Registry struct {
	RoleARN         string   `json:"roleArn"`
	Region          string   `json:"region"`
	Host            string   `json:"host"`
	TrustedRefs     []string `json:"trustedRefs"`
	RunnerListen    string   `json:"runnerListen"`
	RunnerEndpoint  string   `json:"runnerEndpoint"`
	IdentityKeyFile string   `json:"identityKeyFile"`
}

// DefaultTrustedRefs apply when trustedRefs is absent. The repository's
// default branch is always trusted in addition, whatever the list holds.
var DefaultTrustedRefs = []string{"refs/heads/release/*", "refs/tags/v*"}

// MinIdentityKeyBytes is the shortest accepted registry identity key.
const MinIdentityKeyBytes = 32

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
	// RegistryIdentityKey is nil without a registry block.
	RegistryIdentityKey []byte
}

func (Secrets) String() string   { return "config.Secrets{redacted}" }
func (Secrets) GoString() string { return "config.Secrets{redacted}" }

const maxConfigBytes = 1 << 20

var (
	repoPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}/[A-Za-z0-9._-]{1,100}$`)
	// Runner names are lux-gha-<instance>-<repo id>-<job id>-<gen>; 20
	// characters keeps them well inside GitHub's runner name limit.
	instancePattern  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,18}[a-z0-9])?$`)
	namePattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	labelPattern     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	roleARNPattern   = regexp.MustCompile(`^arn:aws(-[a-z]+)*:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_/-]{1,512}$`)
	awsRegionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	// ecrHostPattern's second group is the region, compared to Region.
	ecrHostPattern = regexp.MustCompile(`^[0-9]{12}\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com(\.cn)?$`)
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
	n := rc.Reruns()
	if n < 0 || n > MaxHostLossReruns {
		add("reconcile.hostLossReruns %d must be between 0 and %d", n, MaxHostLossReruns)
	}
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
	if c.Cache != nil {
		c.Cache.validate(c.Listen, add)
	}
	if c.Registry != nil {
		errs = append(errs, c.Registry.validate(c.Listen)...)
	}
	return errors.Join(errs...)
}

func (cc *Cache) validate(webhookListen string, add func(string, ...any)) {
	if _, _, err := net.SplitHostPort(cc.Listen); err != nil {
		add("cache.listen: %q is not host:port", cc.Listen)
	} else if cc.Listen == webhookListen && !strings.HasSuffix(cc.Listen, ":0") {
		add("cache.listen must differ from listen")
	}
	if err := checkURL(cc.PublicURL); err != nil {
		add("cache.publicURL: %v", err)
	} else if u, _ := url.Parse(cc.PublicURL); strings.TrimSuffix(u.Path, "/") != "" {
		add("cache.publicURL %q must not have a path", cc.PublicURL)
	}
	s := &cc.S3
	if !bucketPattern.MatchString(s.Bucket) || strings.Contains(s.Bucket, "..") {
		add("cache.s3.bucket %q is not an S3 bucket name", s.Bucket)
	}
	if !regionPattern.MatchString(s.Region) {
		add("cache.s3.region %q is required, e.g. eu-west-1", s.Region)
	}
	if s.Prefix != "" {
		if !cachePrefixPattern.MatchString(s.Prefix) || len(s.Prefix) > 200 {
			add("cache.s3.prefix %q must be /-separated segments of A-Z, a-z, 0-9, '.', '_' and '-', none starting with '.', at most 200 characters", s.Prefix)
		}
	}
	if s.Endpoint != "" {
		if err := checkURL(s.Endpoint); err != nil {
			add("cache.s3.endpoint: %v", err)
		}
	}
	if cc.SigningKeyFile == "" {
		add("cache.signingKeyFile is required")
	}
	if cc.Issuer == "" {
		cc.Issuer = cache.DefaultIssuer
	}
	if err := checkURL(cc.Issuer); err != nil {
		add("cache.issuer: %v", err)
	}
	if cc.UpstreamResultsURL == "" {
		cc.UpstreamResultsURL = cache.DefaultUpstreamResultsURL
	}
	if err := checkURL(cc.UpstreamResultsURL); err != nil {
		add("cache.upstreamResultsURL: %v", err)
	}
	if cc.DownloadURLTTL.Duration == 0 {
		cc.DownloadURLTTL.Duration = cache.DefaultDownloadURLTTL
	}
	if d := cc.DownloadURLTTL.Duration; d < time.Minute || d > MaxCacheDownloadURLTTL {
		add("cache.downloadURLTTL %s must be between 1m and %s", d, MaxCacheDownloadURLTTL)
	}
	if cc.TTL.Duration == 0 {
		cc.TTL.Duration = cache.DefaultTTL
	}
	if cc.TTL.Duration < time.Hour {
		add("cache.ttl %s must be at least 1h", cc.TTL.Duration)
	}
	if cc.MaxBytes < 0 {
		add("cache.maxBytes must not be negative")
	}
	if cc.MaxEntryBytes == 0 && cc.MaxBytes >= 0 {
		cc.MaxEntryBytes = cache.DefaultMaxEntryBytesFor(cc.MaxBytes)
	}
	if cc.MaxEntryBytes <= 0 || cc.MaxEntryBytes > MaxCacheEntryBytes {
		add("cache.maxEntryBytes must be between 1 and %d (default %d, 10 GiB, or 90 %% of cache.maxBytes if smaller)",
			int64(MaxCacheEntryBytes), int64(cache.DefaultMaxEntryBytes))
	}
	// Entries are trimmed to the entry budget, 90 % of maxBytes; a larger
	// entry would be deleted by the first capped pass after it is saved.
	if cc.MaxBytes > 0 && cc.MaxEntryBytes > cache.MaxEntryBytesFor(cc.MaxBytes) {
		add("cache.maxEntryBytes %d must not exceed 90 %% of cache.maxBytes (%d); omit it for the default, 10 GiB or 90 %% of cache.maxBytes if smaller",
			cc.MaxEntryBytes, cache.MaxEntryBytesFor(cc.MaxBytes))
	}
	if cc.ReapInterval.Duration == 0 {
		cc.ReapInterval.Duration = cache.DefaultReapInterval
	}
	if cc.ReapInterval.Duration < time.Minute {
		add("cache.reapInterval %s must be at least 1m", cc.ReapInterval.Duration)
	}
	if len(cc.RepositoryOwnerIDs) == 0 && len(cc.RepositoryIDs) == 0 {
		add("cache: list the served repositories in cache.repositoryIDs or their owners in cache.repositoryOwnerIDs (numeric GitHub IDs)")
	}
	for _, id := range cc.RepositoryOwnerIDs {
		if id <= 0 {
			add("cache.repositoryOwnerIDs: %d is not a positive GitHub ID", id)
		}
	}
	for _, id := range cc.RepositoryIDs {
		if id <= 0 {
			add("cache.repositoryIDs: %d is not a positive GitHub ID", id)
		}
	}
}

// LoadSigningKeys reads cache.signingKeyFile: one key per line, each at
// least 32 bytes. The first signs new upload URLs; every key verifies, so
// a new key is rotated in by adding it as the first line and the old one
// removed once its URLs have expired.
func (cc *Cache) LoadSigningKeys() ([][]byte, error) {
	b, err := readSecret("cache.signingKeyFile", cc.SigningKeyFile)
	if err != nil {
		return nil, err
	}
	var keys [][]byte
	for i, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			continue
		}
		if len(line) < 32 {
			return nil, fmt.Errorf("config: cache.signingKeyFile: key on line %d is shorter than 32 bytes", i+1)
		}
		keys = append(keys, line)
	}
	return keys, nil
}

func (g *Registry) validate(listen string) []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf("registry."+format, a...)) }
	if !roleARNPattern.MatchString(g.RoleARN) {
		add("roleArn %q is not an IAM role ARN", g.RoleARN)
	}
	if !awsRegionPattern.MatchString(g.Region) {
		add("region %q is not an AWS region name", g.Region)
	}
	// docker-credential-ecr-login serves only ECR registry hosts, and the
	// credentials are minted for Region.
	if m := ecrHostPattern.FindStringSubmatch(g.Host); m == nil || m[1] != g.Region {
		add("host %q must be <account>.dkr.ecr.%s.amazonaws.com", g.Host, g.Region)
	}
	if g.TrustedRefs == nil {
		g.TrustedRefs = slices.Clone(DefaultTrustedRefs)
	}
	for _, p := range g.TrustedRefs {
		if _, err := path.Match(p, ""); err != nil ||
			(!strings.HasPrefix(p, "refs/heads/") && !strings.HasPrefix(p, "refs/tags/")) {
			add("trustedRefs: %q must be a refs/heads/ or refs/tags/ glob (the default branch is always trusted)", p)
		}
	}
	if _, port, err := net.SplitHostPort(g.RunnerListen); err != nil {
		add("runnerListen: %q is not host:port", g.RunnerListen)
	} else if g.RunnerListen == listen && port != "0" {
		add("runnerListen must differ from listen")
	}
	u, err := url.Parse(g.RunnerEndpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		add("runnerEndpoint %q must be an http(s) URL with no path, credentials, query or fragment", g.RunnerEndpoint)
	} else if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
		// Bearer tokens and AWS secret keys cross this hop.
		add("runnerEndpoint %q must be https unless its host is loopback", g.RunnerEndpoint)
	} else {
		g.RunnerEndpoint = strings.TrimSuffix(g.RunnerEndpoint, "/")
	}
	if g.IdentityKeyFile == "" {
		add("identityKeyFile is required")
	}
	return errs
}

// LoopbackListen reports whether runnerListen binds only a loopback
// address. An empty host binds every interface.
func (g *Registry) LoopbackListen() bool {
	host, _, err := net.SplitHostPort(g.RunnerListen)
	if err != nil {
		return false
	}
	return loopbackHost(host)
}

func loopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
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
	if c.Registry != nil {
		if s.RegistryIdentityKey, err = readSecret("registry.identityKeyFile", c.Registry.IdentityKeyFile); err != nil {
			return Secrets{}, err
		}
		if len(s.RegistryIdentityKey) < MinIdentityKeyBytes {
			return Secrets{}, fmt.Errorf("config: registry.identityKeyFile: key must be at least %d bytes", MinIdentityKeyBytes)
		}
	}
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
