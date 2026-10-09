# lux-gha

Foundation for a bridge supplying one-shot, just-in-time GitHub Actions
self-hosted runners as Lux generic Runs. GitHub remains the workflow engine;
Lux runs the runner process.

## Status: foundation only

`lux-gha` does **not** provision runners or launch jobs yet.

What exists:

- `config`: JSON configuration, validated before serving. It has an explicit installation ID, a runner group ID, allowed repositories, and routes keyed by label set.
- `githubapp`: GitHub App client:
  - RS256 app JWT
  - cached installation tokens
  - repository JIT config
  - workflow job and run reads
  - listing and deleting repository runners
- `luxclient`: thin HTTP client for Lux Runs:
  - create (with `Idempotency-Key`), get, paged list and terminate
  - a startup compatibility check
- `webhook`: signed `workflow_job` ingress that calls an injected function.
- `cmd/lux-gha`: loads config and secrets, checks Lux compatibility, then serves `GET /healthz` and `POST /webhook`. The webhook callback only logs the event and the matching route.

Wave 2 adds runner provisioning, deduplication by job identity, cleanup, reconciliation, and restart recovery. The runner image is also not implemented.

### Runner attribution

A JIT runner registered with a set of labels can be assigned by GitHub to **any** queued job whose labels it satisfies. That job need not be the one whose `queued` event led to the runner being provisioned. Nothing here assumes a provisioned runner belongs to the triggering job.

## Configuration

```json
{
  "listen": "127.0.0.1:8080",
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
     "nestedContainers": false}
  ]
}
```

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
- **Callback failure**: returns `503`. **GitHub does not automatically redeliver failed or missed deliveries.** Recovery will come from wave-2 reconciliation against the GitHub API, not from redelivery. There is no in-memory queue that claims durability.

## Lux compatibility

Lux has no `/features` or `/version` endpoint, so at startup `lux-gha` reads `GET /openapi.json`. It refuses to start unless both of these hold:

- `info.version` is a release tag `vX.Y.Z` of at least `v0.1.14`. `dev`, `git describe` builds and pre-releases are refused.
- The `RunSpec` schema declares `resumePolicy`.

The schema has no enum, so the version check is what proves `resumePolicy: never` is supported.

Each submission sends `resumePolicy: "never"`. The client rejects a response that doesn't echo it back.

A `never` Run ends `terminated` even when it succeeds. The outcome is in `exitCode` and `stateReason`, so `terminated` alone is not treated as failure.

There is no operator override for unverifiable Lux builds. Deploy a tagged release.

The JIT config is delivered only as a Lux file secret. It never appears in labels, env or the command. GitHub documents no lifetime for a JIT config, so the bridge assumes none.

## Development

```sh
make check   # gofmt check, go vet ./..., go test -race ./...
```
