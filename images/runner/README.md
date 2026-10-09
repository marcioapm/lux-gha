# lux-gha runner image

The image every lux-gha Run starts. Its entrypoint, `/usr/local/bin/lux-gha-runner`
(`entrypoint.sh`), starts rootless Docker, then runs GitHub's runner once with
the JIT config Lux places as a file secret. It exits when the runner exits.

## Contents

- `ubuntu:24.04`, pinned by digest. Built and tested for `linux/arm64`; `linux/amd64` also builds.
- [actions/runner](https://github.com/actions/runner) 2.338.0 in `/home/runner/actions-runner`. The
  tarball is checked against its published SHA-256, and `bin/installdependencies.sh` runs at build time.
- Docker Engine 29.9.0 from Docker's apt repository: `docker-ce`, `docker-ce-cli`, `docker-ce-rootless-extras`,
  `containerd.io`, buildx 0.38.0, compose 5.6.0. Also `uidmap` (`newuidmap`/`newgidmap` with file
  capabilities only), `fuse-overlayfs`, `slirp4netns`, `iptables` and `iproute2`.
- `git`, `curl`, `ca-certificates`, `jq`, `unzip`, `xz-utils`, `python3`. uv is not baked in: `astral-sh/setup-uv` installs it.
- User `runner` (uid/gid 1000) with `/etc/subuid` and `/etc/subgid` ranges `1:999` and `1001:64535`.
  The image is `USER runner`. It has no `sudo` and no setuid or setgid files: a nested Lux Run has no
  `no-new-privileges`, so either would let a job become root in its container.

The entrypoint follows the [runner entrypoint contract](../../README.md#runner-entrypoint-contract).
If `dockerd-rootless.sh` is present and `DOCKER_HOST` is a `unix://` socket (the image default is
`unix:///tmp/xdg/docker.sock`), the entrypoint starts the engine and logs to `/tmp/dockerd.log`. It waits
up to 60 s for `docker info`. If the engine does not come up, the entrypoint prints the log's last 20 lines
and exits 1. Set `LUXGHA_DOCKER=optional` in the Run's env to continue without Docker instead.

## Build and smoke test

```sh
docker buildx build --platform linux/arm64 -t lux-gha-runner:dev images/runner
images/runner/smoke.sh               # tools, uid, entrypoint failure modes
images/runner/smoke.sh --privileged  # also rootless dockerd + buildx
```

The `--privileged` check is only an approximation, because Lux never runs a nested Run privileged. Only a
real nested Run proves that Docker works under Lux.

## Route

```json
{"name": "arm", "labels": ["self-hosted", "linux", "arm64", "lux"], "pool": "gha-nested",
 "image": "ghcr.io/<owner>/lux-gha-runner@sha256:<digest>",
 "cpus": 4, "memoryMiB": 8192, "diskGiB": 40, "timeout": "90m",
 "nestedContainers": true, "egressHosts": ["..."]}
```

- `image`: the pushed image, referenced by digest.
- `nestedContainers: true`, with `pool` set to a pool whose hosts run `lux-runner --nested`. Otherwise the
  entrypoint exits 1 because dockerd cannot start.
- Docker's store sits on the ephemeral volume Lux mounts at `~/.local/share/docker`, so `diskGiB` must
  also hold the job's images and build cache.

### Egress

Some hosts are listed in GitHub's [communication requirements for self-hosted runners](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#communication).
Others were found by reading the source of the tool that uses them. Lux wildcards match subdomains.

| Host | Why | Source |
|---|---|---|
| `github.com`, `api.github.com`, `*.actions.githubusercontent.com` | runner registration, job messages, OIDC | GitHub docs |
| `codeload.github.com` | downloading actions | GitHub docs |
| `results-receiver.actions.githubusercontent.com`, `*.blob.core.windows.net` | logs, summaries, artifacts, caches | GitHub docs |
| `objects.githubusercontent.com` | listed for runner updates; the image pins the runner instead | GitHub docs |
| `release-assets.githubusercontent.com` | release assets; `github.com/.../releases/download` redirects here (uv, Python builds) | GitHub docs; redirect observed |
| `ghcr.io`, `pkg-containers.githubusercontent.com` | GHCR pull and push | GitHub docs |
| `raw.githubusercontent.com` | setup-uv's version manifest | setup-uv v10.3.0 `src/utils/constants.ts` |
| `releases.astral.sh` | setup-uv's mirror for uv downloads; uv's mirror for managed Python | setup-uv `constants.ts`; uv 0.13.0 `uv-python-managed/src/downloads.rs` |
| `pypi.org`, `files.pythonhosted.org` | `uv sync` | uv's default index |
| `registry-1.docker.io`, `auth.docker.io`, `production.cloudfront.docker.com` | `moby/buildkit` for setup-buildx-action's default `docker-container` driver, and Docker Hub base images | Docker allowlist; blob redirect observed |

`astral.sh` itself is only the uv install script's host, and setup-uv does not use it.

Anyone can create a storage account under `*.blob.core.windows.net`. A job allowed that wildcard can therefore
send data to a party it chooses, a risk Lux's runspec spells out for wildcards. GitHub gives no narrower
name for its artifact and cache storage.

GitHub stops queueing jobs to a runner 30 days after a newer runner release, or sooner for a critical
security update. A self-update cannot carry over to the next Run, so keep `RUNNER_VERSION` current.
