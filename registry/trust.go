package registry

import (
	"context"
	"errors"
	"path"
	"time"

	"github.com/marcioapm/lux-gha/githubapp"
)

var errRepository = errors.New("registry: repository id changed, or its name or default branch is missing")

// trustedEvents may be trusted when their ref is trusted. Each can run on a
// tag, so its ref is resolved against both tags and branches. A reusable
// workflow's run reports its caller's event.
var trustedEvents = map[string]bool{"push": true, "workflow_dispatch": true, "schedule": true, "release": true, "workflow_run": true}

// trust maps the workflow run of the executing job to a trust level. First
// match wins:
//  1. read-only: the run's head repository is missing or another repository
//     (a fork), for every event;
//  2. untrusted: the event is not a trusted event, or the run's ref does not
//     resolve to exactly one of refs/tags/<name> at the run's head commit
//     and refs/heads/<name> containing it;
//  3. trusted: the ref is the repository's default branch or matches a
//     trustedRefs pattern;
//  4. untrusted otherwise.
func (s *Server) trust(ctx context.Context, repo githubapp.Repository, repoID int64, run githubapp.WorkflowRun) (string, error) {
	if run.HeadRepository == nil || run.HeadRepository.ID != repoID {
		return ReadOnly, nil
	}
	if !trustedEvents[run.Event] || run.HeadBranch == nil || *run.HeadBranch == "" {
		return Untrusted, nil
	}
	ref, ok, err := s.runRef(ctx, repo, repoID, *run.HeadBranch, run.HeadSHA)
	if err != nil || !ok {
		return Untrusted, err
	}
	def, err := s.defaultBranch(ctx, repo, repoID)
	if err != nil {
		return "", err
	}
	if ref == "refs/heads/"+def {
		return Trusted, nil
	}
	for _, p := range s.o.Config.TrustedRefs {
		if ok, _ := path.Match(p, ref); ok {
			return Trusted, nil
		}
	}
	return Untrusted, nil
}

type refKey struct {
	repoID    int64
	name, sha string
}

type cachedRef struct {
	ref string // empty: ambiguous or unresolved
	at  time.Time
}

// runRef resolves the run's head_branch, which the API fills with a tag's
// name for a tag push, release or dispatch on a tag, to its full ref: the
// tag when it points at the run's head commit, the branch when the branch
// contains that commit. A branch name alone proves nothing: a tag named
// "main", pushed and then deleted or moved, leaves only the branch to find.
// ok is false when the name is both such a tag and such a branch, or
// neither. The answer is reused for repositoryTTL per repository, name and
// commit.
func (s *Server) runRef(ctx context.Context, repo githubapp.Repository, repoID int64, name, sha string) (string, bool, error) {
	key := refKey{repoID, name, sha}
	now := s.o.Now()
	s.mu.Lock()
	c, hit := s.refs[key]
	s.mu.Unlock()
	if hit && now.Sub(c.at) < repositoryTTL {
		return c.ref, c.ref != "", nil
	}
	tagSHA, isTag, err := s.o.GitHub.TagCommit(ctx, repo, name)
	if err != nil {
		return "", false, err
	}
	isTag = isTag && tagSHA == sha
	onBranch, err := s.o.GitHub.BranchContains(ctx, repo, name, sha)
	if err != nil {
		return "", false, err
	}
	ref := ""
	switch {
	case isTag && !onBranch:
		ref = "refs/tags/" + name
	case onBranch && !isTag:
		ref = "refs/heads/" + name
	}
	s.mu.Lock()
	s.refs[key] = cachedRef{ref: ref, at: now}
	s.mu.Unlock()
	return ref, ref != "", nil
}

func (s *Server) defaultBranch(ctx context.Context, repo githubapp.Repository, repoID int64) (string, error) {
	info, err := s.repository(ctx, repo, repoID)
	return info.DefaultBranch, err
}

// repository is GET /repos for the runner's repository, reused for
// repositoryTTL: its current owner/name and default branch.
func (s *Server) repository(ctx context.Context, repo githubapp.Repository, repoID int64) (githubapp.RepositoryInfo, error) {
	now := s.o.Now()
	s.mu.Lock()
	d, ok := s.repos[repoID]
	s.mu.Unlock()
	if ok && now.Sub(d.at) < repositoryTTL {
		return d.info, nil
	}
	info, err := s.o.GitHub.GetRepository(ctx, repo)
	if err != nil {
		return githubapp.RepositoryInfo{}, err
	}
	if info.ID != repoID || info.DefaultBranch == "" || info.FullName == "" {
		// A renamed-away or replaced repository: nothing is trusted.
		return githubapp.RepositoryInfo{}, errRepository
	}
	s.mu.Lock()
	s.repos[repoID] = cachedRepository{info: info, at: now}
	s.mu.Unlock()
	return info, nil
}
