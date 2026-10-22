package registry

import (
	"context"
	"errors"
	"path"

	"github.com/marcioapm/lux-gha/config"
	"github.com/marcioapm/lux-gha/githubapp"
)

var errRepository = errors.New("registry: repository id or default branch changed")

// forkEvents are the events whose run from a fork is read-only. A
// pull_request_target run is not: it runs the base repository's workflow.
var forkEvents = map[string]bool{"pull_request": true, "pull_request_review": true, "pull_request_review_comment": true}

// trustedEvents may be trusted when their ref matches a trusted pattern. A
// reusable workflow's run reports its caller's event.
var trustedEvents = map[string]bool{"push": true, "workflow_dispatch": true, "schedule": true, "release": true, "workflow_run": true}

// trust maps the workflow run of the executing job to a trust level, first
// match wins: a pull request from a fork is read-only; a trusted event on a
// trusted ref is trusted; everything else is untrusted.
func (s *Server) trust(ctx context.Context, repo githubapp.Repository, repoID int64, run githubapp.WorkflowRun) (string, error) {
	if forkEvents[run.Event] && (run.HeadRepository == nil || run.HeadRepository.ID != repoID) {
		return ReadOnly, nil
	}
	if !trustedEvents[run.Event] || run.HeadBranch == nil || *run.HeadBranch == "" {
		return Untrusted, nil
	}
	ref, err := s.runRef(ctx, repo, run)
	if err != nil {
		return "", err
	}
	def, err := s.defaultBranch(ctx, repo, repoID)
	if err != nil {
		return "", err
	}
	for _, p := range s.o.Config.TrustedRefs {
		if p == config.DefaultBranchRef {
			if ref == "refs/heads/"+def {
				return Trusted, nil
			}
			continue
		}
		if ok, _ := path.Match(p, ref); ok {
			return Trusted, nil
		}
	}
	return Untrusted, nil
}

// runRef is the run's full ref. The API reports a tag push, release or
// dispatch on a tag by the tag's name in head_branch, like a branch. The
// name is taken as a tag when a trusted tag pattern could match it and the
// repository has that tag at the run's head commit; otherwise as a branch.
func (s *Server) runRef(ctx context.Context, repo githubapp.Repository, run githubapp.WorkflowRun) (string, error) {
	name := *run.HeadBranch
	tagRef := "refs/tags/" + name
	couldBeTag := false
	for _, p := range s.o.Config.TrustedRefs {
		if ok, _ := path.Match(p, tagRef); ok {
			couldBeTag = true
		}
	}
	if !couldBeTag {
		return "refs/heads/" + name, nil
	}
	for page := 1; page <= maxTagPages; page++ {
		tp, err := s.o.GitHub.ListTags(ctx, repo, githubapp.PageOptions{Page: page, PerPage: 100})
		if err != nil {
			return "", err
		}
		for _, t := range tp.Tags {
			if t.Name == name && t.Commit.SHA == run.HeadSHA {
				return tagRef, nil
			}
		}
		if !tp.HasNext {
			break
		}
	}
	return "refs/heads/" + name, nil
}

func (s *Server) defaultBranch(ctx context.Context, repo githubapp.Repository, repoID int64) (string, error) {
	now := s.o.Now()
	s.mu.Lock()
	d, ok := s.repos[repoID]
	s.mu.Unlock()
	if ok && now.Sub(d.at) < defaultBranchTTL {
		return d.name, nil
	}
	info, err := s.o.GitHub.GetRepository(ctx, repo)
	if err != nil {
		return "", err
	}
	if info.ID != repoID || info.DefaultBranch == "" {
		// A renamed-away or replaced repository: nothing is trusted.
		return "", errRepository
	}
	s.mu.Lock()
	s.repos[repoID] = defaultBranch{name: info.DefaultBranch, at: now}
	s.mu.Unlock()
	return info.DefaultBranch, nil
}
