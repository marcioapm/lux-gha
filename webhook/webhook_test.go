package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const secret = "test-webhook-secret"

type recorder struct{ events []WorkflowJobEvent }

func (r *recorder) on(_ context.Context, e WorkflowJobEvent) error {
	r.events = append(r.events, e)
	return nil
}

func newHandler(t *testing.T, cb func(context.Context, WorkflowJobEvent) error, logs io.Writer) *Handler {
	t.Helper()
	if logs == nil {
		logs = io.Discard
	}
	h, err := New([]byte(secret), 42, func(r string) bool { return strings.EqualFold(r, "acme/widgets") }, cb, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func sign(body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func deliver(h http.Handler, event string, body []byte, signature string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", "72d3162e-cc78-11e3-81ab-4c9367dc0958")
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestSignatureIsVerifiedOverRawBytesBeforeDispatch(t *testing.T) {
	body := fixture(t, "workflow_job_queued.json")
	reformatted := bytes.ReplaceAll(body, []byte(": "), []byte(":"))
	tampered := bytes.Replace(body, []byte(`"id": 42`), []byte(`"id": 43`), 1)
	wrongKey := hmac.New(sha256.New, []byte("other"))
	wrongKey.Write(body)
	cases := map[string]struct {
		body []byte
		sig  string
	}{
		"missing":                {body, ""},
		"no prefix":              {body, strings.TrimPrefix(sign(body), "sha256=")},
		"sha1 prefix":            {body, "sha1=" + strings.TrimPrefix(sign(body), "sha256=")},
		"not hex":                {body, "sha256=zz"},
		"wrong key":              {body, "sha256=" + hex.EncodeToString(wrongKey.Sum(nil))},
		"semantically same JSON": {reformatted, sign(body)},
		"tampered after signing": {tampered, sign(body)},
		"truncated signature":    {body, sign(body)[:20]},
		"malformed but unsigned": {[]byte(`{`), ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{}
			w := deliver(newHandler(t, rec.on, nil), "workflow_job", tc.body, tc.sig)
			if w.Code != http.StatusUnauthorized || len(rec.events) != 0 {
				t.Fatalf("status %d, %d callbacks", w.Code, len(rec.events))
			}
		})
	}
}

// Well-formed signatures that agree with the true digest on a prefix must
// still fail: the full 32-byte digest has to match.
func TestSignatureRequiresEveryDigestByte(t *testing.T) {
	body := fixture(t, "workflow_job_queued.json")
	digest, err := hex.DecodeString(strings.TrimPrefix(sign(body), "sha256="))
	if err != nil {
		t.Fatal(err)
	}
	sigs := map[string]string{}
	for _, n := range []int{1, 2, 7, 16, 31} {
		sigs["prefix "+strconv.Itoa(n)] = "sha256=" + hex.EncodeToString(digest[:n])
	}
	// Odd hex lengths do not decode; included so a lenient decoder is caught too.
	sigs["odd hex 63"] = sign(body)[:7+63]
	sigs["extended"] = sign(body) + "00"
	for i := 1; i < len(digest); i++ {
		flipped := bytes.Clone(digest)
		flipped[i] ^= 0x01
		sigs["flip byte "+strconv.Itoa(i)] = "sha256=" + hex.EncodeToString(flipped)
	}
	for name, sig := range sigs {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{}
			w := deliver(newHandler(t, rec.on, nil), "workflow_job", body, sig)
			if w.Code != http.StatusUnauthorized || len(rec.events) != 0 {
				t.Fatalf("status %d, %d callbacks", w.Code, len(rec.events))
			}
		})
	}
	rec := &recorder{}
	if w := deliver(newHandler(t, rec.on, nil), "workflow_job", body, sign(body)); w.Code != http.StatusAccepted || len(rec.events) != 1 {
		t.Fatalf("valid signature: status %d, %d callbacks", w.Code, len(rec.events))
	}
}

func TestDispatchesTypedWorkflowJob(t *testing.T) {
	rec := &recorder{}
	h := newHandler(t, rec.on, nil)
	body := fixture(t, "workflow_job_completed.json")
	if w := deliver(h, "workflow_job", body, sign(body)); w.Code != http.StatusAccepted {
		t.Fatalf("status %d", w.Code)
	}
	if len(rec.events) != 1 {
		t.Fatalf("callbacks %d", len(rec.events))
	}
	got := rec.events[0]
	want := WorkflowJobEvent{
		DeliveryID: "72d3162e-cc78-11e3-81ab-4c9367dc0958", Action: "completed", InstallationID: 42,
		Repository: "acme/widgets", JobID: 29679449, RunID: 5132587829, RunAttempt: 2,
		Status: "completed", Conclusion: "cancelled", RunnerID: 23, RunnerName: "lux-gha-a1b2c3",
	}
	if strings.Join(got.Labels, ",") != "self-hosted,linux,lux" {
		t.Fatalf("labels %v", got.Labels)
	}
	got.Labels = nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}

	queued := fixture(t, "workflow_job_queued.json")
	deliver(h, "workflow_job", queued, sign(queued))
	if e := rec.events[1]; e.Action != "queued" || e.RunnerID != 0 || e.RunnerName != "" || e.Conclusion != "" {
		t.Fatalf("null runner fields mis-parsed: %+v", e)
	}
}

