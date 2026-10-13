package reconcile

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/marcioapm/lux-gha/githubapp"
	"github.com/marcioapm/lux-gha/luxclient"
)

// Descriptive Run labels. Nothing reads them back: ownership, matching and
// idempotency use only the labels in jobLabels plus gen and runner.
const (
	LabelRepoName   = "lux-gha/repo-name"
	LabelWorkflow   = "lux-gha/workflow"
	LabelJobName    = "lux-gha/job-name"
	LabelRunID      = "lux-gha/run-id"
	LabelRunAttempt = "lux-gha/run-attempt"
	LabelBranch     = "lux-gha/branch"
	LabelPR         = "lux-gha/pr"
	LabelSHA        = "lux-gha/sha"
)

// maxRunName bounds the Run name in bytes. Neither luxclient nor the Lux
// documentation available to this repository states a Run-name limit, so it
// stays well inside the 256-byte bound Lux puts on a label value.
const maxRunName = 128

const shortSHA = 12

// jobMeta is what the demand listing told about a job: the job object and
// its workflow run. It costs no request beyond the listing.
type jobMeta struct {
	workflow, job, branch string
	runID, runAttempt     int64
	runNumber             int64
	pr                    int64 // 0 when the run lists no pull request
	sha                   string
}

func newJobMeta(run githubapp.WorkflowRun, j githubapp.WorkflowJob) jobMeta {
	m := jobMeta{job: j.Name, runID: j.RunID, runAttempt: j.RunAttempt, runNumber: run.RunNumber, sha: j.HeadSHA}
	// A run can list several pull requests sharing its head; only one is
	// named, and only when it is unambiguous.
	if len(run.PullRequests) == 1 {
		m.pr = run.PullRequests[0].Number
	}
	switch {
	case j.WorkflowName != nil && *j.WorkflowName != "":
		m.workflow = *j.WorkflowName
	case run.Name != nil:
		m.workflow = *run.Name
	}
	if run.HeadBranch != nil {
		m.branch = *run.HeadBranch
	}
	if m.runID == 0 {
		m.runID = run.ID
	}
	if m.sha == "" {
		m.sha = run.HeadSHA
	}
	return m
}

// runName is "<repo> · <workflow> / <job> · <#PR or branch> run <number>",
// with absent parts left out, cut to maxRunName on a rune boundary. It is
// for people only; nothing parses it.
func runName(full string, jobID int64, m jobMeta) string {
	_, repo, _ := strings.Cut(full, "/")
	job := cleanText(m.job)
	if job == "" {
		job = "job " + strconv.FormatInt(jobID, 10)
	}
	head := cleanText(repo) + " · "
	if wf := cleanText(m.workflow); wf != "" {
		head += wf + " / "
	}
	head += job
	if m.pr > 0 {
		head += " · #" + strconv.FormatInt(m.pr, 10)
	} else if b := cleanText(m.branch); b != "" {
		head += " · " + b
	}
	tail := ""
	if m.runNumber > 0 {
		tail = " run " + strconv.FormatInt(m.runNumber, 10)
	}
	// The run number is kept whole; the free text before it gives way.
	if len(head)+len(tail) > maxRunName {
		head = cutRunes(head, maxRunName-len(tail)-len("…")) + "…"
	}
	return head + tail
}

// metaLabels returns the descriptive labels for a job, each value cleaned
// and cut to Lux's label-value bound; empty values are left out.
func metaLabels(full string, m jobMeta) map[string]string {
	sha := m.sha
	if len(sha) > shortSHA {
		sha = sha[:shortSHA]
	}
	raw := map[string]string{
		LabelRepoName: full,
		LabelWorkflow: m.workflow,
		LabelJobName:  m.job,
		LabelBranch:   m.branch,
		LabelSHA:      sha,
	}
	if m.runID > 0 {
		raw[LabelRunID] = strconv.FormatInt(m.runID, 10)
	}
	if m.runAttempt > 0 {
		raw[LabelRunAttempt] = strconv.FormatInt(m.runAttempt, 10)
	}
	if m.pr > 0 {
		raw[LabelPR] = strconv.FormatInt(m.pr, 10)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if v = labelValue(v); v != "" {
			out[k] = v
		}
	}
	return out
}

func labelValue(s string) string {
	return cutRunes(cleanText(s), luxclient.MaxLabelValue)
}

// cleanText replaces control characters, line and paragraph separators and
// invalid UTF-8 with spaces, then collapses runs of whitespace.
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, " ")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}
