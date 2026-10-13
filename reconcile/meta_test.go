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

// metaRepoID is acme/widgets' repository id in these tests and in fakeGitHub.
const metaRepoID = 1001

// pullRequest is a pull request number n whose base is repository base.
func pullRequest(n, base int64) githubapp.PullRequestRef {
	pr := githubapp.PullRequestRef{Number: n}
	pr.Base.Repo.ID = base
	return pr
}

// forkPR is run as GitHub lists it for a pull request from contributor's
// fork: no pull requests, the fork as head repository.
func forkPR(run githubapp.WorkflowRun) githubapp.WorkflowRun {
	run.Event = "pull_request"
	run.PullRequests = nil
	run.HeadRepository = &githubapp.HeadRepository{}
	run.HeadRepository.Owner.Login = "contributor"
	return run
}

func TestRunName(t *testing.T) {
	job := githubapp.WorkflowJob{ID: 7, RunID: 70, Name: "build", WorkflowName: ptr("Lib (Java)")}
	run := githubapp.WorkflowRun{ID: 70, Name: ptr("Lib (Java)"), HeadBranch: ptr("lux/lib-on-lux"), RunNumber: 12}
	withPRs := func(prs ...githubapp.PullRequestRef) githubapp.WorkflowRun {
		r := run
		r.PullRequests = prs
		return r
	}
	cases := map[string]struct {
		run  githubapp.WorkflowRun
		job  githubapp.WorkflowJob
		want string
	}{
		"branch":       {run, job, "widgets · Lib (Java) / build · lux/lib-on-lux run 12"},
		"pull request": {withPRs(pullRequest(34, metaRepoID)), job, "widgets · Lib (Java) / build · #34 run 12"},
		"several pull requests name the branch": {withPRs(pullRequest(34, metaRepoID), pullRequest(35, metaRepoID)), job,
			"widgets · Lib (Java) / build · lux/lib-on-lux run 12"},
		"a pull request into another repository names the branch": {withPRs(pullRequest(5, 2002)), job,
			"widgets · Lib (Java) / build · lux/lib-on-lux run 12"},
		"own pull request beside one into another repository": {withPRs(pullRequest(5, 2002), pullRequest(34, metaRepoID)), job,
			"widgets · Lib (Java) / build · #34 run 12"},
		"pull request from a fork names owner and branch": {forkPR(run), job, "widgets · Lib (Java) / build · contributor:lux/lib-on-lux run 12"},
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
			if got := runName("acme/widgets", 7, newJobMeta(metaRepoID, tc.run, tc.job)); got != tc.want {
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
	got := runName("acme/widgets", 7, newJobMeta(metaRepoID, run, job))
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
	run := githubapp.WorkflowRun{ID: 70, HeadBranch: ptr(long), RunNumber: 3, PullRequests: []githubapp.PullRequestRef{pullRequest(34, metaRepoID)}}
	job := githubapp.WorkflowJob{ID: 7, RunID: 70, RunAttempt: 2, Name: long, WorkflowName: ptr(long), HeadSHA: strings.Repeat("ab", 20)}
	got := metaLabels("acme/widgets", newJobMeta(metaRepoID, run, job))
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
	spec := luxclient.RunSpec{Name: runName("acme/widgets", 7, newJobMeta(metaRepoID, run, job)), Labels: got, ImageRef: "img", Command: []string{"run"}}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMetaLabelsLeaveOutWhatIsUnknown(t *testing.T) {
	got := metaLabels("acme/widgets", newJobMeta(metaRepoID, githubapp.WorkflowRun{ID: 70}, githubapp.WorkflowJob{ID: 7, WorkflowName: ptr(" \n ")}))
	want := map[string]string{LabelRepoName: "acme/widgets", LabelRunID: "70"}
	if !mapsEqual(got, want) {
		t.Fatalf("labels %v", got)
	}
}

func TestMetaLabelsLeaveOutAPullRequestIntoAnotherRepository(t *testing.T) {
	run := githubapp.WorkflowRun{ID: 70, HeadBranch: ptr("main"), PullRequests: []githubapp.PullRequestRef{pullRequest(5, 2002)}}
	got := metaLabels("acme/widgets", newJobMeta(metaRepoID, run, githubapp.WorkflowJob{ID: 7}))
	if _, ok := got[LabelPR]; ok || got[LabelBranch] != "main" {
		t.Fatalf("labels %v", got)
	}
}

func TestMetaLabelsNameAForkPullRequestsOwner(t *testing.T) {
	fork := forkPR(githubapp.WorkflowRun{ID: 70, HeadBranch: ptr("main")})
	if got := metaLabels("acme/widgets", newJobMeta(metaRepoID, fork, githubapp.WorkflowJob{ID: 7}))[LabelBranch]; got != "contributor:main" {
		t.Fatalf("fork pull request branch %q", got)
	}
	push := fork
	push.Event = "push"
	if got := metaLabels("acme/widgets", newJobMeta(metaRepoID, push, githubapp.WorkflowJob{ID: 7}))[LabelBranch]; got != "main" {
		t.Fatalf("push branch %q", got)
	}
	own := fork
	own.Event = "pull_request_target"
	own.PullRequests = []githubapp.PullRequestRef{pullRequest(34, metaRepoID)}
	got := metaLabels("acme/widgets", newJobMeta(metaRepoID, own, githubapp.WorkflowJob{ID: 7}))
	if got[LabelBranch] != "main" || got[LabelPR] != "34" {
		t.Fatalf("own pull request labels %v", got)
	}
}
