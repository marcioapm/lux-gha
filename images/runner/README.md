# lux-gha runner image

The image every lux-gha Run starts. Its entrypoint, `/usr/local/bin/lux-gha-runner`
(`entrypoint.sh`), starts rootless Docker, then runs GitHub's runner once with
the JIT config Lux places as a file secret. It exits when the runner exits.

## Contents

- `ubuntu:24.04`, pinned by its multi-arch index digest. Built and tested for `linux/arm64`; the
  Containerfile also selects the `linux/amd64` runner tarball and hash, but that build is untested.
- [actions/runner](https://github.com/actions/runner) 2.338.0 in `/home/runner/actions-runner`. The
  tarball is checked against its published SHA-256, and `bin/installdependencies.sh` runs at build time.
- Docker Engine 29.9.0 from Docker's apt repository: `docker-ce`, `docker-ce-cli`, `docker-ce-rootless-extras`,
  `containerd.io`, buildx 0.38.0, compose 5.6.0. Also `uidmap` (`newuidmap`/`newgidmap` with file
  capabilities only), `fuse-overlayfs`, `slirp4netns`, `iptables` and `iproute2`.
- `git`, `curl`, `ca-certificates`, `jq`, `unzip`, `xz-utils`, `python3`. uv is not baked in: `astral-sh/setup-uv` installs it.
- User `runner` (uid/gid 1000) with `/etc/subuid` and `/etc/subgid` ranges `1:999` and `1001:64535`.
  The image is `USER runner`. It has no `sudo` and no setuid or setgid files: a nested Lux Run has no
  `no-new-privileges`, so either would let a job become root in its container.
- The project licence at `/usr/share/lux-gha/LICENSE`, copied from `images/runner/LICENSE`, which must stay
  byte-identical to the root `LICENSE` (`scripts/check-license.sh`, also run by `smoke.sh`).

The entrypoint follows the [runner entrypoint contract](../../README.md#runner-entrypoint-contract).
If `dockerd-rootless.sh` is present and `DOCKER_HOST` is a `unix://` socket (the image default is
`unix:///tmp/xdg/docker.sock`), the entrypoint starts the engine listening on `DOCKER_HOST` (`--host`) and
logs to `/tmp/dockerd.log`. It waits up to 60 s in total for `docker info`, each probe bounded to 5 s, and
stops early if the daemon exits. If the engine does not come up, the entrypoint prints the log's last 20
lines and exits 1. Set `LUXGHA_DOCKER=optional` in the Run's env to continue without Docker instead.
`LUXGHA_DOCKER_WAIT` (whole seconds, no leading zero, default 60) changes the deadline. It exists for
`entrypoint_test.sh`; a Run should not need it.

## Build and smoke test

```sh
docker buildx build --platform linux/arm64 --load -t lux-gha-runner:dev -f images/runner/Containerfile images/runner
images/runner/smoke.sh                     # tools, caps, runner version, entrypoint lifecycle and failure modes
images/runner/smoke.sh --privileged        # also rootless dockerd + a docker-container buildx build
images/runner/smoke.sh --checksum-negative # a wrong runner SHA-256 must fail the build; rebuilds the runner layer
```

`entrypoint_test.sh` runs inside the image as part of `smoke.sh`, or on its own on Linux as a non-root
user with `sh images/runner/entrypoint_test.sh`, or anywhere with Docker as `make entrypoint-test` (uid 1000
in the pinned base image, against the working tree's `entrypoint.sh`). It drives the entrypoint with a fake `run.sh` and fake
`dockerd-rootless`/`docker`. It checks that `run.sh` replaces the entrypoint (exec), that its exit status
propagates, that a SIGTERM to the entrypoint's process group (how Lux stops a Run) reaches `run.sh` with
`RUNNER_MANUALLY_TRAP_SIG=1` set and leaves dockerd running, the bad-config, daemon-exit, never-ready and
hung-probe paths, and that the config never shows up in output or in the runner's environment.

The smoke cases that pass a fake config to the real runner only show that `run.sh` was reached with
invalid input. The `--privileged` checks are only an approximation, because Lux never runs a nested Run
privileged. Only a real nested Run proves that Docker works under Lux.
The real listener's job cancellation and log upload within `stopGrace` is
only verified live, against a real GitHub job; the signal harness uses a fake listener.
`make entrypoint-test` also checks uid 0 through real `setpriv`, dropping to
uid 1000; this path leaves Docker startup to the job.

CI runs the default `smoke.sh` on a linux/arm64 runner for every pull request. A `v*` tag builds, smoke-tests
and pushes the arm64 image to `ghcr.io/<owner>/lux-gha-runner` (see [Releases](../../README.md#releases)).

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
- Docker's store sits on the ephemeral volume Lux mounts at `~/.local/share/docker`, so `diskGiB` (or, on a
  route with `size`, that size's disk) must also hold the job's images and build cache.
- `egressHosts` or `unrestrictedEgress: true`, never both. With neither, the Run has no network and the
  runner cannot reach GitHub. For the pilot, see [Pilot egress](#pilot-egress).

### Egress

Some hosts are listed in GitHub's [communication requirements for self-hosted runners](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#communication).
Others were found by reading the source of the tool that uses them. Lux wildcards match subdomains.

| Host | Why | Source |
|---|---|---|
| `github.com`, `api.github.com`, `*.actions.githubusercontent.com` | runner registration, job messages, OIDC | GitHub docs |
| `codeload.github.com` | downloading actions | GitHub docs |
| `results-receiver.actions.githubusercontent.com`, `*.blob.core.windows.net` | logs, summaries, artifacts, caches | GitHub docs |
| `objects.githubusercontent.com`, `objects-origin.githubusercontent.com`, `github-releases.githubusercontent.com`, `github-registry-files.githubusercontent.com` | runner self-update, which a JIT runner cannot turn off (below) | GitHub docs |
| `release-assets.githubusercontent.com` | release assets; `github.com/.../releases/download` redirects here (uv, Python builds) | GitHub docs; redirect observed |
| `ghcr.io`, `pkg-containers.githubusercontent.com` | GHCR pull and push | GitHub docs |
| `raw.githubusercontent.com` | setup-uv's version manifest | setup-uv v10.3.0 `src/utils/constants.ts` |
| `releases.astral.sh` | setup-uv's mirror for downloading uv itself; uv's mirror for managed Python | setup-uv `constants.ts`; uv 0.13.0 `uv-python-managed/src/downloads.rs` |
| `pypi.org`, `files.pythonhosted.org` | `uv sync` | uv's default index |
| `registry-1.docker.io`, `auth.docker.io`, `production.cloudfront.docker.com` | Docker Hub base images, and `moby/buildkit` if setup-buildx-action keeps its default `docker-container` driver | Docker allowlist; blob redirect observed |

`astral.sh` itself is only the uv install script's host, and setup-uv does not use it.

Anyone can create a storage account under `*.blob.core.windows.net`. A job allowed that wildcard can therefore
send data to a party it chooses, a risk Lux's runspec spells out for wildcards. GitHub gives no narrower
name for its artifact and cache storage.

#### Runner updates

Pinning the tarball does not stop the runner from updating itself. `--disableupdate` is a `config.sh`
flag that `run.sh` rejects. A JIT runner reads the setting (`DisableUpdate`) from the `.runner` file GitHub
embeds in its JIT config, and `generate-jitconfig` has no parameter to set it (actions/runner v2.338.0
`CommandSettings.cs`, `Runner.cs`, `ConfigurationStore.cs`; GitHub REST docs). Whether GitHub sets it in
JIT configs is not documented. If GitHub sends this runner an update message, the runner downloads the new
version from the update hosts above and restarts into it. Keep those hosts allowed. The update does not
carry over to the next Run, which starts from the image again. GitHub stops queueing jobs to a runner 30
days after a newer runner release, or sooner for a critical security update, so keep `RUNNER_VERSION`
current.

#### Pilot egress

The table covers the runner and the tools named above. The first pilot workflow's own image build needed more:

- Docker Hub, for its base `ubuntu:26.04`;
- the Ubuntu arm64 apt mirror, `ports.ubuntu.com`;
- `registry.npmjs.org`;
- `services.gradle.org`;
- `builds.dotnet.microsoft.com`;
- the Maven and Gradle plugin repositories and PyPI hosts its build resolves from.

These come from reading the build, not from observing it. Start the pilot route with
`"unrestrictedEgress": true` and no `egressHosts` (see [Configuration](../../README.md#configuration)).
An unrestricted Run uses the host's resolvers, not Lux's DNS stub, so Lux records no `dns` events for it.
To tighten, run a route with `egressHosts` from the table above plus the list here, and add the refused
names Lux records as `dns` events for its Runs.

### BuildKit driver

`docker/setup-buildx-action` defaults to the `docker-container` driver. On every job, that driver pulls
`moby/buildkit` (about 106 MB for arm64) anonymously from Docker Hub into the empty per-Run store. That
pull counts against Docker Hub's anonymous pull limit, shared with every other pull from the same egress
IP. `--load` then copies the result twice: the BuildKit container exports it, then dockerd imports it.
This image runs Docker 29 with the containerd image store, so the `docker` driver uses the daemon's
embedded BuildKit: no pull, and `--load` goes straight into the store. Setting `driver: docker` in
a workflow's `setup-buildx-action` step is worth trying; that change belongs in the workflow. `smoke.sh
--privileged` builds with `docker-container`, setup-buildx-action's default.