func TestAllDocumentedActionsDispatchAndUnknownIsIgnored(t *testing.T) {
	base := fixture(t, "workflow_job_queued.json")
	for _, action := range []string{"queued", "waiting", "in_progress", "completed", "requested_action"} {
		t.Run(action, func(t *testing.T) {
			rec := &recorder{}
			body := bytes.Replace(base, []byte(`"action": "queued"`), []byte(`"action": "`+action+`"`), 1)
			w := deliver(newHandler(t, rec.on, nil), "workflow_job", body, sign(body))
			wantCalls := 1
			wantCode := http.StatusAccepted
			if action == "requested_action" {
				wantCalls, wantCode = 0, http.StatusNoContent
			}
			if w.Code != wantCode || len(rec.events) != wantCalls {
				t.Fatalf("status %d, callbacks %d", w.Code, len(rec.events))
			}
		})
	}
}

func TestSignedNonJobEventsAndForeignSourcesAreIgnored(t *testing.T) {
	queued := fixture(t, "workflow_job_queued.json")
	ping := []byte(`{"zen":"Keep it logically awesome.","hook_id":1,"hook":{"type":"App"}}`)
	otherRepo := bytes.Replace(queued, []byte(`"full_name": "acme/widgets"`), []byte(`"full_name": "acme/secret-sauce"`), 1)
	otherInstall := bytes.Replace(queued, []byte(`"installation": {"id": 42}`), []byte(`"installation": {"id": 7}`), 1)
	cases := []struct {
		name, event string
		body        []byte
	}{
		{"ping", "ping", ping},
		{"push", "push", []byte(`{"ref":"refs/heads/main"}`)},
		{"other repository", "workflow_job", otherRepo},
		{"other installation", "workflow_job", otherInstall},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			w := deliver(newHandler(t, rec.on, nil), tc.event, tc.body, sign(tc.body))
			if w.Code != http.StatusNoContent || len(rec.events) != 0 {
				t.Fatalf("status %d, callbacks %d", w.Code, len(rec.events))
			}
		})
	}
}

