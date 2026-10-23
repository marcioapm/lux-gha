# <img src="docs/brand/lux-gha.svg" alt="" width="36" align="top"> lux-gha

A bridge supplying one-shot, just-in-time GitHub Actions self-hosted runners
as Lux generic Runs. GitHub remains the workflow engine; Lux runs the runner
process.

## Packages

- `config`: JSON configuration, validated before serving. It has an explicit installation ID, a runner group ID, optional allowed repositories (absent: every repository of the installation), routes keyed by label set, and reconciler settings.
- `githubapp`: GitHub App client:
  - RS256 app JWT
  - cached installation tokens
  - the installation's repositories
  - repository lookup (numeric ID)
  - repository JIT config
  - workflow job and run reads
  - listing and deleting repository runners
  - creating and listing check runs, force-cancelling a workflow run, and re-running a job
- `luxclient`: thin HTTP client for Lux Runs:
  - create (with `Idempotency-Key`), get, paged list and terminate
  - a startup compatibility check
- `webhook`: signed `workflow_job` ingress that calls an injected function, and optionally `installation` and `installation_repositories` ingress.
- `reconcile`: the single reconciler that provisions and removes runners.
- `cache`: an optional GitHub Actions cache server (cache service v2) backed by S3, on its own listener ([Actions cache](#actions-cache)).
- `registry`: the runner listener of the [in-region image registry](#in-region-image-registry): identity tokens, trust levels and STS credentials for the job a runner executes.
- `cmd/lux-gha`: loads config and secrets, checks Lux compatibility, starts the reconciler, then serves `GET /healthz`, `GET /status` and `POST /webhook`, with a `cache` section the Actions cache on a second listener, and with a `registry` block the runner listener on its own address. `lux-gha --version` prints the version (`dev` unless set at build time) and exits; it is also logged at startup.

The runner image is in `images/runner` (see its [README](images/runner/README.md) and the [Runner entrypoint contract](#runner-entrypoint-contract)). Besides the runner and rootless Docker, it ships `moby/buildkit` for its own architecture, pinned by digest. The entrypoint loads it into each Run's Docker store, so that `docker/setup-buildx-action`'s default `docker-container` builder does not pull BuildKit from Docker Hub on every job. Workflows should pin `driver-opts: image=moby/buildkit:<version>` to the version the image carries (its `io.github.marcioapm.lux-gha.buildkit` label). With `buildx-stable-1`, buildx downloads a newer BuildKit once that tag moves ([BuildKit driver](images/runner/README.md#buildkit-driver)).

### Runner attribution

A JIT runner registered with a set of labels can be assigned by GitHub to **any** queued job whose labels it satisfies. That job need not be the one whose `queued` event led to the runner being provisioned. Nothing here assumes a provisioned runner belongs to the triggering job.

## Reconciliation

One goroutine runs reconciliation passes, never two at once. A pass starts at
startup, every `reconcile.interval`, and after an accepted `queued` delivery
for a job whose labels match a route. Other deliveries are left to the tick.
A delivery only wakes the loop (a one-slot channel, so wakes coalesce) and is
answered at once; a woken pass starts no sooner than `max(5s, interval/12)`
after the previous pass started, and a tick that falls in that wait is served
by the woken pass. The loop does not tick during the wait, so with
`interval < 5s` a wake can delay the next pass past the next tick. The
event's contents are not trusted beyond that, because every pass re-reads job
state from GitHub.

A repository read costs `2 + active workflow runs + ⌈runners/100⌉` GitHub
requests (active = `queued` or `in_progress`, including runs with only
GitHub-hosted jobs), plus one per provisioned runner. A job whose Lux Run
ended under it adds two or three more, once. A job lost to host loss adds
one job read when it is found, one workflow-run read once its workflow run
is no longer listed active, one workflow listing to check that the run was
not superseded, and the re-run; nothing while its workflow run is listed
active (a run not listed because `maxJobsPerPass` truncated the
`in_progress` listing costs one workflow-run read per pass while it waits).
Each pass also reads each Lux Run that has ended since the previous pass,
one `GET /v1/runs/{id}` per Run end, and sometimes lists the instance's
ended Lux Runs
([When Lux stops a job](#when-lux-stops-a-job)). With a `repositories`
list every listed repository is read every pass; size `interval` against the
installation's hourly limit and the number of repositories. Without one, a
pass reads a bounded subset ([Repository discovery](#repository-discovery)).
There is no database: the only persistent
state is Lux Run labels and GitHub's own records.

The bridge keeps **one live Run per queued matching job**. A pass:

1. **Demand.** For each repository read this pass, lists workflow runs with status `in_progress`
   before `queued` and their latest-attempt jobs. A job is demand when its
   status is `queued` and its labels match a route. `waiting` jobs
   (environment approval) are not demand. At most `reconcile.maxJobsPerPass`
   queued matching jobs are admitted per pass; other listed jobs do not spend
   this allowance. Its ceiling half is reserved for `in_progress` workflows
   and floor half for `queued` workflows, without borrowing unused portions.
   At a limit of one, the allocated status alternates across passes in memory,
   starting with `in_progress` after restart. Each listing retains its page
   bound; exhaustion marks the pass truncated and a cut-short workflow partial,
   so unseen siblings cannot permit force-cancel and pruning waits for a
   complete pass. Repository order remains configuration order (name order
   when discovered): earlier backlogs can still delay later repositories,
   including their stopped-job recovery. This is not cross-repository fairness.
2. **Supply.** Lists the instance's live Lux Runs (`submitted`, `scheduled`,
   `provisioning`, `starting`, `running`, `stopping`, `resuming`) and the
   repository's runners named `lux-gha-<instance>-...`.
3. **Provision.** A demand job with no live Run labelled with its ID gets a
   JIT runner and a Run, within the route's and the global `maxRunners`. The
   runner is named `lux-gha-<instance>-<repo id>-<job id>-<gen>` and the Run is
   submitted with
   `Idempotency-Key: <github host>/<instance>/<repo id>/<job id>/<gen>`.
   `gen` is one more than the highest gen of the job's earlier Runs, so it
   rises only when an earlier attempt ended while the job was still queued
   (spot loss, secrets lost to a luxd restart, or its runner took another
   job). Before provisioning, the job's Runs are listed again: a live one
   (a submission whose outcome was unknown and has since committed) is
   counted as its supply instead. A registered runner for the job whose gen
   has no Run at all may still have a submission in flight, so the job waits
   until that runner's Run appears or orphan cleanup deletes the runner.
   Past `reconcile.maxGensPerJob` the job is logged and left alone. A Run
   returned by Lux (new or replayed) whose `managed`, `repo`, `job`, `gen` or
   `runner` label differs from the submission is logged, neither counted nor
   terminated, and provisioning stops for the pass. A JIT
   `409` (a runner of that name already exists: the bridge stopped between JIT
   and Run submission) deletes that runner; the job is provisioned next pass.
   If Lux does not echo `resumePolicy: never`, the Run is terminated and
   provisioning stops for the pass. If Lux refuses the Run's size, the job
   fails at once and is not resubmitted ([Sizes](#sizes)).
   Every managed runner whose job was not looked at this way (cancelled,
   picked by another runner, or beyond `maxJobsPerPass`) is checked too: a
   live Run is counted, and a runner with no Run holds a slot against the
   global cap and every route whose labels it carries (GitHub adds its
   default labels, so more than one can match). If any repository selected
   this pass could not be read, its submissions are unknown and no Run is
   created that pass.
4. **Surplus.** An idle (not busy) runner whose Run is live, whose triggering
   job has not been queued for `reconcile.idleGrace`, and which exceeds its
   repository and route's queued demand, is deleted from GitHub and then its
   Run is terminated. A Run is terminated only after its runner was deleted,
   and GitHub answers a delete of a runner assigned to a job with `422`
   "Runner … is still running a job", which actions-runner-controller relies
   on (`controllers/actions.summerwind.net/runner_graceful_stop.go`; the
   scale-set API's equivalent is `JobStillRunningException`, see its
   `docs/adrs/2022-12-27-pick-the-right-runner-to-scale-down.md`). Any delete
   error other than a rate limit leaves the runner and its Run alone.
5. **Finished runs and orphans.** A live Run whose runner is no longer
   registered (it ran its job and deregistered) is terminated if still live
   `idleGrace` later. An offline managed runner with no live Run (lost before
   pickup, or a submission whose outcome stayed unknown) is deleted `idleGrace` after it was
   first seen. A Run lost mid-job is never resubmitted for the same job
   attempt: GitHub fails the job, and step 6 explains it, makes sure it does
   not hang, and re-runs it if its Run's host was lost.
6. **Stopped jobs.** An `in_progress` job whose runner is one of this
   instance's and whose Run has ended gets a check run, and is
   force-cancelled if it stays stuck. A job whose Run's host was lost,
   found from this or from Lux's ended Runs, is re-run once its workflow
   run completes, within `reconcile.hostLossReruns`
   ([When Lux stops a job](#when-lux-stops-a-job)).

Steps 3 to 6 act only on repositories read successfully this pass. A
repository not read this pass, because it was not selected, its read
failed, or it left the set, is **unknown**: no Run is created,
terminated or adopted for it, none of its runners is deleted, and its live
Runs still count against the global and route caps (they are in the
instance's Lux listing, whichever repository they belong to).

### Repository discovery

Without `repositories`, the bridge serves every repository the App
installation can access. Installing the App on a repository (with
"Only select repositories") is the on switch; a workflow job still opts in
through `runs-on` labels that match a route.

**The set.** `GET /installation/repositories` (installation token,
`per_page=100`, following `Link`) is listed at startup, every
`reconcile.repositoryRefresh`, and on the next pass after a signed delivery
for this installation of:

- `installation_repositories` (`added`, `removed`);
- `installation` (`deleted`, `suspend`, `unsuspend`, `new_permissions_accepted`);
- `workflow_job` from a repository outside the current set (it may have just
  been added).

GitHub sends the first two to every App without a subscription. A delivery
only marks the set stale and wakes the loop; the listing happens in the pass.
Archived and disabled repositories are left out. A failed listing keeps the
last good set, is logged, and is retried on the next pass; before the first
success the set is empty, so no repository is read and no Run is created. The
webhook accepts `workflow_job` deliveries for repositories in the set and
checks the installation ID exactly as with a list.

**Hot and cold.** Reading all of a large installation every pass would exceed
its rate limit, so each **tick pass** (the first pass, and every `interval`)
reads:

- every **hot** repository: one with a live managed Run in Lux; one whose
  last read found a managed runner; one whose last read found a queued job a
  route serves, while the global cap and that job's route cap have room; and
  one with a `queued` delivery for such a job that no read has listed yet,
  for at most `reconcile.hotWindow` after the latest such delivery;
- every repository not yet read successfully since startup;
- a round-robin slice of the other, **cold**, repositories, least recently
  selected first. Each pass takes its elapsed time's share of the cold set
  over `coldSweep − interval` (the fraction carries to the next pass, so woken
  passes add no reads), and also takes any cold repository that would
  otherwise go unread past `coldSweep` before the next tick. A selected
  repository moves to the back of the rotation whether its read succeeded or
  not.

The per-pass counts (known, hot, new, cold, read, skipped) are logged at debug.
A pass lists Lux's live Runs before it selects repositories, so a Run that
appeared since the last pass makes its repository hot at once. Demand the
caps keep unserved goes cold; the tick pass after a slot frees reads every
repository whose last read found demand again.

A **woken pass** (a matching `queued` delivery woke the loop, and no tick is
due) reads only the repositories with such a delivery, or a `workflow_job`
delivery from outside the set, since the previous pass selected its
repositories; a refresh an installation event requested still lists the
installation first. Every other repository is unread in that pass, under the
rules below. A tick that comes due while a wake waits out the minimum gap
makes that pass a tick pass. With `repositories`, woken and tick passes both
read every listed repository.

A repository that leaves the set (removed from the installation, archived or
disabled) is no longer read. Its live Runs are neither adopted nor
terminated; they count against the caps until they end on their own (the
runner exits, or the route's `timeout`). Its runners stay registered. A read
that answers 404 or 403 re-lists the installation on the next pass, so a
removal whose `installation_repositories` delivery was lost blocks creation
for a pass or two, not `repositoryRefresh`.

**Rate-limit arithmetic.** With the defaults (`interval` 60 s, `coldSweep`
10 m) and 68 repositories of which 2 are hot, a tick pass reads the 2 hot
repositories (`2 × (3 + their active runs)`, 8 with one active run each)
and about `66 / 9 ≈ 7.3` cold ones at 3 requests each (22), plus one listing
page every 10 minutes: about 30 requests per pass, about 1,800 per hour.
The cold share is paced by time, not by passes, so it stays near
`66 × 3 × 60 / 9 ≈ 1,320` requests per hour however often webhooks wake the
loop. The startup pass reads every repository once, at about 3 requests each
(about `68 × 3 ≈ 204`). An installation's limit is 5,000 requests per hour
(more for large organisations), so this leaves well over half of it free.

Hot reads happen once per tick: `hot × ~4 requests × 60` per hour. A woken
pass costs about `K × 4` requests, K being the repositories with a delivery
since the previous pass, whatever the hot set (plus one listing page when an
installation event asked for a refresh); so wakes, at most every 5 s,
add about 4 requests per delivered repository rather than a hot-set read.
During a burst the hot set is bounded by free capacity, not by the
installation: repositories with live Runs (at most `maxRunners`), demand only
while a slot is free for it, and deliveries whose job no read has listed yet.
The burst worst case is therefore about
`hot × 4 × 60 + deliveries × 4` per hour on top of the cold share. With
`maxRunners` 10, every slot used by a different repository, and a backlog of
67 matching `queued` deliveries arriving one per 5 s wake over 15 minutes,
that is `10 × 4 × 60 + 67 × 4 ≈ 2,700`; the test suite measures 4,287
requests in that hour with the cold share and the first tick's reads of
every delivered repository, and 1,803 for 2 hot repositories with 10
minutes of wakes. Past that, a rate limit pauses passes until GitHub's reset
time.

### Lifecycle safety with partial reads

Every rule below holds per pass for the repositories read in it; an unread
repository is unknown, as above. A woken pass reads only its delivered
repositories, so it leaves more repositories unread than a tick pass; the
rules need nothing more of it than they need of a tick pass with a failed
read.

- **Unread repositories.** A hot repository is read every tick pass, so a
  repository a tick pass leaves unread has no live Run and no remembered
  runner, unless its read failed or it left the set; then its live Runs are
  kept and counted all the same. A woken pass leaves hot repositories unread
  too, with their live Runs counted the same way. The grace timers of
  unread repositories (`idleGrace` for surplus, finished and orphaned
  runners) keep running across woken passes; only a pass that reads the
  repository resets or acts on them.
- **Read failure.** If any repository selected this pass fails to read, no Run
  is created that pass, exactly as with a list. A cold repository that keeps
  failing blocks creation only on the passes that select it (its rotation slot
  moves on). A hot one, or one not yet read successfully since startup, is
  selected every tick pass and so blocks creation until it reads: it may hold
  a registration whose submission is unresolved. A repository that stays in
  the listing but always fails to read therefore blocks creation in every
  repository; it is logged at `WARN` with its name, and without
  `repositories` the fix is to remove it from the installation.
- **Unresolved submissions.** A registered runner with no Run may still have a
  submission in flight, so it holds a cap slot. In a repository read this pass
  it is classified as before. In any other repository, a remembered managed
  runner whose name no live Run carries holds a slot, against the global cap
  and each route its labels match; a remembered runner is what its last
  successful read found, plus the runners this process registered. A
  repository with a remembered managed runner is hot, so every tick pass reads
  it; if that read fails, creation is blocked. If it left the set, its
  remembered runners without a live Run keep holding slots for `idleGrace`
  after it left, the same delay orphan cleanup gives a readable one.
- **Restart.** After a restart nothing is remembered, so a registration left
  by the previous process cannot hold a slot until its repository is read. No
  Run is created by any pass, tick or woken, while a repository in the set
  has not been read successfully since startup and was not read in that pass.
  The first tick pass selects every such repository; a woken pass before it
  reads only its delivered repositories and so creates nothing. The same holds
  for a repository just added to the installation: woken passes create
  nothing until the next tick pass reads it, at most one `interval` later.
- **Deliveries across a pause.** A delivery that arrives while passes are
  paused (rate limit, Lux outage) is read by the first pass after the pause,
  however long it was: a tick pass reads every repository delivered since the
  last pass as well as every marked one, and a pass that selected a delivered
  repository but did not read it hands it to the next.
- **Adoption of late Runs.** A submission whose outcome was unknown commits
  into Lux; the next tick pass's Lux listing makes its repository hot, so it
  is read and its job adopts the Run as before. An adopted Run counts against
  the caps from the moment Lux lists it, read or not.
- **Surplus deletion.** Deletes only idle runners whose repository was read
  this pass, compares them with that repository's demand, and terminates a Run
  only after its runner was deleted. No unread repository's Run is reached.
- **Finished Runs.** A live Run whose runner is gone is terminated only when
  its repository was read and lacked the runner, in two passes `idleGrace`
  apart. Its live Run makes the repository hot, so every tick pass reads it.
- **Orphans.** A cold repository has no Run Lux knows of. An orphaned runner
  there (offline, no live Run) is seen when the repository is next read,
  within `coldSweep`, which makes the repository hot; it is deleted
  `idleGrace` later, by a pass that reads it.
- **Caps.** The global and route caps count every live Run of the instance
  and every held slot, regardless of which repositories were read.

`GET /status` reports `demand` for the repositories read in the pass.

Run labels (no secrets ever go in labels). These six identify the Run;
ownership, matching, adoption and cleanup read only these:

| Label | Value |
|---|---|
| `lux-gha/managed` | instance |
| `lux-gha/repo` | numeric repository ID |
| `lux-gha/route` | route name |
| `lux-gha/job` | ID of the job that triggered provisioning |
| `lux-gha/gen` | generation for that job |
| `lux-gha/runner` | GitHub runner name |

`app` is always `lux-gha`, Lux's convention for which tool submitted a Run.
It is for cost breakdowns only (`label:app`); nothing reads it back.

The labels below describe the triggering job for people reading Lux (the console,
`lux ls`, costs grouped by label). They come from the workflow-runs and jobs
listings that found the job, so they cost no extra GitHub request. Nothing
reads them back, and they are not part of the `Idempotency-Key`: a replayed
key matches whatever they say. Control characters become spaces, and each
value is cut to 256 bytes on a UTF-8 boundary. Lux does not check Run labels
at submit; 256 bytes and its key rule are what it applies to server labels
and to cost label-filter keys, and `lux-gha` holds Run labels to them so they
stay queryable in costs. An empty value is left out.

| Label | Value |
|---|---|
| `lux-gha/repo-name` | `owner/repo` |
| `lux-gha/workflow` | workflow name |
| `lux-gha/job-name` | job name |
| `lux-gha/run-id` | workflow run ID |
| `lux-gha/run-attempt` | workflow run attempt |
| `lux-gha/branch` | head branch; `<fork owner>:<branch>` (e.g. `contributor:main`) for a `pull_request*` run that lists no pull request into this repository, as a pull request from a fork does |
| `lux-gha/pr` | pull request number, only when exactly one of the pull requests the run lists has this repository as its base (GitHub also lists pull requests into forks that share the run's head, and none for a pull request from a fork) |
| `lux-gha/sha` | head commit, first 12 characters |

The Run's name is `<repo> · <workflow> / <job> · <#PR or branch> run <run number>`,
for example `widgets · CI / build · #34 run 12`; the branch is written as in
`lux-gha/branch`. Unknown parts are left out
(`job <id>` stands in for a missing job name), and the name is cut to 128
bytes on a UTF-8 boundary with `…`, keeping the run number. The name is
cosmetic: nothing parses it.

A GitHub rate limit stops the pass where it is; no pass calls GitHub again
until the reported retry time. Other errors end the pass early or skip one
item; the next pass retries. The idle and orphan timers are in memory: a
restart re-derives everything else from Lux and GitHub and only restarts those
timers, which delays cleanup but never duplicates a Run. Jobs waiting for a
re-run after host loss are in memory too; the first pass after a restart
finds them again from Lux's ended Runs of the last 24 hours, and GitHub's
`run_attempt` tells which were re-run already.

`GET /status` returns the last pass as JSON: `time`, `durationMs`, `error`
(a category: `github_rate_limited`, `github`, `github_permission`, `lux`,
`lux_resume_policy_not_echoed`, `lux_run_mismatch`, `lux_size_refused`), `rateLimitedUntil`, `demand` and `live` per
route, `idle`, `busy`, and `provisioned`, `terminated`, `deleted`,
`checkRuns`, `forceCancelled` and `rerun` in that pass. It holds no credentials or
JIT configuration.

## When Lux stops a job

Lux can end a Run while its runner is running a job:

| Lux `stateReason` (each with `; resumePolicy never`) | Cause |
|---|---|
| `disk limit exceeded` | The job wrote more than the route's `diskGiB`, or its `size`'s disk. Lux measures disk every 15 s, so a job can overshoot by that much before the stop. |
| `timeout` | The Run reached the route's `timeout`. |
| `preempt: not resumed (...)`, `drain: not resumed (...)` | A spot preemption or a force-evicting drain took the host. |
| `host lost: missed heartbeats` | The host stopped answering. |
| `terminated` | An operator, or another client, terminated the Run. |

Without help, the runner dies mid-step without telling GitHub. The job
stays `in_progress` until its `timeout-minutes` (6 hours by default) plus
GitHub's 5-minute cancel grace, then shows "exceeded the maximum execution
time", with no log. Two layers prevent that.

**The runner cancels its own job.** On every stop except a lost host, Lux
sends SIGTERM to the Run's workload, then SIGKILL `workload.grace` later.
The entrypoint `exec`s `run.sh` with `RUNNER_MANUALLY_TRAP_SIG=1`, so
`run.sh` passes the SIGTERM to the runner listener as SIGINT. The
listener then cancels the running step, reports the job cancelled, and
uploads its log. Rootless dockerd runs in its own session and keeps serving
the step's containers while it is cancelled. Each Run is submitted with
`workload.grace` set to `reconcile.stopGrace` (default 60 s), which leaves
room for that. A preemption or force-evicting drain can shorten the grace
to half the time left before the host goes, and a lost host gives none.

**The bridge closes the job out.** Every pass already lists the jobs of
`in_progress` workflow runs. For each `in_progress` job whose
`runner_name` is one of this instance's runners
(`lux-gha-<instance>-<repo id>-<job id>-<gen>`) and has no live Run, the
bridge reads that runner's Runs (`lux-gha/managed` and `lux-gha/runner`
labels, any state; one Run per runner name). If the Run has ended:

- **Explain.** The bridge posts one check run named `lux-gha` on the job's
  commit, with conclusion `failure`, title `Lux stopped job "<job name>":
  <reason>`, and a summary with the route, Lux Run id, state and reason,
  exit code, finish time, and a hint. For `disk limit exceeded` the hint
  names the raw route with the next larger disk ("use a larger-disk profile
  such as `<route>` ... or reduce disk use"); on a size route it asks for a
  larger size. Its `external_id` is the Lux
  Run id. Before posting, the bridge lists the commit's `lux-gha` check
  runs, so neither a later pass nor a restarted bridge posts it twice. A
  Run that ended `succeeded` (the job finished, and GitHub's listing lags)
  gets nothing.
- **Unstick.** If the job is still `in_progress` `reconcile.stuckJobGrace`
  (default 2 m) after the Run's `finishedAt`, the runner did not report
  (SIGKILL, a lost host). The bridge then calls
  `POST /repos/{owner}/{repo}/actions/runs/{run_id}/force-cancel`, once per
  Run, and logs it at `WARN`. A `409` (the run has completed meanwhile)
  counts as done. **This cancels every unfinished job of that workflow
  run, not only the stuck one.** That is the trade-off for not waiting out
  the job's timeout. Queued siblings and siblings running on GitHub-hosted
  runners are cancelled too. It is never done while another listed job of the
  workflow run is running on a runner of this instance with a live Run, or
  when the pass did not list all of the run's jobs. The guard protects
  siblings that are running a job: a queued sibling has no runner assigned,
  so a fresh, idle Run provisioned for it does not hold the cancel back. That
  sibling is cancelled before it started, so no job work is lost, and its Run
  is reclaimed. A job that completes on its own
  within the grace is left alone.
- **Re-run after host loss.** When the reason says the host was taken away
  (it contains `preempt`, `drain` or `host lost`), the job did nothing
  wrong, so the bridge re-runs it. On a preemption or drain the runner
  usually cancels the job itself within `stopGrace`, before its Run ends,
  so the bridge does not rely on seeing the job `in_progress`. Every pass
  reads each Run of the instance that was live in the previous pass and is
  no longer live (`GET /v1/runs/{id}`, one request per Run end; Runs whose
  `lux-gha/run-attempt` is past the bound are not read). Instead, a pass
  lists the instance's Runs in `stopped`, `failed`, `terminated` or `lost`
  (`GET /v1/runs?label=lux-gha/managed=<instance>&state=…&sort=created`)
  created since the oldest Run live in the previous pass when it cannot rely
  on that set: the first pass after a start, after a pass that failed before
  this step or whose Lux read failed, and after a pass with a submission of
  unknown outcome or an unread repository, whose Run may start and end
  between two passes. Lux has no `finishedAt` filter, and a Run that ends
  later was live then or is created later. The first pass after a start
  reaches back 24 hours plus the longest route `timeout`. A listing past
  the Lux client's page limit (20,000 Runs) is skipped with a `WARN`, and
  host losses in it are not re-run. A Run that lost its host, ended within the
  last 24 hours and carries the `lux-gha/repo`, `lux-gha/job`,
  `lux-gha/run-id`, `lux-gha/run-attempt` and `lux-gha/runner` labels is a
  candidate. Once per candidate the bridge reads the job
  (`GET /repos/{owner}/{repo}/actions/jobs/{job_id}`) and keeps it only if
  its `runner_name` is the Run's runner, its `run_attempt` the Run's, and
  its conclusion `failure` or `cancelled`. A Run lost before its runner took
  the job fails this: the job ran on a later Run.

  The bridge waits while the workflow run is listed `queued` or
  `in_progress`, or while any of its Lux Runs is live (a sibling's runner
  can still be uploading its log after the run completed), then reads it once
  (`GET /repos/{owner}/{repo}/actions/runs/{run_id}`). It must be
  `completed` with conclusion `failure` or `cancelled`. For a `push` or
  `pull_request` run, if a newer run of the same workflow, branch and event
  exists
  (`GET /repos/{owner}/{repo}/actions/workflows/{workflow_id}/runs?branch=…&event=…&per_page=1`),
  and for `pull_request` from the same head repository (forks share branch
  names such as `main`), the run was superseded (a new push, or
  `concurrency: cancel-in-progress`) and is not re-run. Runs of other
  events (`workflow_dispatch` with other inputs, `schedule`) and runs with no
  head branch skip this check. The bridge
  then calls `POST /repos/{owner}/{repo}/actions/jobs/{job_id}/rerun`, which
  starts a new attempt that runs that job and the jobs that depend on it
  again, but not its siblings. When several jobs of the same attempt are due
  in the same pass, it calls `POST /repos/{owner}/{repo}/actions/runs/{run_id}/rerun-failed-jobs`
  once instead, since after one job re-run the others are no longer in the
  latest attempt. That also re-runs any other failed or cancelled job of the
  run. A sibling found only in a later pass, because reading its job failed
  in the pass that re-ran the others, is not re-run. Re-runs are logged at
  `WARN`. `disk limit exceeded`, `timeout`,
  `terminated` (including `terminated; host lost`, a host lost while the
  bridge or an operator terminated the Run) and any other reason are never
  re-run.

  A person pressing Cancel on a run that was not superseded looks the same
  to the bridge as the runner's own cancel, so the job is re-run. That is
  bounded by `hostLossReruns`, and the person can cancel the re-run.

  **The bound is per workflow run, not per job.** Every re-run, by anyone,
  increments the workflow run's `run_attempt`, and GitHub has no attempt
  count of a job apart from its run's. A job is re-run only if its
  `run_attempt` is at most `reconcile.hostLossReruns` (default 2, so at most
  3 attempts), and only while it belongs to the latest attempt. Before the
  call the bridge reads the workflow run again; if its `run_attempt` has
  moved past the job's, it was re-run already (by this bridge before a
  restart, by a second bridge, or by a person) and nothing is posted. The
  docs list `201` and `403` for the re-run; a `409` or `422` is treated as
  settled defensively, and a `403` other than GitHub's permission refusal
  ("Resource not accessible by integration") is logged at `WARN` and the
  job is given up. A job is also given up, with a `WARN`, after 5 passes
  whose reads or re-run failed, or 24 hours after its Run ended. The count
  needs no local state. A workflow run re-run by hand for another reason
  uses up attempts too.

  The check run's hint says what will happen, since it is posted when the job
  is seen stopped, before the re-run: "lux-gha re-runs the job automatically
  (attempt N of M)", or, past the bound, "re-run the job. Automatic re-runs
  are used up (this was attempt N; lux-gha re-runs up to attempt M)." With
  `hostLossReruns: 0` it says "Automatic re-runs are off." and the job is
  left failed, as before. The hint can promise a re-run that is then not
  made: the run succeeded, was superseded by a newer run, or is gone. The
  check run is not updated in that case.

`in_progress` workflow runs are listed before `queued` ones so a queued
backlog cannot hide stopped jobs. Reading their Lux Runs has its own per-pass
allowance of `maxRunners`, separate from `reconcile.maxJobsPerPass`: Lux reads
are not GitHub requests, and newly ended busy runners are bounded by the
previous pass's live Runs. An ended Run is cached, so later passes do not
read it again. When the allowance runs out, uncached jobs wait, but cached
jobs still get their explanation and cancellation. Cache pruning waits for
a complete pass; removed repositories are forgotten after `idleGrace`.
A `403` from any of these calls means the App lacks Checks or Actions write,
except on a re-run, where only GitHub's "Resource not accessible by
integration" does (any other `403` there gives the job up, see above). It is
logged once per pass and reported in `GET /status` as `github_permission`.
The pass goes on: provisioning and cleanup are not affected, the
runner's own cancellation still applies, and a job waiting for a re-run is
retried on a later pass; no further re-run is tried in that pass.

## Runner entrypoint contract

The Run's command is `["/usr/local/bin/lux-gha-runner"]`, or the route's
`command`. Lux places the JIT configuration as a file secret at
`/run/lux-gha/jitconfig` and the Run's environment has
`LUXGHA_JITCONFIG_FILE=/run/lux-gha/jitconfig`. (Lux reserves the `LUX_`
environment prefix for itself.) The entrypoint:

- reads the JIT configuration from the file named by `LUXGHA_JITCONFIG_FILE`;
- deletes nothing;
- `exec`s `./run.sh --jitconfig "<contents>"` from the runner directory as a
  non-root user, with `RUNNER_MANUALLY_TRAP_SIG=1` so that a SIGTERM cancels
  the running job ([When Lux stops a job](#when-lux-stops-a-job));
- exits when the runner exits, after at most one job.

With a [registry](#in-region-image-registry), the Run also has the identity token at
`/run/lux-gha/identity` (`LUXGHA_IDENTITY_FILE`) and `LUXGHA_RUNNER_ENDPOINT`, `LUXGHA_REGISTRY_HOST` and
`LUXGHA_REGISTRY_REGION`. The entrypoint then adds the credential helper for `LUXGHA_REGISTRY_HOST` and the
job-started hook; neither can stop the runner from starting.

`images/runner/entrypoint.sh` is a reference POSIX `sh` implementation. It
also starts rootless Docker first when the image has it, then loads the
image's BuildKit archive into it (a failed load only warns).

## Configuration

```json
{
  "listen": "127.0.0.1:8080",
  "instance": "prod",
  "maxRunners": 20,
  "reconcile": {"interval": "60s", "idleGrace": "2m", "maxJobsPerPass": 200, "maxGensPerJob": 5,
                "repositoryRefresh": "10m", "hotWindow": "2m", "coldSweep": "10m",
                "stopGrace": "60s", "stuckJobGrace": "2m", "hostLossReruns": 2},
  "github": {
    "appID": "Iv1.0123456789abcdef",
    "installationID": 12345678,
    "runnerGroupID": 1,
    "privateKeyFile": "/etc/lux-gha/github-app.pem",
    "webhookSecretFile": "/etc/lux-gha/webhook-secret"
  },
  "lux": {"url": "https://lux.example.com", "apiKeyFile": "/etc/lux-gha/lux-api-key"},
  "routes": [
    {"name": "default", "labels": ["self-hosted", "linux", "lux"], "pool": "gha",
     "image": "ghcr.io/acme/gha-runner@sha256:...", "cpus": 2, "memoryMiB": 4096, "diskGiB": 20,
     "timeout": "90m", "egressHosts": ["github.com", "*.actions.githubusercontent.com"],
     "nestedContainers": false, "maxRunners": 10},
    {"name": "m-2", "labels": ["lux-m-2-arm64"], "pool": "gha-shaped",
     "image": "ghcr.io/acme/gha-runner@sha256:...", "size": "m-2-arm64",
     "timeout": "90m", "egressHosts": ["github.com", "*.actions.githubusercontent.com"]}
  ]
}
```

- **`instance`**: required, 1-20 of `a-z`, `0-9` and `-`. It names this bridge in Run labels and runner names; two bridges sharing a GitHub repository or Lux tenant need different instances.
- **`maxRunners`**: required global cap on live Runs. A route's optional `maxRunners` caps that route too.
- **`reconcile`**: optional; the values shown are the defaults. `repositoryRefresh`, `hotWindow` and `coldSweep` apply only without `repositories` ([Repository discovery](#repository-discovery)): how often the installation's repositories are re-listed, how long a `queued` delivery keeps its repository read every tick pass while the runs API does not yet list its job (GitHub's lag between delivery and API), and the longest a cold repository goes unread. `coldSweep` should exceed `interval`; at or below it every repository is read every pass. `stopGrace` (15 s to 10 m) is each Run's Lux `workload.grace`, the time between SIGTERM and SIGKILL in which the runner cancels its job and uploads the log. Every stop of a runner Run, a surplus terminate included, can take that long. `stuckJobGrace` (30 s to 1 h) is how long a job may stay `in_progress` after its Run ended before its workflow run is force-cancelled ([When Lux stops a job](#when-lux-stops-a-job)). `hostLossReruns` (0 to 5; 0 turns it off) is the highest `run_attempt` of a workflow run whose job lost its Run's host and is still re-run automatically. It is counted per workflow run, not per job ([When Lux stops a job](#when-lux-stops-a-job)).
- **`command`** (route): optional, overrides the Run's command (default `["/usr/local/bin/lux-gha-runner"]`).
- **Resources** (route): either `size` or the raw `cpus`, `memoryMiB` and `diskGiB`, never both ([Sizes](#sizes)). Raw fields left out or `0` are not sent. On an unshaped pool, luxd's defaults fill them; on a shaped pool, the fields given are rounded up to the smallest size that covers all of them, and a route with none of them gets the pool's default size. `size` must be a name luxd can give a size: `<family>-<cpus>-<arch>`, the family a lowercase slug of `a-z`, `0-9` and `-` that starts and ends with a letter or digit, cpus `05` or a power of two, arch `arm64` or `amd64`, at most 63 characters.
- **Egress** (route): Lux Runs are default-deny. `egressHosts` sends one `network.egress` host rule per entry, and the Run reaches only those names. `unrestrictedEgress: true` sends `network: {"unrestricted": true}` instead, which switches Lux's egress filtering off (Lux's hard blocks, such as cloud metadata and the control plane, still apply). Setting both is rejected. A route with neither is valid, but its Runs have **no network at all**, so the runner cannot reach GitHub.
- **`appID`**: the JWT issuer. It can be the App's client ID (GitHub's recommendation) or its numeric app ID.
- **`runnerGroupID`**: required by `generate-jitconfig`. Read the repository's runner group ID from GitHub rather than assuming `1`.
- **`apiURL`**: optional. It defaults to `https://api.github.com` and must be HTTPS. Plain HTTP is accepted only for loopback test servers. The API host never comes from webhook payloads.
- **Routing**: a job is routed only when **every** label it requests is in exactly one route's label set. Label comparison is case-insensitive. Configuration is rejected if one route's label set contains another's, because a job could then match both.
- **Repositories**: runners are registered per repository. Organisation-level runners are not supported. `repositories` is optional. Omitted, as in the example, every repository of the installation is served ([Repository discovery](#repository-discovery)). Given, as `"repositories": ["acme/widgets"]`, only those are served and each is read every pass; an empty list is rejected.
- **Secrets**: these are read from the three files named above. A trailing newline is stripped. Secrets are never logged and are never part of the config value. Unknown config fields are rejected, so secrets can't be inlined by mistake.
- **`cache`**: optional; absent, nothing below runs ([Actions cache](#actions-cache)).
- **`registry`**: optional; see [In-region image registry](#in-region-image-registry). Without it nothing below applies: no runner listener, no identity secret, no registry env in Runs.

### Actions cache

```json
"cache": {
  "listen": "0.0.0.0:8090",
  "publicURL": "https://gha-cache.example.com",
  "s3": {"bucket": "cache-bucket", "region": "eu-west-1", "prefix": "gha-cache"},
  "signingKeyFile": "/etc/lux-gha/cache-signing-keys",
  "repositoryIDs": [123456789], "repositoryOwnerIDs": [],
  "issuer": "https://token.actions.githubusercontent.com",
  "upstreamResultsURL": "https://results-receiver.actions.githubusercontent.com",
  "downloadURLTTL": "10m", "ttl": "168h", "maxBytes": 0, "maxEntryBytes": 10737418240,
  "reapInterval": "1h"
}
```

With `cache` present, lux-gha serves the GitHub Actions cache service (v2,
the API `@actions/cache` 4 and later and `actions/cache@v4` use) on a second
listener; the webhook listener is unchanged. Runners use `publicURL` as
`ACTIONS_RESULTS_URL` (pointing runners at it is not part of lux-gha).

- **`listen`**, **`publicURL`**, **`s3.bucket`**, **`s3.region`** and
  **`signingKeyFile`** are required. `publicURL` is a scheme and host with no
  path, HTTPS except on loopback; signed upload URLs are built from it.
  `listen` must differ from the top-level `listen`.
- **`repositoryIDs`**, **`repositoryOwnerIDs`**: numeric GitHub IDs (JSON
  numbers) of the repositories served, or of their owners; at least one list
  must be non-empty. The issuer signs runtime tokens for every repository on
  GitHub, so without this any workflow anywhere could use the cache. A token
  is served when its `repository_id` is in `repositoryIDs`, or its
  `repository_owner_id` claim is present and in `repositoryOwnerIDs`; a token
  without that claim matches only by `repositoryIDs`. Every other token gets
  `403` (`permission_denied`), logged at most once a minute per repository.
  `repository_owner_id` is a claim of GitHub's OIDC tokens; it has not yet
  been confirmed in a runtime token (`ACTIONS_RUNTIME_TOKEN`). Decode one
  real runtime token and check the claim before relying on
  `repositoryOwnerIDs`; `repositoryIDs` does not depend on it.
- **`s3`**: credentials come only from the AWS default chain (an instance
  role, the environment, shared config); there is no key field. `prefix`
  (optional) holds every object. `endpoint` (optional) names an
  S3-compatible server, addressed path-style; HTTPS except on loopback.
- **`signingKeyFile`**: one key per line, each at least 32 bytes, used to
  HMAC-sign upload URLs. The first line signs; every line verifies. To rotate,
  add the new key as the first line and restart, then remove the old one
  after 6 hours (the upload URL lifetime).
- **`issuer`**: the runtime token issuer, discovered through
  `/.well-known/openid-configuration`; the default is GitHub's.
- **`upstreamResultsURL`**: every request that is not a cache request
  (artifacts, logs, other Results calls) is passed here unchanged, method,
  path, query, headers and body included, and the response is streamed
  back. The upstream is fixed; no request chooses it. Pass-through
  requests are limited in two ways. A request whose
  `Authorization: Bearer` token verifies (signature, issuer, `exp`, `nbf`),
  is a runtime token (`ac` and a numeric `repository_id`, as for cache
  calls) and names a repository this server serves counts against that
  job's 32 concurrent requests, keyed on a hash of the token's decoded
  payload. A token with whitespace inside it or non-canonical base64url
  never verifies, here or on cache calls, so one token is one job. Every
  other request (no token, one that does not verify, one longer than
  8 KiB, an OIDC ID token, another repository's token, or any token before
  the issuer has first been discovered) is still forwarded, but counts
  against the 32 concurrent requests of its TCP peer address;
  forwarded-for headers are ignored. The
  listener runs at most 4096 in total. Past any limit the request gets
  `503` with `Retry-After`. Jobs of served repositories behind one NAT,
  load balancer or TLS terminator are limited separately; everything else
  behind it shares that address's 32. Make the cache listener reachable
  only from the runners' network, not from the public internet: the
  address limits are per source address, so a client with many addresses
  gets many buckets, up to the 4096 ceiling that every runner shares.
- **Timeouts.** The cache listener has no overall body or response
  timeout, since a block upload or a proxied transfer can take minutes.
  Instead a Twirp request body must arrive within 30 s, and an upload or
  proxied body, and every response, must move at least 64 KiB (or finish)
  in each 60 s spent waiting on the client, or the connection is closed.
- **`downloadURLTTL`** (1 m to 7 d): lifetime of a presigned S3 download URL.
- **`ttl`** (at least 1 h): entries are deleted this long after they were
  saved.
- **`maxBytes`** (0: no cap): the total for entries and uploads in
  progress, split into two budgets the reaper enforces separately (see
  Expiry): 10 % for uploads in progress, 90 % for entries. Size it so that
  the upload budget is at least the number of saves you expect at once
  times a typical archive size; saves beyond it fail when a pass runs
  during them (a 10 % budget of a 100 GiB cap holds one 10 GiB upload, or
  ten 1 GiB ones).
- **`maxEntryBytes`** (1 B to 5 TiB): the largest archive accepted; default
  10 GiB, or 90 % of `maxBytes` if that is smaller. It may never exceed
  90 % of `maxBytes` (the entry budget), so the cap never deletes an entry
  just saved. It may exceed the upload budget: the newest upload is never
  trimmed, so it always completes.
- **`reapInterval`** (at least 1 m): how often the reaper runs.

**Authentication.** Every cache call carries the job's runtime token
(`ACTIONS_RUNTIME_TOKEN`), a JWT the issuer signs. Its signature (RS256,
against the issuer's JWKS), exact issuer, `exp` and `nbf` are checked; there
is no setting that turns this off. The audience is not checked: the runtime
token's audience names GitHub's service and the run, not this server, so no
stable value exists to require. Two claims are used. `repository_id`
partitions every entry, so a job never reads or writes another repository's
cache. `ac` is a JSON string holding `[{"Scope": "<ref>", "Permission": n}]`,
read 1 and write 2: a job saves only to its one writable scope (its own ref)
and restores from its readable scopes, its own first and then the others in
token order (for a pull request, the base branch). A missing or malformed
claim is `401`. A save with no writable scope gets `ok: false` with a
message starting `cache write denied:`, and a restore with no readable scope
a `403` starting `cache read denied:`; the client reports either as a
warning and carries on.

**Matching.** Within each scope in that order: the exact key, then the
newest entry whose key starts with the key, then the same for each restore
key in order. Only entries of the requested version (the client's hash of
paths and compression) match. A miss is `ok: false`, not an error.

**Saving.** `CreateCacheEntry` returns `ok: false` when the key and version
already exist in the job's scope, and otherwise a signed upload URL on this
server, bound by HMAC to the repository, scope, key, version and runtime
token, valid for 6 hours. The client uploads to it with the Azure Blob SDK;
lux-gha implements the three requests that client sends: one `Put Blob` up
to 128 MiB, or `Put Block` per 64 MiB block followed by `Put Block List`.
Every block but the last must be at least 5 MiB, S3's multipart minimum;
`@actions/cache` meets that, but BuildKit's `type=gha` cache, which uploads
1 MiB blocks, is not supported. A `Put Blob` is at most 5000 MiB and a
block at most 4000 MiB (Azure's limits, and never above `maxEntryBytes`),
and an upload holds at most 10,000 blocks (`400 BlockCountExceedsLimit`).
A job that saves the same key again reuses its upload: a `Put Blob`
discards blocks an earlier attempt staged, and a blob an earlier attempt
staged counts with the blocks toward `maxEntryBytes`. Blocks are streamed
to S3, never held in memory.
`FinalizeCacheEntryUpload` checks the uploaded size against `size_bytes`
and only then makes the entry visible, as one server-side multipart copy
completed with `If-None-Match: *`. A finalize after another job's entry of
the same key and version exists gets `ok: false`. Of two finalizes in
flight together, exactly one wins only on a store that serialises
conditional completes, which AWS S3 documents and which has not been
verified here; a store that does not (versitygw v1.7.0) lets the last one
win. Either way a reader sees one whole archive.

**Restoring.** `GetCacheEntryDownloadURL` returns a presigned S3 `GET` URL;
the client downloads it with plain HTTP, so runners need egress to the S3
endpoint as well as to `publicURL`.

**Storage.** S3 is the only state; there is no database. Under `prefix`:

| Objects | Holds |
|---|---|
| `v1/<repository_id>/<sha256(scope)>/<version>/<key>` | finalized entries. The key is escaped (bytes outside `A-Za-z0-9._-`, and a leading `.`, as `%XX`) so that a key prefix is an S3 listing prefix. |
| `staging/<reservation>/…` | uploads not yet finalized; never read by a restore. |

**Expiry.** The reaper runs at start and every `reapInterval`, one pass at a
time and each bounded by `reapInterval`, and logs one `cache reaper pass`
line with the entry count and bytes, the staging bytes, and what it deleted.
It deletes entries older than `ttl`, staging uploads untouched for 24 hours,
and multipart uploads under `prefix` started more than 24 hours ago (without
a `prefix`, every multipart upload in the bucket: give the cache its own
bucket or a prefix). With `maxBytes` set it then enforces two budgets
separately:

- **Uploads in progress** (staging), 10 % of `maxBytes`: while staging
  exceeds it, whole staging uploads are deleted, ordered by their first
  write, oldest first, including uploads still running, but never the one
  started last; writing to an upload again does not move it in this order.
  A deleted upload's `FinalizeCacheEntryUpload` gets `ok: false` ("… the
  uploaded data has expired; save again"), and a block upload deleted
  before its `Put Block List` fails there (`400 InvalidBlockList`); the
  client reports either as a failed save.
- **Entries**, the other 90 %: while entries exceed it, the oldest are
  deleted. Staging never counts here, so uploads in progress never push out
  an entry, and since `maxEntryBytes` is at most this budget, an entry just
  saved is never deleted for the cap.

Every entry and every staging object (each block, blob and manifest)
counts as at least 1 MiB, so after a pass the reaper lists and holds about
`maxBytes` / 1 MiB objects, plus the newest upload's blocks (at most
10,000). A failed delete is
retried next pass. As a backstop, give the bucket a lifecycle rule that
aborts incomplete multipart uploads after a day or more.

**Bucket permissions.** `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject`,
`s3:ListBucket`, `s3:ListBucketMultipartUploads` and
`s3:AbortMultipartUpload` on the bucket and `prefix`.

### Sizes

A Lux pool with a shape (Lux `v0.1.24` or later) sizes every Run as a
power-of-two fraction of one nominal host, named
`<family>-<cpus>-<arch>`: for an 8-CPU `m` arm64 host, `m-05-arm64`
(half a CPU), `m-1-arm64`, `m-2-arm64`, `m-4-arm64` and `m-8-arm64`.
`lux pools sizes <pool>` lists a pool's sizes, and `GET /v1/pools` lists them
as each pool's `sizes`.

A route with `size` sends `resources.size` and no cpus, memory or disk; the
Run gets that size's cpus, memory and disk. By convention its runner label is
the size prefixed with `lux-`, so a job selects a size with
`runs-on: lux-<family>-<cpus>-<arch>`:

```yaml
jobs:
  build:
    runs-on: lux-m-2-arm64
```

```json
{"name": "m-2", "labels": ["lux-m-2-arm64"], "pool": "gha-shaped",
 "image": "ghcr.io/acme/gha-runner@sha256:...", "size": "m-2-arm64", "timeout": "90m",
 "egressHosts": ["github.com", "*.actions.githubusercontent.com"]}
```

The bridge does not derive routes from labels: each size a job may ask for
needs its own route. The label is only a convention, and routes with sizes
go through the same routing and ambiguity checks as any other.

A route with raw `cpus`, `memoryMiB` and `diskGiB` works on any pool. On an
unshaped pool the Run gets them as given. On a shaped pool Lux rounds them up
to the smallest size covering every one given, so the Run can get more than
the route asks for (on the pool above, `"cpus": 3` gets `m-4-arm64`).

**When Lux refuses a size.** Lux answers `422` when the pool has no size of
that name (`unknown_size`), the pool has no shape (`size_unsupported`), the
raw resources exceed the largest size (`size_too_large`), or a size comes with
cpus, memory or disk (`size_conflict`). Configuration validation rules out
the last one. The same submission would be refused every time, so the bridge
does not resubmit it:

- it deletes the runner it registered for the job, and `GET /status` reports
  `lux_size_refused` for that pass;
- it posts a `lux-gha` check run on the job's commit with conclusion
  `failure`, title `Lux refused job "<job name>": <code>`, the route, pool,
  size and Lux error code, and what to change. Its `external_id` is
  `job-<job id>`, so it is posted once;
- it force-cancels the job's workflow run, under the same conditions as for
  [a stopped job](#when-lux-stops-a-job): never while another listed job of
  the run is running on a runner of this instance with a live Run, nor when
  the pass did not list all of the run's jobs. A queued sibling's fresh, idle
  Run does not hold it back; that sibling is cancelled before it started.

The job is not submitted again while it stays queued. A restarted bridge
submits it once more and is refused once more. Without Checks or Actions
write, the check run or the force-cancel is skipped, as for stopped jobs.

## In-region image registry

With a `registry` block, jobs on lux-gha runners can pull and push container images (an image cache, a
buildx layer cache) in an ECR registry in the Lux workers' own region, with no credentials in the workflow.
The registry is shared: its paths are named like GHCR's, `<owner>/<repo>/…`, and lux-gha is one credential
broker for it. The bridge decides, for the job a runner is **executing**, what that job may do, and mints
short-lived AWS credentials scoped to it. Off Lux, or without the block, nothing changes: the variables below
are unset.

```json
"registry": {
  "roleArn": "arn:aws:iam::123456789012:role/lux-gha-registry",
  "region": "eu-west-1",
  "host": "123456789012.dkr.ecr.eu-west-1.amazonaws.com",
  "trustedRefs": ["refs/heads/release/*", "refs/tags/v*"],
  "runnerListen": "127.0.0.1:8081",
  "runnerEndpoint": "https://gha.example.com",
  "identityKeyFile": "/etc/lux-gha/registry-identity-key"
}
```

- **`roleArn`**, **`region`**, **`host`**: the registry role the bridge assumes, and the registry
  (`<account>.dkr.ecr.<region>.amazonaws.com`, in `region`).
- **`trustedRefs`** (default as shown): `refs/heads/` or `refs/tags/` globs (`path.Match`; `*` does not
  cross `/`). The repository's default branch, read from GitHub, is always trusted in addition, whatever the
  list holds (`[]` trusts the default branch only).
- **`runnerListen`**: the address of the **runner listener**, a plain-HTTP server separate from `listen` that
  serves only the two runner routes below. Keep it on loopback: the bridge logs a warning at startup when it is
  not, because the listener carries bearer tokens and AWS credentials in clear text.
- **`runnerEndpoint`**: the URL Runs use to reach the listener (scheme, host and port only). Normally an
  `https://` public hostname served by a tunnel to `runnerListen` ([Network](#network)). It must be `https`
  unless its host is loopback: the token and the returned AWS secret key cross it.
- **`identityKeyFile`**: at least 32 random bytes (`head -c 32 /dev/urandom | base64 > …`), read like the
  other secret files; a trailing newline is stripped. There is no environment variable for it. Changing it
  invalidates the tokens of Runs already submitted.
- AWS credentials come from the bridge's default chain (on EC2, its instance role).

### What a Run gets

Each Run gets a second Lux file secret, `/run/lux-gha/identity`, holding
`v1.<runner name>.<base64url(HMAC-SHA256(identity key, "v1." + runner name))>`, and the env
`LUXGHA_IDENTITY_FILE`, `LUXGHA_RUNNER_ENDPOINT`, `LUXGHA_REGISTRY_HOST` and `LUXGHA_REGISTRY_REGION`. The
token is a function of the runner name, so the `Idempotency-Key` and its replay are unchanged. It is never in
labels, the command, env or logs. On a route with `egressHosts` (default-deny), the bridge adds one host
egress rule for `runnerEndpoint`'s hostname, so the Run can reach the tunnel. An `unrestrictedEgress` route
gets none, and neither does an endpoint given as an IP address.

The runner image's entrypoint then names `docker-credential-lux-gha` in the runner user's
`~/.docker/config.json` `credHelpers` for `host` (other keys are kept), which also covers the runner's own
`services:` and `container:` pulls, and sets `ACTIONS_RUNNER_HOOK_JOB_STARTED` to a hook that runs before
the job's first step ([runner image README](images/runner/README.md#in-region-image-registry)).

### Runner routes

Both take `Authorization: Bearer <token>` and answer only after these checks, each from Lux or GitHub, never
from the request: the HMAC (constant time) → a live Lux Run of this instance with that runner name → the job
GitHub shows `in_progress` on that runner ([Runner attribution](#runner-attribution); the jobs a pass
already lists, a signed `in_progress` delivery naming the runner, or a search of the repository's
`in_progress` runs, each confirmed with `GET /repos/{owner}/{repo}/actions/jobs/{job_id}`) → its workflow
run → the trust level below. The search runs at most every 5 s per repository, shared by every runner
waiting on it, reads at most 20 pages of runs and jobs, and publishes every `in_progress` job it sees on
this instance's runners, so a sibling runner then needs one job read. It runs for at most 8 s, within the
hook's 10 s, and does not stop when the request that started it does; a search cut short answers `409`,
which the hook retries. A delivery's candidate is kept until a
pass lists its job completed or on another runner, the runner's Run ends, or 10 minutes pass. While GitHub
rate-limits the installation, the listener answers `503` without calling it; a rate limit it meets also
pauses the reconciler's passes, and the other way round.

- `POST /runner/v1/registry` → `200 {"registry", "read", "push", "trust", "expiresAt"}`: `read` is
  `<host>/<owner>/<repo>`; `push` is the same for `trusted`, `<host>/<owner>/<repo>/pr` for `untrusted` and
  `""` for `read-only`. `<owner>` and `<repo>` are the GitHub names of the executing job's repository,
  lower-cased, as `GET /repos` reports them now (cached 10 minutes), so a renamed repository gets its new
  path. A name that is not an ECR path component after lower-casing (runs of `a-z`, `0-9` joined by single
  `.`, `_` or `-`; for example `.github`) gets `401` on both routes and one warning in the log.
- `POST /runner/v1/aws-credentials` → `200` with AWS `credential_process` JSON
  (`Version`, `AccessKeyId`, `SecretAccessKey`, `SessionToken`, `Expiration`).

| Status | When |
|---|---|
| `401` | Missing or bad token, a runner name of another instance, no live Run, the job has completed, or the repository's name is not a registry path |
| `409` | The Run is live but GitHub does not show a job `in_progress` on the runner yet |
| `503` | Lux, GitHub or STS failed, GitHub is rate-limiting the installation, or the token exceeded its rate limit (a bucket of 30, refilled at one request per 2 s, charged only by a request that calls Lux, GitHub or STS; answers from the runner's cache are free) |
| `204` | Reserved for a registry that is not configured; the listener does not run then |

The resolved job is kept per runner (a JIT runner runs one job) and re-read at most once a minute. A signed
`workflow_job` `completed` delivery naming the runner, from the runner's own repository, refuses it at once;
without one, a completed job is refused within a minute. Credentials are an STS `AssumeRole` of `roleArn` with `DurationSeconds` 900 (15
minutes, STS's minimum), session tags `lux-owner=<owner>`, `lux-repo=<repo>` and `lux-trust=<level>`, the
[session policy](#session-policy) below and session name `lux-gha-<repo id>-<job id>-<attempt>` (at most 64
characters of `[\w+=,.@-]`), cached per runner until 5 minutes before they expire; the credential helper asks
again on demand. They are never logged; each resolution and mint logs one line with the runner, job, event
and trust level.

**Lifetime after the job.** Refusing the runner stops new credentials, not ones already handed out. A
process a step leaves running can keep using the last STS session for up to 15 minutes. An ECR
authorization token that `docker-credential-ecr-login` obtained with it (`ecr:GetAuthorizationToken`) is
valid for 12 hours, whatever the session's lifetime, and is held by whatever logged in with it; ECR offers
no way to revoke it.

### Trust levels

Decided for the job the runner is executing, from its workflow run's `event`, `head_branch`,
`head_sha` and `head_repository`; the first row that matches wins.

| # | Level | Workflow run | Pull `<owner>/<repo>/*` | Push |
|---|---|---|---|---|
| 1 | `read-only` | any event whose `head_repository` is missing or is another repository (`head_repository.id` ≠ the repository's): a pull request, review or review comment from a fork, `pull_request_target` from a fork, a `workflow_run` triggered from a fork | yes | nothing |
| 2 | `untrusted` | `push`, `workflow_dispatch`, `schedule`, `release` or `workflow_run` whose `head_branch` is both a tag at `head_sha` and a branch containing `head_sha`, or neither | yes | `<owner>/<repo>/pr/*` |
| 3 | `trusted` | `push`, `workflow_dispatch`, `schedule`, `release` or `workflow_run` on `refs/heads/<default branch>` (always) or on a ref matching `trustedRefs` | yes, except `…/pr/*` | `<owner>/<repo>/*` |
| 4 | `untrusted` | anything else: a same-repository pull request (from any branch, `release/*` included), same-repository `pull_request_target`, `merge_group`, `issue_comment`, a push or dispatch on another branch, a tag named like a trusted branch | yes | `<owner>/<repo>/pr/*` |
| | none (`401`/`409`/`503`) | the job cannot be resolved, the Run is not live, the job is not `in_progress` on this runner, or an upstream is down | | |

The API reports a tag push, a release or a dispatch on a tag by the tag's name in `head_branch`, like a
branch. For the events of rows 2 and 3 the bridge resolves the name with
`GET /repos/{owner}/{repo}/commits/refs/tags/{name}` (which peels an annotated tag): a tag of that name at
the run's `head_sha` makes the ref `refs/tags/<name>`.
`GET /repos/{owner}/{repo}/compare/{head_sha}...refs/heads/{name}` makes it `refs/heads/<name>` only when the
branch contains `head_sha` (status `identical` or `ahead`); `behind`, `diverged`, a missing branch or an
unrelated history (`404`, `422`) do not. A branch name alone is not enough: a tag named `main` pushed at
an arbitrary commit and deleted or moved before the job starts would otherwise resolve to the default
branch. A commit that is on the branch reached it through the branch's protection, so a re-run of an
older commit of `main` stays trusted. A name that is both is untrusted, and so is one that is neither (a
deleted branch). A tag named `main` therefore resolves to `refs/tags/main`, which no trusted pattern
matches. Any other GitHub answer is `503`, never a guess. The answer is
cached per repository, name and commit for 10 minutes. A reusable workflow's jobs report the caller's
event. "Trusted" relies on branch and tag protection for the trusted refs: anyone who can push a
`release/*` branch or a `v*` tag gets trusted credentials.

### IAM the operator provides

Only repositories of the GitHub App installation get runners, so only they get credentials.

- **Bridge principal** (the instance role): `sts:AssumeRole` and `sts:TagSession` on the registry role.
- **Registry role trust policy**: allow the bridge principal `sts:AssumeRole` and `sts:TagSession`, requiring
  `aws:RequestTag/lux-owner`, `aws:RequestTag/lux-repo`, `aws:RequestTag/lux-trust` in `trusted`,
  `untrusted`, `read-only`, and `sts:RoleSessionName` like `lux-gha-*`. The bridge asks for 15-minute
  sessions; role chaining would cap them at 1 hour.
- **Registry role permissions**, on `repository/${aws:PrincipalTag/lux-owner}/${aws:PrincipalTag/lux-repo}/*`
  and keyed on `aws:PrincipalTag/lux-trust`:
  - `ecr:GetAuthorizationToken` on `*`;
  - pull (`BatchGetImage`, `GetDownloadUrlForLayer`, `BatchCheckLayerAvailability`) on `…/*` for every level;
  - push (`InitiateLayerUpload`, `UploadLayerPart`, `CompleteLayerUpload`, `PutImage`) on `…/pr/*` for
    `untrusted` and `trusted`, and on `…/*` for `trusted` only;
  - an explicit **Deny** of `BatchGetImage` and `GetDownloadUrlForLayer` on `…/pr/*` when `lux-trust` is
    `trusted`, so a pull request cannot poison a trusted build's layer cache;
  - no `ecr:CreateRepository`, nothing on other owners or repositories, no deletes, no lifecycle or policy
    changes.
- **Repositories are created on push by ECR repository creation templates**, one per owner prefix (a
  template's prefix `acme` covers `acme/…`), applied for `CREATE_ON_PUSH`. Without a template for its owner,
  a push to a repository that does not exist fails. The template carries the lifecycle policy, by tag:
  untagged images expire after 1 day; every other tag except `v*` and `release-*` expires 14 days after it
  was pushed: pull-request, cache and content-hash tags, and `latest` too, so a `latest` not pushed again
  within 14 days goes; `v*` and `release-*` are kept. Templates apply only when ECR creates a repository, so
  existing repositories keep their own settings.
- **A rename frees the old path.** Paths are the repository's current owner/name, not its id. After a
  rename or transfer, the repository's new jobs use the new path (within the 10-minute name cache), and the
  images under the old path stay. Another installed repository that later takes the old owner/name gets
  that path, with the images left there, including any that the old repository's trusted jobs pushed.
  Delete or move the old path's images when a repository is renamed and its name may be reused.

### Session policy

The bridge also sends an inline session policy with every `AssumeRole`. A session can do only what both the
role policy and the session policy allow, so the role policy stays the real limit, and the session policy
keeps a session to its own job's paths even if the role policy is ever broader than above. It names the
paths literally (partition from `roleArn`, account from `host`). For `acme/widgets`, `trusted`:

```json
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["ecr:GetAuthorizationToken"],"Resource":"*"},
 {"Effect":"Allow","Action":["ecr:BatchCheckLayerAvailability","ecr:BatchGetImage","ecr:GetDownloadUrlForLayer"],
  "Resource":"arn:aws:ecr:eu-west-1:123456789012:repository/acme/widgets/*"},
 {"Effect":"Allow","Action":["ecr:CompleteLayerUpload","ecr:InitiateLayerUpload","ecr:PutImage","ecr:UploadLayerPart"],
  "Resource":"arn:aws:ecr:eu-west-1:123456789012:repository/acme/widgets/*"},
 {"Effect":"Deny","Action":["ecr:BatchGetImage","ecr:GetDownloadUrlForLayer"],
  "Resource":"arn:aws:ecr:eu-west-1:123456789012:repository/acme/widgets/pr/*"}]}
```

`untrusted` has the first two statements and the push statement on `…/acme/widgets/pr/*`, with no Deny;
`read-only` has the first two only. The bridge sends it without whitespace: about 1 KB for the longest
GitHub names, under STS's 2048-character limit, which the bridge checks before calling STS.

### Network

Runs cannot reach the Lux control host: Lux hard-blocks the control plane for every Run, unrestricted
egress included ([Configuration](#configuration)), whatever the security groups allow. A bridge on the
control host is therefore reached through a tunnel, never by a private address:

- `runnerListen` is on loopback, for example `127.0.0.1:8081`;
- a tunnel (for example `cloudflared`, which may already carry the webhook's public hostname) forwards
  requests whose path starts with `/runner/v1/` on a public `https://` hostname to `runnerListen`;
- `runnerEndpoint` is that hostname, for example `https://gha.example.com`. The Run speaks TLS to the
  tunnel's edge; only the hop from the tunnel's local connector to `runnerListen` is plain HTTP, on loopback.

No security group rule is needed. A default-deny route gets the host egress rule for the hostname (above).
Check from a Run before relying on it:
`curl -sS -o /dev/null -w '%{http_code}' -X POST "$LUXGHA_RUNNER_ENDPOINT/runner/v1/registry"` should print
`401`.

### Fail-open

The registry is an optimisation. The hook always exits 0 (a failing job-started hook fails the job): on any
answer but `200`, after retrying `409` for up to 20 s, or when nothing listens, it writes nothing and prints
one line, `lux-gha registry: unavailable (HTTP <code>); continuing without it`. On `200` it prints
`lux-gha registry: trust=<level>`. A pull through the credential helper that fails later (the bridge went
down mid-job) fails that `docker` command as any registry error would.

### In a workflow

After the hook, a job sees `LUX_REGISTRY` (where it may pull) and, unless it is `read-only`,
`LUX_REGISTRY_PUSH` (where it may push). No `docker login`: the credential helper answers for the host.

```yaml
- run: |
    cache=${LUX_REGISTRY:-ghcr.io/example-org/cache}
    docker pull "${LUX_REGISTRY_PUSH:-$cache}/app:deps" || docker pull "$cache/app:deps" || true
    if [ -n "${LUX_REGISTRY_PUSH:-}" ]; then
      docker tag app:deps "$LUX_REGISTRY_PUSH/app:deps" && docker push "$LUX_REGISTRY_PUSH/app:deps"
    fi
```

buildx needs `image-manifest=true,oci-mediatypes=true` in `cache-to` for an ECR cache.

## GitHub App permissions

Repository permissions, and nothing else:

| Permission | Access | Used for |
|---|---|---|
| Administration | Read and write | `generate-jitconfig`, listing and deleting repository runners |
| Actions | Read and write | `workflow_job` webhook events, reading workflow jobs and runs (also the registry's runner attribution); force-cancelling a workflow run whose job's Lux Run was stopped, and re-running a job whose Run's host was lost |
| Checks | Read and write | A check run explaining why Lux stopped a job, and listing it so it is posted once |
| Metadata | Read-only | Granted to every App; `GET /installation/repositories`; with a registry, the default branch |
| Contents | Read-only | With a registry only: resolving a run's ref (`GET /repos/{owner}/{repo}/commits/refs/tags/{name}` and `GET /repos/{owner}/{repo}/compare/{head_sha}...refs/heads/{name}`, see [Trust levels](#trust-levels)) |

Upgrading an existing App to use the registry: add **Contents: Read** and accept it on each installation before deploying; until then the runner listener answers `503` to every trust decision, and logs the refusal once per repository as `github_permission`.

Subscribe to the **Workflow job** webhook event. With a registry, its `in_progress` deliveries let the runner listener find a runner's job before the next pass lists it.

Actions and Checks write serve only [When Lux stops a job](#when-lux-stops-a-job).
With Actions read-only and no Checks permission, the bridge still
provisions and cleans up as before, and the runner still cancels its own
job on a stop. Only the check run, the force-cancel and the re-run are skipped, and
`GET /status` reports `github_permission`.

Without `repositories`, install the App with **Only select repositories**:
adding a repository to the installation is what enables the bridge for it,
and removing it disables it. No further permission or subscription is
needed: the repository listing uses the installation token and Metadata, and
GitHub sends `installation` and `installation_repositories` events to every
App.

## Webhook semantics

- **Signature**: `X-Hub-Signature-256` is checked over the exact raw body with a constant-time comparison. This happens before any parsing.
- **Ignored deliveries**: unsigned or invalid deliveries get `401`. Signed `ping`s, other events, unknown actions, and other installations or repositories get `204`, and no callback is made. Without `repositories`, a `workflow_job` of this installation from a repository outside the discovered set gets `202` and only requests a refresh of the set; `installation` and `installation_repositories` deliveries of this installation with the actions listed in [Repository discovery](#repository-discovery) get `202` and do the same.
- **Actions handled**: `queued`, `waiting`, `in_progress` and `completed`. Cancellation is `completed` with `conclusion: cancelled`; it is not a separate action.
- **Callback budget**: the callback runs inline and must be a fast signal, not provisioning, because GitHub expects a response within 10 seconds. The callback gets an 8 s context.
- **Callback failure**: returns `503`. **GitHub does not automatically redeliver failed or missed deliveries.** Recovery comes from the periodic reconciliation pass against the GitHub API, not from redelivery. There is no in-memory queue that claims durability.

## Lux compatibility

Lux has no `/features` or `/version` endpoint, so at startup `lux-gha` reads `GET /openapi.json`. It refuses to start unless both of these hold:

- `info.version` is a release tag `vX.Y.Z` of at least `v0.1.14`. `dev`, `git describe` builds and pre-releases are refused.
- The `RunSpec` schema declares `resumePolicy`.

The schema has no enum, so the version check is what proves `resumePolicy: never` is supported.

Each submission sends `resumePolicy: "never"`. The client rejects a response that doesn't echo it back.

Each submission also sends `workload.grace` (`reconcile.stopGrace`). Lux
decodes RunSpecs strictly, and `workload.grace` is in its RunSpec from
`v0.1.14`, so the minimum version is unchanged.

A route with `size` sends `resources.size`, which Lux accepts from
`v0.1.24`. The minimum version stays `v0.1.14` for raw routes, but if any
route sets `size` and the `Resources` schema does not declare `size`, the
bridge refuses to start: `lux compatibility: route "<name>" asks for a size;
luxd <version> predates sizes (v0.1.24)`.

A `never` Run ends `terminated` even when it succeeds. The outcome is in `exitCode` and `stateReason`, so `terminated` alone is not treated as failure.

There is no operator override for unverifiable Lux builds. Deploy a tagged release.

The JIT config is delivered only as a Lux file secret. It never appears in labels, env or the submitted Lux command. At runtime the entrypoint passes it as `./run.sh --jitconfig` argv, which is GitHub's documented way to start a JIT runner. GitHub documents no lifetime for a JIT config, so the bridge assumes none.

## Development

```sh
make lint              # gofmt check, go vet ./..., scripts/check-license.sh
make test              # go test -race -count=1 ./...
make check             # lint, then test
make entrypoint-test   # images/runner/entrypoint_test.sh as uid 1000 in the runner's pinned ubuntu base plus jq and curl (Docker)
make image             # the runner image for the host's architecture, as lux-gha-runner:dev (IMAGE_TAG=..., PLATFORM=linux/amd64)
make dist VERSION=vX.Y.Z  # release tarballs in dist/ (below); needs GNU tar (gtar on macOS)
```

The cache's S3 and end-to-end tests are left out of `make test`. They build
with `-tags s3integration` and need an S3-compatible server, and for the
end-to-end tests `node`, `zstd` and GNU tar plus a directory holding
`@actions/cache` 6 (`npm --prefix <dir> install @actions/cache@6`):

```sh
docker run -d --name cache-s3 -p 127.0.0.1:59240:7070 -e ROOT_ACCESS_KEY=testuser -e ROOT_SECRET_KEY=testsecret \
  versity/versitygw:v1.7.0 --port :7070 --iam-dir /tmp posix /tmp
LUXGHA_CACHE_S3_ENDPOINT=http://127.0.0.1:59240 LUXGHA_CACHE_S3_BUCKET=cache-bucket \
  AWS_ACCESS_KEY_ID=testuser AWS_SECRET_ACCESS_KEY=testsecret AWS_REGION=us-east-1 \
  LUXGHA_CACHE_NODE_DIR=<dir> LUXGHA_CACHE_S3_CONDITIONAL=1 \
  go test -tags s3integration -count=1 ./cache/
```

`LUXGHA_CACHE_S3_CONDITIONAL=1` fails the run unless the server refuses
`CompleteMultipartUpload` with `If-None-Match: *` against an object that
already exists. versitygw v1.7.0 does, so a finalize that comes after
another job's finalized entry gets `ok: false` there.

versitygw v1.7.0 does not serialise two such completes in flight together:
both succeed, and the last to finish is the entry. The command above
therefore does not assert that exactly one of several concurrent saves
wins; it asserts that at least one does and that every restore sees one
whole archive, which holds on any server. The concurrent first-writer-wins
guarantee is targeted at AWS S3, which documents conditional writes as
admitting one of concurrent writers; it has not been run against AWS here.
To assert it against a server that should enforce it, add
`LUXGHA_CACHE_S3_CONCURRENT_CONDITIONAL=1`; against versitygw that run
fails intermittently.

`make test` covers the S3 requests themselves (conditional complete, part
order, copy-source escaping, error mapping, abort and cancellation) against
an in-process fake S3 (`cache/s3store_test.go`).

CI (`.github/workflows/ci.yml`) runs on pull requests and pushes to `main`: `make lint`, one
`go test -race -v` run that fails if any test skipped, and `make dist VERSION=v0.0.0`; `shellcheck`, `sh -n`
and `make entrypoint-test`; and the runner image built and smoke-tested with `images/runner/smoke.sh`
(unprivileged) natively for linux/arm64 on `ubuntu-24.04-arm` and for linux/amd64 on `ubuntu-24.04`.
`ubuntu-24.04-arm` is a hosted runner GitHub provides only to public repositories: while the repository
is private the arm64 jobs get no runner.

## Releases

Pushing a tag `vX.Y.Z` runs `.github/workflows/release.yml`:

1. `make test`, then `make dist` with `VERSION` set to the tag, published as a GitHub release with generated notes:
   - `lux-gha_vX.Y.Z_linux_arm64.tar.gz` and `lux-gha_vX.Y.Z_linux_amd64.tar.gz`, each holding
     `bin/lux-gha` (static, `lux-gha --version` prints `vX.Y.Z`) and `share/lux-gha/LICENSE`;
   - `SHA256SUMS` for both.

   `make dist` fails unless each binary carries the tag as its version. The tarballs are reproducible:
   rebuilding the same tag with the same Go toolchain and GNU gzip gives byte-identical files.
2. The runner image for **linux/arm64 and linux/amd64**. One job per architecture, on a hosted runner of
   that architecture, builds the image and smoke-tests it (`smoke.sh`). It then pushes it to
   `ghcr.io/<owner>/lux-gha-runner` by digest, with no tag. The push rebuilds from the same builder's cache,
   and the job fails unless the pushed image's platform, layers and runtime config fields equal the tested
   image's (`images/runner/push-by-digest.sh`; [the fields compared](images/runner/README.md#build-and-smoke-test)).
   A final job publishes `ghcr.io/<owner>/lux-gha-runner:vX.Y.Z` as an image index over the two digests, after
   checking the index it would push (`images/runner/image-index.sh`). It runs only if both architectures passed.

The GitHub release is published before the image jobs start. If an image job fails, re-run the failed
jobs; the digest artifacts are kept for 7 days. Re-running after a successful push moves `:vX.Y.Z` to a
new index digest; routes pinned by digest keep working. If the index job fails at the read-back after
`imagetools create -t`, the tag has already moved: re-run the release, because routes pin the digest the
job summary prints.

Tags move; digests do not. The index job's summary prints the pushed reference,
`ghcr.io/<owner>/lux-gha-runner@sha256:...`, the digest of the index. Put that digest in each route's
`image`: a Lux host pulls the manifest for its own architecture from it, so one digest serves arm64 and
amd64 pools. To read it later:

```sh
docker buildx imagetools inspect ghcr.io/<owner>/lux-gha-runner:vX.Y.Z --format '{{.Manifest.Digest}}'
```

A new GHCR package is private. After the first release, make it public once (the package's
**Package settings** → **Change visibility**, reached from the repository's or owner's **Packages**), so
Lux hosts can pull it without registry credentials. Later pushes keep that visibility.

## License

lux-gha is source-available under the [Elastic License 2.0](LICENSE) (ELv2).
Copyright 2026 Márcio Martins. It is not an OSI-approved open-source license.

Personal use, internal business use, modification, and redistribution are
permitted subject to the license. For example, using lux-gha as internal
infrastructure for a noncompeting commercial application is permitted. Providing
lux-gha to third parties as a hosted or managed service that gives users access
to a substantial set of its features or functionality is not permitted. For
example, hosting lux-gha and giving customers access to a substantial set of its
features as a hosted or managed CI-runner service falls within this restriction.

ELv2 also prohibits circumventing license-key functionality or removing or
obscuring licensing, copyright, or other notices. It does not impose a general
ban on commercial redistribution or selling self-hosted derivatives.

This summary does not change the terms; the full [license](LICENSE) governs.
Third-party components retain their own licenses. For separate permission to
provide a hosted or managed service covered by the restriction, contact the
maintainer.
