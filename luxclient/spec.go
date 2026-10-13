package luxclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"strings"
	"time"
)

// ResumePolicyNever is the only policy this client submits: a Run holding a
// single-use JIT runner credential must never be resumed or restarted.
const ResumePolicyNever = "never"

// Run is the subset of Lux's Run representation the bridge relies on.
// A Run with resumePolicy never always ends "terminated"; its outcome is in
// StateReason and ExitCode, so terminated alone does not mean failure.
type Run struct {
	ID             string            `json:"id"`
	Labels         map[string]string `json:"labels"`
	State          string            `json:"state"`
	StateReason    string            `json:"stateReason,omitempty"`
	ExitCode       *int              `json:"exitCode,omitempty"`
	Spec           RunSpecEcho       `json:"spec"`
	CreatedAt      time.Time         `json:"createdAt"`
	FirstStartedAt *time.Time        `json:"firstStartedAt,omitempty"`
	FinishedAt     *time.Time        `json:"finishedAt,omitempty"`
}

type RunSpecEcho struct {
	ResumePolicy string `json:"resumePolicy,omitempty"`
}

// RunSpec is a one-shot generic Run. Lux decodes submissions strictly
// (unknown fields are a 400), so the wire form is built by MarshalJSON with
// exactly Lux's field names; workload.adapter and resumePolicy are fixed.
type RunSpec struct {
	Name      string
	Labels    map[string]string
	ImageRef  string
	Command   []string
	Workdir   string
	User      string
	Env       map[string]string
	Secrets   []FileSecret
	Resources Resources
	// Timeout is running time summed over placements; zero means no limit.
	Timeout time.Duration
	// Grace is workload.grace: how long a stop waits after SIGTERM before
	// SIGKILL. Zero takes Lux's default (30s); a preemption or a
	// force-evicting drain may shorten it to half the time left.
	Grace  time.Duration
	Pool   string
	Egress []EgressRule
	// UnrestrictedEgress sends network.unrestricted: Lux stops filtering the
	// Run's egress (its hard blocks still apply). Exclusive with Egress.
	UnrestrictedEgress bool
	Sandbox            Sandbox
}

// FileSecret is delivered by Lux on a tmpfs linked at Path ("as": "file").
// Lux holds Value in memory until placement and never stores it.
type FileSecret struct {
	Name  string
	Value string
	Path  string
}

// Memory and Disk are bytes. Zero fields take luxd's defaults.
type Resources struct {
	CPUs   float64 `json:"cpus,omitempty"`
	Memory int64   `json:"memory,omitempty"`
	Disk   int64   `json:"disk,omitempty"`
	Pids   int64   `json:"pids,omitempty"`
}

// EgressRule is exactly one of Host (a name, or "*.<domain>") or CIDR.
type EgressRule struct {
	Host string `json:"host,omitempty"`
	CIDR string `json:"cidr,omitempty"`
}

type Sandbox struct {
	NestedContainers bool `json:"nestedContainers,omitempty"`
	ReadOnlyRoot     bool `json:"readOnlyRoot,omitempty"`
}

type wireSpec struct {
	Name         string            `json:"name,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Image        wireImage         `json:"image"`
	Workload     wireWorkload      `json:"workload"`
	Env          map[string]string `json:"env,omitempty"`
	Secrets      []wireSecret      `json:"secrets,omitempty"`
	Resources    Resources         `json:"resources"`
	Timeout      string            `json:"timeout,omitempty"`
	Placement    wirePlacement     `json:"placement"`
	Network      wireNetwork       `json:"network"`
	Sandbox      Sandbox           `json:"sandbox"`
	ResumePolicy string            `json:"resumePolicy"`
}

type wireImage struct {
	Ref string `json:"ref"`
}

type wireWorkload struct {
	Adapter string   `json:"adapter"`
	Command []string `json:"command"`
	Workdir string   `json:"workdir,omitempty"`
	User    string   `json:"user,omitempty"`
	Grace   string   `json:"grace,omitempty"`
}

type wireSecret struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	As    string `json:"as"`
	Path  string `json:"path"`
}

type wirePlacement struct {
	Pool string `json:"pool,omitempty"`
}

type wireNetwork struct {
	Egress       []EgressRule `json:"egress,omitempty"`
	Unrestricted bool         `json:"unrestricted,omitempty"`
}

func (s RunSpec) MarshalJSON() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	w := wireSpec{
		Name:         s.Name,
		Labels:       s.Labels,
		Image:        wireImage{Ref: s.ImageRef},
		Workload:     wireWorkload{Adapter: "generic", Command: s.Command, Workdir: s.Workdir, User: s.User},
		Env:          s.Env,
		Resources:    s.Resources,
		Placement:    wirePlacement{Pool: s.Pool},
		Network:      wireNetwork{Egress: s.Egress, Unrestricted: s.UnrestrictedEgress},
		Sandbox:      s.Sandbox,
		ResumePolicy: ResumePolicyNever,
	}
	if s.Timeout > 0 {
		w.Timeout = s.Timeout.String()
	}
	if s.Grace > 0 {
		w.Workload.Grace = s.Grace.String()
	}
	for _, sec := range s.Secrets {
		w.Secrets = append(w.Secrets, wireSecret{Name: sec.Name, Value: sec.Value, As: "file", Path: sec.Path})
	}
	return json.Marshal(w)
}

// secretNameRe is Lux's secret name rule (spec.nameRe); the "lux-" prefix is
// reserved by Lux for its own files.
var secretNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,62}$`)