func TestMalformedAndOversizedSignedBodies(t *testing.T) {
	rec := &recorder{}
	h := newHandler(t, rec.on, nil)
	for _, body := range [][]byte{[]byte(`{"action":`), []byte(`{"action":"queued"}`)} {
		if w := deliver(h, "workflow_job", body, sign(body)); w.Code != http.StatusBadRequest {
			t.Fatalf("status %d for %s", w.Code, body)
		}
	}
	// A signed workflow_job just over 1 MiB is refused before verification.
	queued := fixture(t, "workflow_job_queued.json")
	big := append(bytes.Repeat([]byte(" "), 1<<20+1-len(queued)), queued...)
	if w := deliver(h, "workflow_job", big, sign(big)); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status %d", w.Code)
	}
	atLimit := append(bytes.Repeat([]byte(" "), 1<<20-len(queued)), queued...)
	if w := deliver(newHandler(t, (&recorder{}).on, nil), "workflow_job", atLimit, sign(atLimit)); w.Code != http.StatusAccepted {
		t.Fatalf("1 MiB body status %d", w.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed || len(rec.events) != 0 {
		t.Fatalf("GET status %d", w.Code)
	}
}

type leakyError struct{}

func (leakyError) Error() string { return "jit config ZXlKaGJHY2lPaUpJVXpJMU5pSXNJblI1Y0NJ6" }

func TestCallbackFailureIsRetryableAndDoesNotLeak(t *testing.T) {
	var logs bytes.Buffer
	h := newHandler(t, func(context.Context, WorkflowJobEvent) error {
		return errors.Join(leakyError{}, errors.New("token ghs_supersecret"))
	}, &logs)
	body := fixture(t, "workflow_job_queued.json")
	w := deliver(h, "workflow_job", body, sign(body))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", w.Code)
	}
	out := w.Body.String() + logs.String()
	for _, s := range []string{"ZXlKaGJHY2", "ghs_supersecret", secret} {
		if strings.Contains(out, s) {
			t.Fatalf("leaked %q in %q", s, out)
		}
	}
}

func TestCallbackIsBoundedByDeliveryBudget(t *testing.T) {
	old := CallbackTimeout
	CallbackTimeout = 20 * time.Millisecond
	defer func() { CallbackTimeout = old }()
	h := newHandler(t, func(ctx context.Context, _ WorkflowJobEvent) error {
		<-ctx.Done()
		return ctx.Err()
	}, nil)
	body := fixture(t, "workflow_job_queued.json")
	if w := deliver(h, "workflow_job", body, sign(body)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", w.Code)
	}
}

func TestNewRequiresAllInputs(t *testing.T) {
	ok := func(string) bool { return true }
	cb := func(context.Context, WorkflowJobEvent) error { return nil }
	if _, err := New(nil, 42, ok, cb, nil); err == nil {
		t.Fatal("empty secret accepted")
	}
	if _, err := New([]byte("s"), 0, ok, cb, nil); err == nil {
		t.Fatal("zero installation accepted")
	}
	if _, err := New([]byte("s"), 42, ok, nil, nil); err == nil {
		t.Fatal("nil callback accepted")
	}
}

type installRecorder struct{ events []InstallationEvent }

func (r *installRecorder) on(_ context.Context, e InstallationEvent) error {
	r.events = append(r.events, e)
	return nil
}

func installationBody(action string, installation int64) []byte {
	return []byte(`{"action":"` + action + `","installation":{"id":` + strconv.FormatInt(installation, 10) + `},
		"repositories_added":[{"id":1,"full_name":"acme/new"}],"repositories_removed":[]}`)
}

