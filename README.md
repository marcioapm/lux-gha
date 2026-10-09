# lux-gha

A bridge supplying one-shot, just-in-time GitHub Actions self-hosted runners
as Lux generic Runs. GitHub remains the workflow engine; Lux runs the runner
process.

## Packages

- `config`: JSON configuration, validated before serving. It has an explicit installation ID, a runner group ID, allowed repositories, routes keyed by label set, and reconciler settings.
- `githubapp`: GitHub App client:
  - RS256 app JWT
  - cached installation tokens
  - repository lookup (numeric ID)
  - repository JIT config
  - workflow job and run reads
  - listing and deleting repository runners
- `luxclient`: thin HTTP client for Lux Runs:
  - create (with `Idempotency-Key`), get, paged list and terminate
  - a startup compatibility check
- `webhook`: signed `workflow_job` ingress that calls an injected function.
- `reconcile`: the single reconciler that provisions and removes runners.
- `cmd/lux-gha`: loads config and secrets, checks Lux compatibility, starts the reconciler, then serves `GET /healthz`, `GET /status` and `POST /webhook`.

The runner image is not built here yet; `images/runner/entrypoint.sh` is the reference entrypoint (see [Runner entrypoint contract](#runner-entrypoint-contract)).

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

A pass costs `2 + active workflow runs + ⌈runners/100⌉` GitHub requests per
repository (active = `queued` or `in_progress`, including runs with only
GitHub-hosted jobs), plus one per provisioned runner; size `interval` against
the installation's hourly limit and the number of repositories. There is no database: the only persistent
state is Lux Run labels and GitHub's own records.

The bridge keeps **one live Run per queued matching job**. A pass:

1. **Demand.** For each repository, lists workflow runs with status `queued`
   and `in_progress` and their latest-attempt jobs. A job is demand when its
   status is `queued` and its labels match a route. `waiting` jobs
   (environment approval) are not demand. At most `reconcile.maxJobsPerPass`
   jobs are read per pass, in configuration order, so a backlog over that
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
   default labels, so more than one can match). If any repository could not
   be read, its submissions are unknown and no Run is created that pass.
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

`images/runner/entrypoint.sh` is a reference POSIX `sh` implementation.

## Configuration

```json
{
  "listen": "127.0.0.1:8080",
  "instance": "prod",
  "maxRunners": 20,
  "reconcile": {"interval": "60s", "idleGrace": "2m", "maxJobsPerPass": 200, "maxGensPerJob": 5},
  "github": {
    "appID": "Iv1.0123456789abcdef",
    "installationID": 12345678,
    "runnerGroupID": 1,
    "privateKeyFile": "/etc/lux-gha/github-app.pem",
    "webhookSecretFile": "/etc/lux-gha/webhook-secret"
  },
  "lux": {"url": "https://lux.example.com", "apiKeyFile": "/etc/lux-gha/lux-api-key"},
  "repositories": ["acme/widgets"],
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
- **`reconcile`**: optional; the values shown are the defaults.
- **`command`** (route): optional, overrides the Run's command (default `["/usr/local/bin/lux-gha-runner"]`).
- **`appID`**: the JWT issuer. It can be the App's client ID (GitHub's recommendation) or its numeric app ID.
- **`runnerGroupID`**: required by `generate-jitconfig`. Read the repository's runner group ID from GitHub rather than assuming `1`.
- **`apiURL`**: optional. It defaults to `https://api.github.com` and must be HTTPS. Plain HTTP is accepted only for loopback test servers. The API host never comes from webhook payloads.
- **Routing**: a job is routed only when **every** label it requests is in exactly one route's label set. Label comparison is case-insensitive. Configuration is rejected if one route's label set contains another's, because a job could then match both.
- **Repositories**: runners are registered per repository. Organisation-level runners are not supported.
- **Secrets**: these are read from the three files named above. A trailing newline is stripped. Secrets are never logged and are never part of the config value. Unknown config fields are rejected, so secrets can't be inlined by mistake.

## GitHub App permissions

Repository permissions, and nothing else:

| Permission | Access | Used for |
|---|---|---|
| Administration | Read and write | `generate-jitconfig`, listing and deleting repository runners |
| Actions | Read-only | `workflow_job` webhook events, reading workflow jobs and runs |

Subscribe to the **Workflow job** webhook event. Actions write is not needed.

## Webhook semantics

- **Signature**: `X-Hub-Signature-256` is checked over the exact raw body with a constant-time comparison. This happens before any parsing.
- **Ignored deliveries**: unsigned or invalid deliveries get `401`. Signed `ping`s, other events, unknown actions, and other installations or repositories get `204`, and no callback is made.
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
make check   # gofmt check, go vet ./..., go test -race ./...
```