// MaxLabelValue is Lux's bound on a label value, in bytes; labelKeyRe is its
// label key alphabet.
const MaxLabelValue = 256

var labelKeyRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// Validate refuses specs Lux would reject on shape alone, and any secret
// value that also appears in the name, labels, env, command, workdir or user:
// those are stored with the Run and visible to every reader.
func (s RunSpec) Validate() error {
	var errs []string
	fail := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	if s.ImageRef == "" {
		fail("image ref is required")
	}
	if len(s.Command) == 0 || s.Command[0] == "" {
		fail("command is required")
	}
	if s.Workdir != "" && !path.IsAbs(s.Workdir) {
		fail("workdir must be absolute")
	}
	if s.Timeout < 0 {
		fail("timeout must not be negative")
	}
	if s.Grace < 0 {
		fail("grace must not be negative")
	}
	r := s.Resources
	if r.CPUs < 0 || r.Memory < 0 || r.Disk < 0 || r.Pids < 0 {
		fail("resources must not be negative")
	}
	for k, v := range s.Labels {
		// Neither the key nor the value is quoted: either could hold a secret.
		if !labelKeyRe.MatchString(k) {
			fail("a label key has characters Lux refuses")
		}
		if len(v) > MaxLabelValue {
			fail("a label value exceeds %d bytes", MaxLabelValue)
		}
	}
	for k := range s.Env {
		if strings.HasPrefix(k, "LUX_") {
			fail("env %q: the LUX_ prefix is reserved", k)
		}
	}
	if s.UnrestrictedEgress && len(s.Egress) > 0 {
		fail("egress rules and unrestricted egress are mutually exclusive")
	}
	for i, e := range s.Egress {
		switch {
		case (e.Host == "") == (e.CIDR == ""):
			fail("egress[%d]: exactly one of host or cidr", i)
		case e.CIDR != "":
			if _, err := netip.ParsePrefix(e.CIDR); err != nil {
				fail("egress[%d]: invalid cidr", i)
			}
		}
	}
	names := map[string]bool{}
	for i, sec := range s.Secrets {
		if !secretNameRe.MatchString(sec.Name) || strings.HasPrefix(sec.Name, "lux-") {
			fail("secrets[%d]: invalid name", i)
		}
		if names[sec.Name] {
			fail("secrets[%d]: duplicate name", i)
		}
		names[sec.Name] = true
		if sec.Value == "" {
			fail("secrets[%d]: value is required", i)
		}
		if !path.IsAbs(sec.Path) {
			fail("secrets[%d]: path must be absolute", i)
		}
		if sec.Value != "" && s.exposes(sec.Value) {
			fail("secrets[%d]: value appears outside secrets", i)
		}
	}
	if len(errs) > 0 {
		return errors.New("luxclient: invalid run spec: " + strings.Join(errs, "; "))
	}
	return nil
}

func (s RunSpec) exposes(value string) bool {
	public := []string{s.Name, s.Workdir, s.User, s.ImageRef, s.Pool}
	public = append(public, s.Command...)
	for k, v := range s.Labels {
		public = append(public, k, v)
	}
	for k, v := range s.Env {
		public = append(public, k, v)
	}
	for _, p := range public {
		if strings.Contains(p, value) {
			return true
		}
	}
	return false
}
