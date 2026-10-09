package luxclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// MinVersion is the first Lux release that implements resumePolicy never.
var MinVersion = [3]uint64{0, 1, 14}

var releaseRe = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// CheckCompatibility reads GET /openapi.json (unauthenticated in Lux) and
// requires info.version to be a release tag vX.Y.Z >= MinVersion and the
// RunSpec schema to declare resumePolicy. "dev", git-describe builds
// (v0.1.16-1-gabc), prereleases and anything unparseable are refused.
// The schema has no enum for resumePolicy, so presence alone does not prove
// "never" support: the version gate is what does.
func (c *Client) CheckCompatibility(ctx context.Context) (string, error) {
	var doc struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
		Components struct {
			Schemas struct {
				RunSpec struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"RunSpec"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := c.do(ctx, request{op: "openapi", method: http.MethodGet, path: "/openapi.json", anonymous: true, want: []int{http.StatusOK}}, &doc); err != nil {
		return "", err
	}
	v := doc.Info.Version
	m := releaseRe.FindStringSubmatch(v)
	if m == nil {
		return "", fmt.Errorf("%w: version is not a release tag", ErrIncompatible)
	}
	var got [3]uint64
	for i := range got {
		n, perr := strconv.ParseUint(m[i+1], 10, 64)
		if perr != nil {
			return "", fmt.Errorf("%w: version is not a release tag", ErrIncompatible)
		}
		got[i] = n
	}
	if slices.Compare(got[:], MinVersion[:]) < 0 {
		return "", fmt.Errorf("%w: version %s is older than v%d.%d.%d", ErrIncompatible, v, MinVersion[0], MinVersion[1], MinVersion[2])
	}
	prop, ok := doc.Components.Schemas.RunSpec.Properties["resumePolicy"]
	var obj map[string]json.RawMessage
	if !ok || json.Unmarshal(prop, &obj) != nil {
		return "", fmt.Errorf("%w: RunSpec schema has no resumePolicy", ErrIncompatible)
	}
	return v, nil
}

// Create submits spec with POST /v1/runs. idempotencyKey is required: Lux
// answers a repeat with the first Run (200) instead of creating another, per
// tenant. The request is sent once; on a transport error the outcome is
// unknown and the caller retries Create with the same key.
//
// If the response does not echo spec.resumePolicy "never", the returned Run
// is non-nil together with ErrResumePolicyNotEchoed: it exists and must be
// terminated, never given work.
func (c *Client) Create(ctx context.Context, idempotencyKey string, spec RunSpec) (*Run, error) {
	if !validIdempotencyKey(idempotencyKey) {
		return nil, errors.New("luxclient: invalid idempotency key")
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	// Encoding errors can quote values (a NaN cpus); none is passed on.
	body, err := json.Marshal(spec)
	if err != nil {
		return nil, errors.New("luxclient: cannot encode run spec")
	}
	if len(body) > maxBody {
		return nil, errors.New("luxclient: run spec too large")
	}
	var run Run
	err = c.do(ctx, request{
		op: "create run", method: http.MethodPost, path: "/v1/runs", body: body,
		idempotencyKey: idempotencyKey, want: []int{http.StatusCreated, http.StatusOK},
	}, &run)
	if err != nil {
		return nil, err
	}
	if run.ID == "" {
		return nil, errors.New("luxclient: create run: response has no id")
	}
	if run.Spec.ResumePolicy != ResumePolicyNever {
		return &run, ErrResumePolicyNotEchoed
	}
	return &run, nil
}

// validIdempotencyKey: visible ASCII, bounded, so it is a safe header value.
func validIdempotencyKey(k string) bool {
	if k == "" || len(k) > 255 {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

func (c *Client) Get(ctx context.Context, id string) (*Run, error) {
	p, err := runPath(id)
	if err != nil {
		return nil, err
	}
	var run Run
	if err := c.do(ctx, request{op: "get run", method: http.MethodGet, path: p, want: []int{http.StatusOK}}, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// Terminate is POST /v1/runs/{id}/terminate: idempotent in Lux (202 with the
// Run, also for one already terminated).
func (c *Client) Terminate(ctx context.Context, id string) (*Run, error) {
	p, err := runPath(id)
	if err != nil {
		return nil, err
	}
	var run Run
	if err := c.do(ctx, request{op: "terminate run", method: http.MethodPost, path: p + "/terminate", want: []int{http.StatusAccepted}}, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func runPath(id string) (string, error) {
	if id == "" || id == "." || id == ".." || len(id) > 255 {
		return "", errors.New("luxclient: invalid run id")
	}
	return "/v1/runs/" + url.PathEscape(id), nil
}

// runStates are Lux's Run state names, accepted by GET /v1/runs?state=.
var runStates = map[string]bool{
	"submitted": true, "scheduled": true, "provisioning": true, "starting": true,
	"running": true, "stopping": true, "stopped": true, "resuming": true,
	"succeeded": true, "failed": true, "terminated": true, "lost": true,
}

// List returns every Run carrying all the given labels and, when states is
// non-empty, in one of those states, newest first, by paging
// GET /v1/runs?sort=created&label=k=v...&state=a,b over its next cursors.
// Terminated Runs are retained by Lux, so a label-only listing grows without
// bound; pass the live states to keep it bounded. It fails rather than
// truncate: on a repeated cursor or past MaxPages.
func (c *Client) List(ctx context.Context, labels map[string]string, states ...string) ([]Run, error) {
	q := url.Values{"sort": {"created"}, "limit": {strconv.Itoa(pageSize)}}
	for _, s := range states {
		if !runStates[s] {
			return nil, errors.New("luxclient: invalid run state filter")
		}
	}
	if len(states) > 0 {
		q.Set("state", strings.Join(states, ","))
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		// Lux splits each selector at its first "=".
		if k == "" || strings.Contains(k, "=") {
			return nil, errors.New("luxclient: invalid label selector key")
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		q.Add("label", k+"="+labels[k])
	}
	var runs []Run
	seenRuns := map[string]bool{}
	seenCursors := map[string]bool{}
	for page := 0; ; page++ {
		if page >= c.maxPages {
			return nil, ErrTooManyPages
		}
		var body struct {
			Runs []Run  `json:"runs"`
			Next string `json:"next"`
		}
		if err := c.do(ctx, request{op: "list runs", method: http.MethodGet, path: "/v1/runs", query: q, want: []int{http.StatusOK}}, &body); err != nil {
			return nil, err
		}
		for _, r := range body.Runs {
			if !seenRuns[r.ID] {
				seenRuns[r.ID] = true
				runs = append(runs, r)
			}
		}
		if body.Next == "" {
			return runs, nil
		}
		if seenCursors[body.Next] {
			return nil, ErrRepeatedCursor
		}
		seenCursors[body.Next] = true
		q.Set("next", body.Next)
	}
}