func TestInstallationEventsAreSignedSignals(t *testing.T) {
	type tc struct {
		event, action string
		installation  int64
		want          int
	}
	var cases []tc
	for event, actions := range InstallationActions {
		for action := range actions {
			cases = append(cases, tc{event, action, 42, http.StatusAccepted}, tc{event, action, 7, http.StatusNoContent})
		}
	}
	cases = append(cases,
		tc{"installation", "created", 42, http.StatusNoContent},
		tc{"installation_repositories", "renamed", 42, http.StatusNoContent},
	)
	for _, c := range cases {
		t.Run(c.event+"/"+c.action+"/"+strconv.FormatInt(c.installation, 10), func(t *testing.T) {
			jobs, installs := &recorder{}, &installRecorder{}
			h := newHandler(t, jobs.on, nil)
			h.OnInstallation = installs.on
			body := installationBody(c.action, c.installation)
			w := deliver(h, c.event, body, sign(body))
			wantCalls := 0
			if c.want == http.StatusAccepted {
				wantCalls = 1
			}
			if w.Code != c.want || len(installs.events) != wantCalls || len(jobs.events) != 0 {
				t.Fatalf("status %d, installation callbacks %d", w.Code, len(installs.events))
			}
			if wantCalls == 1 {
				if got := installs.events[0]; got.Event != c.event || got.Action != c.action || got.InstallationID != 42 {
					t.Fatalf("event %+v", got)
				}
			}
			// The signature is checked before the event type is looked at.
			for _, sig := range []string{"", "sha256=" + hex.EncodeToString(make([]byte, sha256.Size))} {
				if w := deliver(h, c.event, body, sig); w.Code != http.StatusUnauthorized || len(installs.events) != wantCalls {
					t.Fatalf("signature %q: status %d, callbacks %d", sig, w.Code, len(installs.events))
				}
			}
		})
	}
	installs := &installRecorder{}
	body := installationBody("added", 42)
	// A signed delivery that is not JSON is refused before any callback.
	withInstall := newHandler(t, (&recorder{}).on, nil)
	withInstall.OnInstallation = installs.on
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-GitHub-Event", "installation_repositories")
	req.Header.Set("X-Hub-Signature-256", sign(body))
	w := httptest.NewRecorder()
	withInstall.ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType || len(installs.events) != 0 {
		t.Fatalf("form-encoded: status %d, callbacks %d", w.Code, len(installs.events))
	}
	// Without OnInstallation the events are ignored as before.
	if w := deliver(newHandler(t, (&recorder{}).on, nil), "installation_repositories", body, sign(body)); w.Code != http.StatusNoContent {
		t.Fatalf("no callback: status %d", w.Code)
	}
	h := newHandler(t, (&recorder{}).on, nil)
	h.OnInstallation = func(context.Context, InstallationEvent) error { return errors.New("x") }
	if w := deliver(h, "installation", installationBody("deleted", 42), sign(installationBody("deleted", 42))); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing callback: status %d", w.Code)
	}
	for _, bad := range [][]byte{[]byte(`{"action":`), []byte(`{"action":"added"}`)} {
		h.OnInstallation = installs.on
		if w := deliver(h, "installation_repositories", bad, sign(bad)); w.Code != http.StatusBadRequest || len(installs.events) != 0 {
			t.Fatalf("malformed %s: status %d", bad, w.Code)
		}
	}
}

func TestUnlistedRepositoryOfOurInstallationGoesToItsCallback(t *testing.T) {
	queued := fixture(t, "workflow_job_queued.json")
	otherRepo := bytes.Replace(queued, []byte(`"full_name": "acme/widgets"`), []byte(`"full_name": "acme/new"`), 1)
	otherBoth := bytes.Replace(otherRepo, []byte(`"installation": {"id": 42}`), []byte(`"installation": {"id": 7}`), 1)
	otherInstall := bytes.Replace(queued, []byte(`"installation": {"id": 42}`), []byte(`"installation": {"id": 7}`), 1)
	for _, c := range []struct {
		name               string
		body               []byte
		code, jobs, misses int
	}{
		{"in the set", queued, http.StatusAccepted, 1, 0},
		{"outside the set", otherRepo, http.StatusAccepted, 0, 1},
		{"other installation, outside the set", otherBoth, http.StatusNoContent, 0, 0},
		{"other installation, in the set", otherInstall, http.StatusNoContent, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			jobs, misses := &recorder{}, &recorder{}
			h := newHandler(t, jobs.on, nil)
			h.OnUnlistedRepository = misses.on
			w := deliver(h, "workflow_job", c.body, sign(c.body))
			if w.Code != c.code || len(jobs.events) != c.jobs || len(misses.events) != c.misses {
				t.Fatalf("status %d, jobs %d, unlisted %d", w.Code, len(jobs.events), len(misses.events))
			}
			if c.misses == 1 && (misses.events[0].Repository != "acme/new" || misses.events[0].Action != "queued") {
				t.Fatalf("unlisted event %+v", misses.events[0])
			}
		})
	}
}
