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
- `luxclient`: thin HTTP client for Lux Runs:
  - create (with `Idempotency-Key`), get, paged list and terminate
  - a startup compatibility check
- `webhook`: signed `workflow_job` ingress that calls an injected function, and optionally `installation` and `installation_repositories` ingress.
- `reconcile`: the single reconciler that provisions and removes runners.
- `cmd/lux-gha`: loads config and secrets, checks Lux compatibility, starts the reconciler, then serves `GET /healthz`, `GET /status` and `POST /webhook`. `lux-gha --version` prints the version (`dev` unless set at build time) and exits; it is also logged at startup.

The runner image is in `images/runner` (see its [README](images/runner/README.md) and the [Runner entrypoint contract](#runner-entrypoint-contract)).

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
GitHub-hosted jobs), plus one per provisioned runner. With a `repositories`
list every listed repository is read every pass; size `interval` against the
installation's hourly limit and the number of repositories. Without one, a
pass reads a bounded subset ([Repository discovery](#repository-discovery)).
There is no database: the only persistent
state is Lux Run labels and GitHub's own records.

The bridge keeps **one live Run per queued matching job**. A pass:

1. **Demand.** For each repository read this pass, lists workflow runs with status `queued`
   and `in_progress` and their latest-attempt jobs. A job is demand when its
   status is `queued` and its labels match a route. `waiting` jobs
   (environment approval) are not demand. At most `reconcile.maxJobsPerPass`
   jobs are read per pass, in configuration order (name order when
   discovered), so a backlog over that
   bound in one repository can delay the others.
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
   provisioning stops for the pass.
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
   first seen. A Run lost mid-job is left alone: GitHub fails the job and
   nothing reruns it.

Steps 3 to 5 act only on repositories read successfully this pass. A
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
its rate limit, so each pass reads:

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
caps keep unserved goes cold; the pass after a slot frees reads every
repository whose last read found demand again.

A repository that leaves the set (removed from the installation, archived or
disabled) is no longer read. Its live Runs are neither adopted nor
terminated; they count against the caps until they end on their own (the
runner exits, or the route's `timeout`). Its runners stay registered. A read
that answers 404 or 403 re-lists the installation on the next pass, so a
removal whose `installation_repositories` delivery was lost blocks creation
for a pass or two, not `repositoryRefresh`.

**Rate-limit arithmetic.** With the defaults (`interval` 60 s, `coldSweep`
10 m) and 68 repositories of which 2 are hot, a ticked pass reads the 2 hot
repositories (`2 × (3 + their active runs)`, 8 with one active run each)
and about `66 / 9 ≈ 7.3` cold ones at 3 requests each (22), plus one listing
page every 10 minutes: about 30 requests per pass, about 1,800 per hour.
The cold share is paced by time, not by passes, so it stays near
`66 × 3 × 60 / 9 ≈ 1,320` requests per hour however often webhooks wake the
loop; woken passes add only hot reads. The startup pass reads every
repository once, at about 3 requests each (about `68 × 3 ≈ 204`). An
installation's limit is 5,000 requests per hour (more for large
organisations), so this leaves well over half of it free.

Hot reads are what passes multiply: the worst case is
`hot × ~4 requests × passes per hour`, where woken passes come at most every
5 s (720 per hour). During a burst the hot set is bounded by free capacity,
not by the installation: repositories with live Runs (at most `maxRunners`),
demand only while a slot is free for it, and deliveries whose job no read has
listed yet. With `maxRunners` 10 and every slot used by a different
repository, an hour of ticks costs about `10 × 4 × 60 = 2,400` hot requests
on top of the cold share, but 15 minutes of 5 s wakes cost about
`10 × 4 × 180 = 7,200`. A sustained stream of matching `queued` deliveries
with many live Runs can therefore exceed the limit, as with a `repositories`
list; a rate limit then pauses passes until GitHub's reset time.

### Lifecycle safety with partial reads

Every rule below holds per pass for the repositories read in it; an unread
repository is unknown, as above.

- **Unread repositories.** Nothing is created, terminated or deleted for a
  repository the pass did not read, and its live Runs count against both caps
  through the Lux listing. A hot repository is read every pass, so a
  repository left unread has no live Run and no remembered runner, unless its
  read failed or it left the set; then its live Runs are kept and
  counted all the same.
- **Read failure.** If any repository selected this pass fails to read, no Run
  is created that pass, exactly as with a list. A cold repository that keeps
  failing blocks creation only on the passes that select it (its rotation slot
  moves on). A hot one, or one not yet read successfully since startup, is
  selected every pass and so blocks creation until it reads: it may hold a
  registration whose submission is unresolved. A repository that stays in the
  listing but always fails to read therefore blocks creation in every
  repository; it is logged at `WARN` with its name, and without
  `repositories` the fix is to remove it from the installation.
- **Unresolved submissions.** A registered runner with no Run may still have a
  submission in flight, so it holds a cap slot. In a repository read this pass
  it is classified as before. A repository with a remembered managed runner is
  hot, so it is read every pass; if that read fails, creation is blocked. If it
  left the set, its remembered runners without a live Run keep holding
  slots for `idleGrace` after it left, the same delay orphan cleanup gives a
  readable one. A repository first seen after a restart is read on the first
  pass that lists it, so a registration from before the restart is found before
  it can be outrun.
- **Adoption of late Runs.** A submission whose outcome was unknown commits
  into Lux; the next pass's Lux listing makes its repository hot, so it is read
  and its job adopts the Run as before. An adopted Run counts against the caps
  from the moment Lux lists it, read or not.
- **Surplus deletion.** Deletes only idle runners whose repository was read
  this pass, compares them with that repository's demand, and terminates a Run
  only after its runner was deleted. No unread repository's Run is reached.
- **Finished Runs.** A live Run whose runner is gone is terminated only when
  its repository was read and lacked the runner, in two passes `idleGrace`
  apart. Its live Run makes the repository hot, so it is read every pass.
- **Orphans.** A cold repository has no Run Lux knows of. An orphaned runner
  there (offline, no live Run) is seen when the repository is next read,
  within `coldSweep`, which makes the repository hot; it is deleted
  `idleGrace` later.
- **Caps.** The global and route caps count every live Run of the instance
  and every held slot, regardless of which repositories were read.

`GET /status` reports `demand` for the repositories read in the pass.

Run labels (no secrets ever go in labels):

| Label | Value |
|---|---|
| `lux-gha/managed` | instance |
| `lux-gha/repo` | numeric repository ID |
| `lux-gha/route` | route name |
| `lux-gha/job` | ID of the job that triggered provisioning |
| `lux-gha/gen` | generation for that job |
| `lux-gha/runner` | GitHub runner name |

A GitHub rate limit stops the pass where it is; no pass calls GitHub again
until the reported retry time. Other errors end the pass early or skip one
item; the next pass retries. The idle and orphan timers are in memory: a
restart re-derives everything else from Lux and GitHub and only restarts those
timers, which delays cleanup but never duplicates a Run.

`GET /status` returns the last pass as JSON: `time`, `durationMs`, `error`
(a category: `github_rate_limited`, `github`, `lux`,
`lux_resume_policy_not_echoed`, `lux_run_mismatch`), `rateLimitedUntil`, `demand` and `live` per
route, `idle`, `busy`, and `provisioned`, `terminated` and `deleted` in that
pass. It holds no credentials or JIT configuration.

## Runner entrypoint contract

The Run's command is `["/usr/local/bin/lux-gha-runner"]`, or the route's
`command`. Lux places the JIT configuration as a file secret at
`/run/lux-gha/jitconfig` and the Run's environment has
`LUXGHA_JITCONFIG_FILE=/run/lux-gha/jitconfig`. (Lux reserves the `LUX_`
environment prefix for itself.) The entrypoint:

- reads the JIT configuration from the file named by `LUXGHA_JITCONFIG_FILE`;
- deletes nothing;
- `exec`s `./run.sh --jitconfig "<contents>"` from the runner directory as a
  non-root user;
- exits when the runner exits, after at most one job.

`images/runner/entrypoint.sh` is a reference POSIX `sh` implementation. It
also starts rootless Docker first when the image has it.

## Configuration

```json
{
  "listen": "127.0.0.1:8080",
  "instance": "prod",
  "maxRunners": 20,
  "reconcile": {"interval": "60s", "idleGrace": "2m", "maxJobsPerPass": 200, "maxGensPerJob": 5,
                "repositoryRefresh": "10m", "hotWindow": "2m", "coldSweep": "10m"},
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
     "nestedContainers": false, "maxRunners": 10}
  ]
}
```

- **`instance`**: required, 1-20 of `a-z`, `0-9` and `-`. It names this bridge in Run labels and runner names; two bridges sharing a GitHub repository or Lux tenant need different instances.
- **`maxRunners`**: required global cap on live Runs. A route's optional `maxRunners` caps that route too.
- **`reconcile`**: optional; the values shown are the defaults. `repositoryRefresh`, `hotWindow` and `coldSweep` apply only without `repositories` ([Repository discovery](#repository-discovery)): how often the installation's repositories are re-listed, how long a `queued` delivery keeps its repository read every pass while the runs API does not yet list its job (GitHub's lag between delivery and API), and the longest a cold repository goes unread. `coldSweep` should exceed `interval`; at or below it every repository is read every pass.
- **`command`** (route): optional, overrides the Run's command (default `["/usr/local/bin/lux-gha-runner"]`).
- **Egress** (route): Lux Runs are default-deny. `egressHosts` sends one `network.egress` host rule per entry, and the Run reaches only those names. `unrestrictedEgress: true` sends `network: {"unrestricted": true}` instead, which switches Lux's egress filtering off (Lux's hard blocks, such as cloud metadata and the control plane, still apply). Setting both is rejected. A route with neither is valid, but its Runs have **no network at all**, so the runner cannot reach GitHub.
- **`appID`**: the JWT issuer. It can be the App's client ID (GitHub's recommendation) or its numeric app ID.
- **`runnerGroupID`**: required by `generate-jitconfig`. Read the repository's runner group ID from GitHub rather than assuming `1`.
- **`apiURL`**: optional. It defaults to `https://api.github.com` and must be HTTPS. Plain HTTP is accepted only for loopback test servers. The API host never comes from webhook payloads.
- **Routing**: a job is routed only when **every** label it requests is in exactly one route's label set. Label comparison is case-insensitive. Configuration is rejected if one route's label set contains another's, because a job could then match both.
- **Repositories**: runners are registered per repository. Organisation-level runners are not supported. `repositories` is optional. Omitted, as in the example, every repository of the installation is served ([Repository discovery](#repository-discovery)). Given, as `"repositories": ["acme/widgets"]`, only those are served and each is read every pass; an empty list is rejected.
- **Secrets**: these are read from the three files named above. A trailing newline is stripped. Secrets are never logged and are never part of the config value. Unknown config fields are rejected, so secrets can't be inlined by mistake.

## GitHub App permissions

Repository permissions, and nothing else:

| Permission | Access | Used for |
|---|---|---|
| Administration | Read and write | `generate-jitconfig`, listing and deleting repository runners |
| Actions | Read-only | `workflow_job` webhook events, reading workflow jobs and runs |
| Metadata | Read-only | Granted to every App; `GET /installation/repositories` |

Subscribe to the **Workflow job** webhook event. Actions write is not needed.

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

A `never` Run ends `terminated` even when it succeeds. The outcome is in `exitCode` and `stateReason`, so `terminated` alone is not treated as failure.

There is no operator override for unverifiable Lux builds. Deploy a tagged release.

The JIT config is delivered only as a Lux file secret. It never appears in labels, env or the submitted Lux command. At runtime the entrypoint passes it as `./run.sh --jitconfig` argv, which is GitHub's documented way to start a JIT runner. GitHub documents no lifetime for a JIT config, so the bridge assumes none.

## Development

```sh
make lint              # gofmt check, go vet ./..., scripts/check-license.sh
make test              # go test -race -count=1 ./...
make check             # lint, then test
make entrypoint-test   # images/runner/entrypoint_test.sh as uid 1000 in the runner's pinned ubuntu base (Docker)
make image             # the runner image for the host's architecture, as lux-gha-runner:dev (IMAGE_TAG=...)
make dist VERSION=vX.Y.Z  # release tarballs in dist/ (below); needs GNU tar (gtar on macOS)
```

CI (`.github/workflows/ci.yml`) runs on pull requests and pushes to `main`: `make lint`, one
`go test -race -v` run that fails if any test skipped, and `make dist VERSION=v0.0.0`; `shellcheck`, `sh -n`
and `make entrypoint-test`; and a linux/arm64 build of the runner image with `images/runner/smoke.sh`
(unprivileged). The arm64 jobs run on `ubuntu-24.04-arm`, a hosted runner GitHub provides only to public
repositories: while the repository is private they get no runner.

## Releases

Pushing a tag `vX.Y.Z` runs `.github/workflows/release.yml`:

1. `make test`, then `make dist` with `VERSION` set to the tag, published as a GitHub release with generated notes:
   - `lux-gha_vX.Y.Z_linux_arm64.tar.gz` and `lux-gha_vX.Y.Z_linux_amd64.tar.gz`, each holding
     `bin/lux-gha` (static, `lux-gha --version` prints `vX.Y.Z`) and `share/lux-gha/LICENSE`;
   - `SHA256SUMS` for both.

   `make dist` fails unless each binary carries the tag as its version. The tarballs are reproducible:
   rebuilding the same tag with the same Go toolchain and GNU gzip gives byte-identical files.
2. The runner image, built for **linux/arm64 only** and smoke-tested before it is pushed as
   `ghcr.io/<owner>/lux-gha-runner:vX.Y.Z`. The Containerfile can select the amd64 runner, but no amd64
   image is built or tested.

The GitHub release is published before the image job starts. If the image job fails, re-run only that
job. Re-running it after a successful push moves `:vX.Y.Z` to a new digest; routes pinned by digest keep
working.

Tags move; digests do not. The release job's summary prints the pushed reference,
`ghcr.io/<owner>/lux-gha-runner@sha256:...`. Put that digest in each route's `image`. To read it later:

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
