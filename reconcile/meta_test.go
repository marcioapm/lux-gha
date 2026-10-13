package reconcile

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/marcioapm/lux-gha/githubapp"
	"github.com/marcioapm/lux-gha/luxclient"
)

func ptr[T any](v T) *T { return &v }

func TestRunName(t *testing.T) {
	job := githubapp.WorkflowJob{ID: 7, RunID: 70, Name: "build", WorkflowName: ptr("Lib (Java)")}
	run := githubapp.WorkflowRun{ID: 70, Name: ptr("Lib (Java)"), HeadBranch: ptr("lux/lib-on-lux"), RunNumber: 12}
	cases := map[string]struct {
		run  githubapp.WorkflowRun
		job  githubapp.WorkflowJob
		want string
	}{
		"branch": {run, job, "widgets · Lib (Java) / build · lux/lib-on-lux run 12"},
		"pull request": {func() githubapp.WorkflowRun {
			r := run
			r.PullRequests = []githubapp.PullRequestRef{{Number: 34}}
			return r
		}(), job, "widgets · Lib (Java) / build · #34 run 12"},
		"several pull requests name the branch": {func() githubapp.WorkflowRun {
			r := run
			r.PullRequests = []githubapp.PullRequestRef{{Number: 34}, {Number: 35}}
			return r
		}(), job, "widgets · Lib (Java) / build · lux/lib-on-lux run 12"},
		"workflow from the run when the job has none": {func() githubapp.WorkflowRun {
			r := run
			r.Name = ptr("CI")
			return r
		}(), func() githubapp.WorkflowJob { j := job; j.WorkflowName = nil; return j }(), "widgets · CI / build · lux/lib-on-lux run 12"},
		"no workflow name": {func() githubapp.WorkflowRun { r := run; r.Name = nil; return r }(),
			func() githubapp.WorkflowJob { j := job; j.WorkflowName = nil; return j }(), "widgets · build · lux/lib-on-lux run 12"},
		"no job name, branch or number": {githubapp.WorkflowRun{ID: 70},
			githubapp.WorkflowJob{ID: 7, WorkflowName: ptr("CI")}, "widgets · CI / job 7"},
		"newlines and tabs": {run, func() githubapp.WorkflowJob { j := job; j.Name = "build\n\tall\r\n"; return j }(),
			"widgets · Lib (Java) / build all · lux/lib-on-lux run 12"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := runName("acme/widgets", 7, newJobMeta(tc.run, tc.job)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// hostile is a 1 KiB-plus name: emoji (4-byte runes), newlines, a NUL, an
// escape sequence and an invalid byte.
func hostile() string {
	return strings.Repeat("🚀 deploy\nstage\x00\x1b[31m é\xff ", 40)
}

func TestRunNameIsCutOnARuneBoundaryKeepingTheRunNumber(t *testing.T) {
	long := hostile()
	if len(long) < 1024 {
		t.Fatalf("fixture is %d bytes", len(long))
	}
	run := githubapp.WorkflowRun{ID: 70, HeadBranch: ptr(long), RunNumber: 123456}
	job := githubapp.WorkflowJob{ID: 7, RunID: 70, Name: long, WorkflowName: ptr(long)}
	got := runName("acme/widgets", 7, newJobMeta(run, job))
	if len(got) > maxRunName || !utf8.ValidString(got) || !strings.HasSuffix(got, "… run 123456") || !strings.HasPrefix(got, "widgets · 🚀 deploy stage") {
		t.Fatalf("%d bytes: %q", len(got), got)
	}
	if strings.ContainsFunc(got, unicode.IsControl) {
		t.Fatalf("control character in %q", got)
	}
	short := runName("acme/widgets", 7, jobMeta{job: "build", runNumber: 1})
	if short != "widgets · build run 1" {
		t.Fatalf("short name %q", short)
	}
}

func TestMetaLabelsAreSanitised(t *testing.T) {
	long := hostile()
	run := githubapp.WorkflowRun{ID: 70, HeadBranch: ptr(long), RunNumber: 3, PullRequests: []githubapp.PullRequestRef{{Number: 34}}}
	job := githubapp.WorkflowJob{ID: 7, RunID: 70, RunAttempt: 2, Name: long, WorkflowName: ptr(long), HeadSHA: strings.Repeat("ab", 20)}
	got := metaLabels("acme/widgets", newJobMeta(run, job))
	for _, k := range []string{LabelWorkflow, LabelJobName, LabelBranch} {
		v := got[k]
		if len(v) == 0 || len(v) > luxclient.MaxLabelValue || !utf8.ValidString(v) || strings.ContainsFunc(v, unicode.IsControl) {
			t.Fatalf("%s: %d bytes %q", k, len(v), v)
		}
		if !strings.HasPrefix(v, "🚀 deploy stage [31m é ") {
			t.Fatalf("%s: %q", k, v)
		}
	}
	if got[LabelRepoName] != "acme/widgets" || got[LabelRunID] != "70" || got[LabelRunAttempt] != "2" ||
		got[LabelPR] != "34" || got[LabelSHA] != "abababababab" {
		t.Fatalf("labels %v", got)
	}
	spec := luxclient.RunSpec{Name: runName("acme/widgets", 7, newJobMeta(run, job)), Labels: got, ImageRef: "img", Command: []string{"run"}}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMetaLabelsLeaveOutWhatIsUnknown(t *testing.T) {
	got := metaLabels("acme/widgets", newJobMeta(githubapp.WorkflowRun{ID: 70}, githubapp.WorkflowJob{ID: 7, WorkflowName: ptr(" \n ")}))
	want := map[string]string{LabelRepoName: "acme/widgets", LabelRunID: "70"}
	if !mapsEqual(got, want) {
		t.Fatalf("labels %v", got)
	}
}
