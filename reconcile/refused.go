package reconcile

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/marcioapm/lux-gha/config"
	"github.com/marcioapm/lux-gha/githubapp"
	"github.com/marcioapm/lux-gha/luxclient"
)

// sizeErrors are the 422 codes with which luxd refuses a Run's
// resources.size or the size its cpus, memory and disk round up to. The
// same submission is refused again on every retry.
var sizeErrors = []string{"unknown_size", "size_unsupported", "size_conflict", "size_too_large"}

// sizeRefusal returns the Lux error code when err refuses the Run's size.
func sizeRefusal(err error) (string, bool) {
	var apiErr *luxclient.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity && slices.Contains(sizeErrors, apiErr.Code) {
		return apiErr.Code, true
	}
	return "", false
}

// refusal is a queued job whose Run Lux refused for its size. The job is
// not provisioned again; it gets a check run and its workflow run is
// force-cancelled, so it fails instead of waiting in the queue.
type refusal struct {
	repoID, jobID, runID int64
	sha, job, code       string
	route                *config.Route
	explained, cancelled bool
}

// refuse records j's size refusal and deletes the runner registered for
// it: a 4xx means no Run was created, so nothing will ever start it.
func (p *pass) refuse(j provisionable, code, name string, runnerID int64, log *slog.Logger) error {
	r, v := p.r, j.v
	meta := v.meta[j.jobID]
	r.refused[jobKey(v, j.jobID)] = &refusal{repoID: v.id, jobID: j.jobID, runID: meta.runID, sha: meta.sha, job: meta.job, code: code, route: j.route}
	log.Error("Lux refused the Run's size; failing the job", "code", code, "pool", j.route.Pool, "size", j.route.Size)
	p.setError(ErrSizeRefused)
	err := r.gh.DeleteRepoRunner(p.ctx, v.ref, runnerID)
	if p.rateLimited(err) {
		return errAbort
	}
	if err != nil {
		log.Info("runner of a refused job not deleted; orphan cleanup will", "runner", name, "error", err)
		return nil
	}
	p.st.Deleted++
	delete(v.runners, name)
	return nil
}

// closeRefused explains and cancels each refused job still queued in a
// repository read this pass, and forgets one no longer queued there.
func (p *pass) closeRefused() error {
	r := p.r
	for _, v := range p.repos {
		for _, jobID := range sortedKeys(v.demand) {
			f, ok := r.refused[jobKey(v, jobID)]
			if !ok {
				continue
			}
			log := r.log.With("repository", v.full, "workflowRun", f.runID, "job", jobID, "code", f.code)
			var err error
			if !f.explained && !p.noChecks {
				f.explained, err = p.postCheck(v, r.refusalCheck(f), log)
				if err != nil {
					return err
				}
			}
			if !f.cancelled && !p.noCancel {
				f.cancelled, err = p.cancelRun(v, f.runID, log)
				if err != nil {
					return err
				}
			}
		}
	}
	if p.truncated {
		return nil
	}
	for _, v := range p.repos {
		for key, f := range r.refused {
			if _, queued := v.demand[f.jobID]; f.repoID == v.id && !queued {
				delete(r.refused, key)
			}
		}
	}
	return nil
}

// refusalCheck is the check run on the job's commit; its external_id names
// the job, as no Lux Run exists.
func (r *Reconciler) refusalCheck(f *refusal) githubapp.CheckRunRequest {
	route := f.route
	var b strings.Builder
	fmt.Fprintf(&b, "Lux refused the Run for this job's runner, so no runner can take the job. "+
		"The bridge does not retry it and cancels the workflow run.\n\n")
	fmt.Fprintf(&b, "| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Route | `%s` |\n", route.Name)
	fmt.Fprintf(&b, "| Pool | `%s` |\n", route.Pool)
	if route.Size != "" {
		fmt.Fprintf(&b, "| Size | `%s` |\n", route.Size)
	} else {
		fmt.Fprintf(&b, "| Resources | cpus %g, memoryMiB %d, diskGiB %d |\n", route.CPUs, route.MemoryMiB, route.DiskGiB)
	}
	fmt.Fprintf(&b, "| Lux error | `%s` |\n", f.code)
	fmt.Fprintf(&b, "\n%s\n", refusalHint(f.code, route))
	return githubapp.CheckRunRequest{
		Name: CheckName, HeadSHA: f.sha, ExternalID: "job-" + strconv.FormatInt(f.jobID, 10), Status: "completed", Conclusion: "failure",
		CompletedAt: r.now(),
		Output: githubapp.CheckRunOutput{
			Title:   cutRunes(`Lux refused job "`+cmp.Or(f.job, "job "+strconv.FormatInt(f.jobID, 10))+`": `+f.code, githubapp.MaxCheckTitle),
			Summary: b.String(),
		},
	}
}

func refusalHint(code string, route *config.Route) string {
	switch code {
	case "unknown_size":
		return fmt.Sprintf("Pool `%s` has no size `%s`. Use a size the pool has in `runs-on` (`lux pools sizes %s` lists them), or ask the bridge's operator to fix route `%s`.",
			route.Pool, route.Size, route.Pool, route.Name)
	case "size_unsupported":
		return fmt.Sprintf("Pool `%s` has no shape, so it takes no size: route `%s` must ask for cpus, memoryMiB and diskGiB instead, or name a shaped pool.",
			route.Pool, route.Name)
	case "size_too_large":
		return fmt.Sprintf("Route `%s` asks for more than the largest size of pool `%s`.", route.Name, route.Pool)
	}
	return fmt.Sprintf("Route `%s` asks pool `%s` for a size together with cpus, memory or disk.", route.Name, route.Pool)
}
