# lux-gha runner image

The image every lux-gha Run starts. Its entrypoint, `/usr/local/bin/lux-gha-runner`
(`entrypoint.sh`), starts rootless Docker, then runs GitHub's runner once with
the JIT config Lux places as a file secret. It exits when the runner exits.

## Contents

- `ubuntu:26.04`, pinned by its multi-arch index digest. Built and smoke-tested for `linux/arm64` and
  `linux/amd64`; the Containerfile selects the runner tarball and its hash by `TARGETARCH`.
- [actions/runner](https://github.com/actions/runner) 2.338.0 in `/home/runner/actions-runner`. The
  tarball is checked against its published SHA-256, and `bin/installdependencies.sh` runs at build time.
- Docker Engine 29.9.0 from Docker's apt repository: `docker-ce`, `docker-ce-cli`, `docker-ce-rootless-extras`,
  `containerd.io`, buildx 0.38.0, compose 5.6.0. Also `uidmap` (`newuidmap`/`newgidmap` with file
  capabilities only), `fuse-overlayfs`, `slirp4netns`, `iptables` and `iproute2`.
- `git`, `curl`, `ca-certificates`, `jq`, `unzip`, `xz-utils`, `python3`, and `getcap`/`setcap` (`libcap2-bin`).
  uv is not baked in: `astral-sh/setup-uv` installs it.
- GitHub CLI 2.103.3 (`gh`) in `/usr/local/bin`, from the release tarball checked against its published SHA-256
  for each architecture. Hosted runners ship it, and jobs call it without a setup action.
- `socat` and the AWS Session Manager plugin 1.2.896.0 (`session-manager-plugin`, checked against a pinned
  SHA-256 per architecture), for `aws ssm start-session` port forwarding, which jobs otherwise install with
  `sudo`.
- AWS CLI v2 2.38.1 (`aws`), installed from versioned, SHA-256-pinned archives for each architecture.
  Jobs can use it directly without downloading tools or elevating privileges at runtime.
- `docker-credential-ecr-login` 0.12.0, checked against the release's SHA-256 for each architecture, behind
  `docker-credential-lux-gha`, with `/etc/lux-gha/aws-config`, `/usr/local/bin/lux-gha-aws-credentials` and
  the job-started hook `/usr/local/lib/lux-gha/job-started.sh`. See [In-region image registry](#in-region-image-registry).
- `libatomic1`: the official Node.js linux-arm64 builds that `actions/setup-node` installs link it, and
  `node` exits 127 without it. The amd64 image has it too.
- System libraries for browser end-to-end jobs, which cannot `apt-get install` here (no root): the Ubuntu
  26.04 `tools` and `chromium` groups in the published playwright-core 1.64.1 `lib/coreBundle.js`
  (from `nativeDeps.ts`). Playwright explicitly supports `ubuntu26.04-x64` and `ubuntu26.04-arm64`;
  it does not fall back to 24.04. Both architectures use the same tools/Chromium package names.
  That is Xvfb (`xvfb-run`), the fonts Playwright expects (Liberation, Noto Color Emoji,
  Unifont, IPA Gothic, WenQuanYi Zen Hei, TLWG Loma, FreeFont, the X11 scalable and Cyrillic fonts) and Chromium's
  shared libraries (NSS, NSPR, ATK/AT-SPI, CUPS, ALSA, GBM, DRM, Cairo, Pango, the X11/XCB extension libraries,
  xkbcommon). A job runs `playwright install chromium` without `--with-deps`, which downloads only the browser,
  matching its own pinned Playwright version, into `~/.cache/ms-playwright`. Active browser matrices are
  Chromium only; Firefox and WebKit dependencies are not included or validated. Allure is not baked in:
  consumers use a pinned Allure tarball with the cached JDK, not a second JRE.
- `ffmpeg` and `espeak-ng`, for jobs that record test runs and narrate videos.
- `libgl1` (`libGL.so.1`), which the `opencv-python` wheel needs to import, for screenshot comparison in
  visual-diff jobs.

  Native amd64: `docker image inspect` reports 3,002,175,411 bytes; `docker history` reports about
  563 MB for the browser/media apt layer (Ubuntu 26.04, 2026-10-27). No same-base baseline image was
  available locally; total baseline size and compressed sizes were not measured. All 36 direct packages
  exist in the resolute arm64/`all` indexes, but native arm64 browser validation and image sizes are
  unmeasured locally; CI runs that native gate. Historical 24.04 sizes do not describe this image.
- User `runner` (uid/gid 1000) with `/etc/subuid` and `/etc/subgid` ranges `1:999` and `1001:64535`.
  The image is `USER runner`. It has no `sudo` and no setuid or setgid files: a nested Lux Run has no
  `no-new-privileges`, so either would let a job become root in its container.
- The project licence at `/usr/share/lux-gha/LICENSE`, copied from `images/runner/LICENSE`, which must stay
  byte-identical to the root `LICENSE` (`scripts/check-license.sh`, also run by `smoke.sh`).
- `moby/buildkit` v0.33.1 for the image's own architecture, as an OCI archive at
  `/usr/share/lux-gha/buildkit.tar` (about 107 MB arm64, 113 MB amd64). The entrypoint loads it into each
  Run's Docker store, so that `setup-buildx-action`'s builder does not pull it. See [BuildKit driver](#buildkit-driver).
- `RUNNER_TOOL_CACHE` and `AGENT_TOOLSDIRECTORY` set to `/opt/hostedtoolcache` (owned by `runner`, mode 0755), where
  hosted runners keep it: `actions/setup-python`'s CPython has its library RUNPATH there and fails to load
  `libpython` elsewhere once `LD_LIBRARY_PATH` is dropped. It is not on the Lux volume, so tools a job installs are
  still per-Run. The tool cache comes preinstalled; see [Preinstalled tool cache](#preinstalled-tool-cache).

## Preinstalled tool cache

| Tool | Version | Path under `/opt/hostedtoolcache` | Found by |
|---|---|---|---|
| CPython | 3.11.17 | `Python/3.11.17/<x64\|arm64>` | `actions/setup-python` with `python-version: 3.11` (or `3.11.17`) |
| Temurin JDK | 25.0.4.1+1 (Adoptium semver `25.0.4+101.0.LTS`) | `Java_Temurin-Hotspot_jdk/25.0.4-101.0.LTS/<x64\|arm64>`, plus `aarch64` → `arm64` | `actions/setup-java` with `distribution: temurin`, `java-version: 25` |
| Node.js | 26.10.0 | `node/26.10.0/<x64\|arm64>` | `actions/setup-node` with `node-version: 26` (or `26.10.0`) |

Each sits at `<tool>/<version>/<arch>` with an `<arch>.complete` marker beside it, owned by `runner`. That is the
layout `@actions/tool-cache`'s `find()` requires. A setup action whose version spec matches therefore uses the
baked copy, adds it to `PATH` and downloads nothing. Measured offline in the image with setup-python v6.3.0,
setup-java v5.7.0 and v6.1.0, and setup-node v7.1.0. The cache is skipped when the spec names a different version, with
`check-latest: true`, with setup-java's `java-version: latest`, or with setup-node aliases (`lts/*`, `latest`,
`current`), which resolve the version remotely first. setup-java v6 names arm64 `aarch64` and v4/v5 name it
`arm64`, hence the symlink, which has its own `.complete` marker.

- Python is the `linux-26.04` build from `actions/python-versions`, installed by its own `setup.sh` as
  setup-python does, so its RUNPATH `/opt/hostedtoolcache/Python/3.11.17/<arch>/lib` holds.
- The JDK directory name is not the JDK's version. setup-java takes the Adoptium API's `version_data.semver`
  (`25.0.4+101.0.LTS` for the release `jdk-25.0.4.1+1`) and replaces its first `+` with `-`. Any other name is a
  silent cache miss. Adoptium derives the semver from the release: `jdk-A.B.C[.P]+N` becomes `A.B.C+(P*100+N)`
  (P is 0 when absent), followed by `.0.LTS` for an LTS build and nothing otherwise (`26.0.2+101` for
  `jdk-26.0.2.1+1`). The build and `smoke.sh` derive the name the same way and fail if `JAVA_TOOLCACHE_VERSION`
  differs.
- Gradle is not baked. `gradle/actions/setup-gradle` (v6.4.0, `sources/src/execution/provision.ts`) never reads
  the tool cache. It looks for a `gradle` on `PATH` whose `gradle -v` matches, then under `RUNNER_TEMP`, then in its
  GitHub cache entry, and only then downloads. Wrapper distributions under `~/.gradle/wrapper/dists` are per-Run.
- Stripped at build time: the JDK's `lib/src.zip`, CPython's `lib/python3.11/test/` and its static
  `lib/python3.11/config-3.11-*/libpython3.11.a` (about 220 MB, none read by a setup action); a job that needs
  CPython's test suite or a static libpython must install its own Python.

Image size (`docker image inspect -f '{{.Size}}'`, local build): arm64 545 433 437 bytes without the tool cache,
745 618 450 with it (about 200 MB more). On disk the three take about 138 MB (Python), 264 MB (JDK) and 224 MB
(Node) on arm64. amd64 was 656 969 687 → 950 214 749 bytes before the strip and has not been re-measured since.

### Bumping a tool

Each tool is one `RUN` in the Containerfile with its `ARG`s declared just above it, so a bump rebuilds that
layer and those after it. `smoke.sh` reads the same `ARG`s. With every bump, also update the version and path in
the [table above](#preinstalled-tool-cache) and the image and per-tool size figures below it, measured on the new
build.

- Python: take the newest stable `3.11.x` entry from `actions/python-versions`' `versions-manifest.json`.
  Set `PYTHON_VERSION`, set `PYTHON_RELEASE` to the release tag in its `download_url` (`<version>-<run id>`), and
  set `PYTHON_SHA256_ARM64`/`_AMD64` to the `digest` of the `python-<v>-linux-26.04-<arm64|x64>.tar.gz` assets in
  `api.github.com/repos/actions/python-versions/releases/tags/<tag>`.
- JDK: query `api.adoptium.net/v3/assets/feature_releases/25/ga?os=linux&image_type=jdk&jvm_impl=hotspot&architecture=<aarch64|x64>`.
  Set `JAVA_RELEASE` to `release_name`, `JAVA_TOOLCACHE_VERSION` to `version_data.semver` with its first `+`
  replaced by `-`, and `JAVA_SHA256_ARM64`/`_AMD64` to `binaries[0].package.checksum`. The build fails if
  `JAVA_TOOLCACHE_VERSION` is not the name derived from `JAVA_RELEASE`, or if the tarball's `release` file does not
  name `JAVA_RELEASE` (with `-LTS` exactly when the name ends in `.LTS`).
- Node: set `NODE_VERSION` and `NODE_SHA256_ARM64`/`_AMD64` from `nodejs.org/dist/v<version>/SHASUMS256.txt`
  (`node-v<version>-linux-<arm64|x64>.tar.xz`).

Other Node and Java versions are downloaded by their setup actions as before. For Python,
`actions/setup-python` downloads an uncached version only if `actions/python-versions` publishes an
Ubuntu **26.04** asset for that version and architecture; it does not fall back to a 24.04 build.
For example, 3.11.17 has 26.04 assets, while 3.11.14 and 3.12.12 do not (manifest checked 2026-10-27).
Update an exact pin to a release with 26.04 assets, or keep a 24.04 image/use another Python provider
for unsupported versions. Do not silently substitute a different Python version.

### Migrating jobs from Ubuntu 24.04

The default PATH uses Ubuntu's **system Python 3.14**, not the cached CPython 3.11.17. Jobs invoking
`python3` without a setup action therefore move from 3.12 to 3.14. In Python 3.14,
`asyncio.get_event_loop()` raises `RuntimeError` if no event loop has been set, and the default
multiprocessing start method on Linux changes from `fork` to `forkserver`; scripts relying on
implicit loops or inherited fork state need adjustment. Select the intended interpreter with
`actions/setup-python` (subject to the asset constraint above), or explicitly update the script.
Native smoke asserts Ubuntu 26.04 and system Python 3.14 separately from the cached-Python tests.

See the [Python 3.14 changes](https://docs.python.org/3/whatsnew/3.14.html) and the
[available setup-python builds](https://github.com/actions/python-versions/blob/main/versions-manifest.json).

The entrypoint follows the [runner entrypoint contract](../../README.md#runner-entrypoint-contract).
If `dockerd-rootless.sh` is present and `DOCKER_HOST` is a `unix://` socket (the image default is
`unix:///tmp/xdg/docker.sock`), the entrypoint starts the engine listening on `DOCKER_HOST` (`--host`) and
logs to `/tmp/dockerd.log`. It runs rootlesskit with `slirp4netns` and host loopback allowed, and sets
`--host-gateway-ip=10.0.2.2`, slirp4netns's address for the runner's loopback: a container started with
`--add-host host.docker.internal:host-gateway` reaches a server the job bound to `127.0.0.1`, as with rootful
Docker. That loopback belongs to the Run's own network namespace, not the Lux host's. It waits up to 60 s in total for `docker info`, each probe bounded to 5 s, and
stops early if the daemon exits. If the engine does not come up, the entrypoint prints the log's last 20
lines and exits 1. Set `LUXGHA_DOCKER=optional` in the Run's env to continue without Docker instead.
`LUXGHA_DOCKER_WAIT` (whole seconds, no leading zero, default 60) changes the deadline. It exists for
`entrypoint_test.sh`; a Run should not need it.

Once `docker info` answers, the entrypoint runs `docker load -i /usr/share/lux-gha/buildkit.tar`, bounded by
`LUXGHA_BUILDKIT_LOAD_TIMEOUT` (whole seconds, default 30), and logs `lux-gha-runner: preloaded … in Ns`. If
the load fails or times out, it prints one warning line to stderr, leaves docker's output in
`/tmp/buildkit-load.log`, and starts the runner anyway. The builder then pulls `moby/buildkit` as it would
without the archive. `LUXGHA_BUILDKIT_ARCHIVE` overrides the path for `entrypoint_test.sh`; set it to `none`
in the Run's env to skip the load silently on routes that never use buildx (unset or empty means the default
path). If no Docker engine came up, nothing is loaded.

## In-region image registry

Only with `LUXGHA_REGISTRY_HOST` in the Run's env, which the bridge sets when it has a `registry` block
([README](../../README.md#in-region-image-registry)). Unset, the entrypoint does none of this.

- The entrypoint merges `"credHelpers": {"<host>": "lux-gha"}` into the runner user's
  `~/.docker/config.json` (or `$DOCKER_CONFIG/config.json`) with `jq`, keeping every other key. A file that is
  not a JSON object is left alone, with one warning line, and the hook is not set either.
- It sets `ACTIONS_RUNNER_HOOK_JOB_STARTED=/usr/local/lib/lux-gha/job-started.sh`, unless the Run's env
  already sets that variable (one warning line). The runner runs the hook before the job's first step. The
  hook POSTs the identity token to `$LUXGHA_RUNNER_ENDPOINT/runner/v1/registry` (curl, 3 s to connect, 10 s
  in all), retries `409` for `LUXGHA_REGISTRY_WAIT` seconds (default 20), and on `200` appends
  `LUX_REGISTRY` (`<host>/<owner>/<repo>`) and, when non-empty, `LUX_REGISTRY_PUSH` (the same, or
  `…/pr` for an untrusted job) to `$GITHUB_ENV`, after checking that both are lower-case paths under the host. It prints one line, `trust=<level>` or why it continued without, and **always exits 0**.
- `docker-credential-lux-gha` unsets every `AWS_*` variable of the job (credentials and profiles, but also
  `AWS_ENDPOINT_URL*`, `AWS_CA_BUNDLE`, `AWS_CONTAINER_CREDENTIALS_*`, `AWS_USE_FIPS_ENDPOINT` and the like,
  so a LocalStack setup cannot redirect it), sets `AWS_CONFIG_FILE=/etc/lux-gha/aws-config`,
  `AWS_PROFILE=lux-gha-registry`, `AWS_ECR_DISABLE_CACHE=true` and `AWS_REGION=$LUXGHA_REGISTRY_REGION`, and
  execs `docker-credential-ecr-login`. Only the helper's process is affected: the job's own AWS setup is
  untouched.
- The profile's `credential_process`, `lux-gha-aws-credentials`, POSTs the token to
  `/runner/v1/aws-credentials` with `curl --fail` and a 10 s limit. The token reaches curl as a config on
  stdin, never in argv.

The identity token lets any step of the job obtain the job's own registry credentials, which is the
access the job already has: the bridge decides it from GitHub's record of the job, not from the token.

`entrypoint_test.sh` covers the merge (existing keys kept, a fresh file, an unparsable file, a pre-set
`ACTIONS_RUNNER_HOOK_JOB_STARTED` kept with a warning, an absent hook file, and as root the runner user's
config owned by uid 1000), the hook on `200` (both variables, or `LUX_REGISTRY` alone for `read-only`, names
with `.`, `_` and `-`), on malformed answers in `read` or `push` (an injected line, another host, an
upper-case path, an empty segment, a trailing `/`, an unknown trust level), on a token holding a quote or a
newline, on `204`, `401`, `500`, `503`, on `409` until the wait runs out and `409` then `200`, on a refused
connection, and without host or token, each exiting 0 and writing nothing; the wrapper's environment
(including `AWS_ENDPOINT_URL`, `AWS_CONTAINER_CREDENTIALS_FULL_URI` and `AWS_CA_BUNDLE`); and the
credentials script on `200` and `503`. A fake `curl` stands in for the bridge; the token must appear in no
output and no argument.

## Build and smoke test

```sh
make image PLATFORM=linux/arm64            # or linux/amd64; loads lux-gha-runner:dev
images/runner/smoke.sh                     # tools, caps, runner version, browser deps, entrypoint lifecycle and failure modes
images/runner/smoke.sh --privileged        # also rootless dockerd, the BuildKit preload, docker-container builders offline and online
images/runner/smoke.sh --checksum-negative # a wrong runner SHA-256 must fail the build; rebuilds the runner layer
PLATFORM=linux/amd64 images/runner/smoke.sh  # the same for linux/amd64 (default linux/arm64)
```

`smoke.sh` builds for `PLATFORM` (`linux/arm64` by default, or `linux/amd64`) and checks that the image,
its `dpkg` architecture and the ELF machine of the runner's `bin/Runner.Listener` match it. Run it on a host
of that architecture; under emulation it may pass or fail for reasons that do not hold on real hardware.
It also checks the BuildKit archive offline against the Containerfile's pins (tags, index, `PLATFORM` manifest
and config, every blob's digest, no other platform's blobs), and that `/opt/hostedtoolcache` is owned by and
writable as `runner` and named by `RUNNER_TOOL_CACHE`. For each preinstalled tool it checks the directory and
`.complete` marker, ownership, and that the binary runs as `runner` and again under `env -i`
(`python3.11 -c 'import ssl, sqlite3'`, `java -version`, `node -v`), which catches a library found only through
`LD_LIBRARY_PATH`. It checks that the stripped files are absent, and that, offline, `import ssl, sqlite3, venv,
ensurepip` works and `python3.11 -m venv` creates a venv with pip. Under `--privileged` the entrypoint must log its
`preloaded` line, and the store must hold the pinned index and `PLATFORM` manifest under both tags. With
`--network none`, a `docker-container` builder must bootstrap from the preloaded image. With network,
`docker pull` of the pinned version tag must download nothing, and a builder pinned to it builds and runs an image.

For browser jobs, `smoke.sh` muxes `espeak-ng` speech with an `ffmpeg` test source into an H.264/AAC mp4 and
checks both streams. As `runner`, with network, it installs playwright-core 1.64.1 from npm and its Chromium
builds, as a job would, then requires `playwright install-deps --dry-run chromium` to print exactly
`All system dependencies are installed.` (no unsupported-distro/fallback warning), and `ldd` to find every
library of every ELF file in the browser cache. It launches Chromium headless
and headed under `xvfb-run`, each rendering a page, taking a screenshot and recording a video that `ffprobe`
reads back.

`entrypoint_test.sh` runs inside the image as part of `smoke.sh`, or on its own on Linux as a non-root
user with `sh images/runner/entrypoint_test.sh`, or anywhere with Docker as `make entrypoint-test` (uid 1000
in the pinned base image, against the working tree's `entrypoint.sh`). It drives the entrypoint with a fake `run.sh` and fake
`dockerd-rootless`/`docker`. It checks that `run.sh` replaces the entrypoint (exec), that its exit status
propagates, that a SIGTERM to the entrypoint's process group (how Lux stops a Run) reaches `run.sh` with
`RUNNER_MANUALLY_TRAP_SIG=1` set and leaves dockerd running, the bad-config, daemon-exit, never-ready and
hung-probe paths, and that the config never shows up in output or in the runner's environment. For the
BuildKit load it checks the order (`docker info`, `docker load`, `run.sh`), the default path when
`LUXGHA_BUILDKIT_ARCHIVE` is unset or empty (inside the image only), one warning line for a failed load, a
load hung past `LUXGHA_BUILDKIT_LOAD_TIMEOUT` while ignoring SIGTERM, or a bad timeout, and no load with
`none`, without an archive or without an engine.

The smoke cases that pass a fake config to the real runner only show that `run.sh` was reached with
invalid input. The `--privileged` checks are only an approximation, because Lux never runs a nested Run
privileged. Only a real nested Run proves that Docker works under Lux.
The real listener's job cancellation and log upload within `stopGrace` is
only verified live, against a real GitHub job; the signal harness uses a fake listener.
`make entrypoint-test` also checks uid 0 through real `setpriv`, dropping to
uid 1000; this path leaves Docker startup to the job.

CI runs `smoke.sh --privileged` for every pull request twice, natively: `linux/arm64` on `ubuntu-24.04-arm`
and `linux/amd64` on `ubuntu-24.04`, after setting `kernel.apparmor_restrict_unprivileged_userns=0` where the
key exists, so that rootlesskit can create its user namespace. A `v*` tag builds and smoke-tests each architecture the same way, pushes
each by digest only (`push-by-digest.sh`), then publishes `ghcr.io/<owner>/lux-gha-runner:<tag>` as an image
index over the two (`image-index.sh`; see [Releases](../../README.md#releases)). A route pins the index digest;
each Lux host pulls the image for its own architecture.

`push-by-digest.sh` rebuilds with the builder that built the tested image and fails unless the pushed image
has the tested image's platform, layer diff IDs and image config fields `User`, `ExposedPorts`, `Env`,
`Entrypoint`, `Cmd`, `Volumes`, `WorkingDir`, `Labels`, `StopSignal` and `ArgsEscaped`. It does not compare
`Created`, the container-only fields `docker image inspect` adds (`Hostname`, `Domainname`, `Image`, `Attach*`,
`Tty`, `OpenStdin`, `StdinOnce`), or `Shell`, `Healthcheck` and `OnBuild`, which `buildx imagetools inspect`
does not return. `image-index.sh` computes the index with `--dry-run`, checks that it holds exactly the
expected `platform=digest` pairs, pushes the tag, then reads the index back by digest and checks again.
CI's `publish` job runs both against a `registry:2` service (`publish_test.sh`): a one-platform arm64
index, an index that files the arm64 digest as amd64 (must fail before tagging), and a tested image that
differs from the push by one label (must fail).

## Route

```json
{"name": "arm", "labels": ["self-hosted", "linux", "arm64", "lux"], "pool": "gha-nested",
 "image": "ghcr.io/<owner>/lux-gha-runner@sha256:<digest>",
 "cpus": 4, "memoryMiB": 8192, "diskGiB": 40, "timeout": "90m",
 "nestedContainers": true, "egressHosts": ["..."]}
```

- `image`: the pushed image index, referenced by digest. The same digest serves arm64 and amd64 pools.
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
| `registry-1.docker.io`, `auth.docker.io`, `production.cloudfront.docker.com` | Docker Hub base images. With the preloaded BuildKit, `setup-buildx-action`'s `docker-container` builder still asks Docker Hub for the tag's index; the blobs come from the store (see [BuildKit driver](#buildkit-driver)) | Docker allowlist; blob redirect observed |

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
- the Ubuntu arm64 apt mirror, `ports.ubuntu.com` (on amd64: `archive.ubuntu.com` and `security.ubuntu.com`);
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

`docker/setup-buildx-action` defaults to the `docker-container` driver, whose builder runs
`moby/buildkit:buildx-stable-1`. Docker's store is the per-Run volume, so it starts empty. Without help,
every job would pull BuildKit (about 106 MB for arm64) anonymously from Docker Hub, which took 13 s on Lux and
counts against Docker Hub's anonymous pull limit, shared with every other pull from the same egress IP.

The image therefore carries `moby/buildkit` as an OCI archive and the entrypoint loads it at
start (above). Pulling into the store at build time would not work: Lux mounts the volume over
`~/.local/share/docker`, which hides anything the image put there. dockerd has no read-only
`additionalimagestores` option, which is a containers/storage feature of Podman and CRI-O. Rootless dockerd's
managed containerd also keeps its content store under that same data root.
The archive holds the multi-arch index that both `buildx-stable-1` and the version tag pointed to on 2026-10-15,
plus the image's own architecture only, tagged both ways. The loaded image's ID is therefore the index digest,
the same as after a pull from Docker Hub. The Containerfile pins the index and each architecture's
manifest by digest (`BUILDKIT_DIGEST`, `BUILDKIT_DIGEST_ARM64`, `BUILDKIT_DIGEST_AMD64`). The build fails if
the index's `org.opencontainers.image.version` annotation is not `BUILDKIT_VERSION`, if it lists another manifest
for `TARGETARCH`, or if any blob does not match its digest. The fetch is an anonymous Docker Hub pull at build time. Loading it took
about 2–3 s in `smoke.sh --privileged` on an Apple-silicon host.

buildx still asks dockerd to pull the builder image when the builder bootstraps, and uses the local image only
if that pull fails. With network, the pull resolves the tag at Docker Hub. If the tag still names the pinned
index, nothing is downloaded (`Image is up to date`). If `buildx-stable-1` has moved to a newer BuildKit,
the pull downloads it in full and the preload saves nothing. Workflows should therefore pin the version
the image carries, which its label `io.github.marcioapm.lux-gha.buildkit` names
(`moby/buildkit:<version>@<index digest>`). When a new image changes it, the workflows' pins must change too:

```yaml
- uses: docker/setup-buildx-action@v3
  with:
    driver-opts: image=moby/buildkit:v0.33.1
```

BuildKit version tags are not expected to move, so the pull only checks the tag; to rule that out, pin
`image=moby/buildkit:<version>@<index digest>` from the label. The lookup still goes to Docker Hub, so keep its
hosts in `egressHosts`. With no route to Docker Hub, the pull fails and buildx starts the preloaded image, as
long as `BUILDX_DEFAULT_POLICY` is unset.

This image runs Docker 29 with the containerd image store, so the `docker` driver uses the daemon's
embedded BuildKit: no BuildKit image at all, and `--load` goes straight into the store instead of being
exported by the BuildKit container and imported again by dockerd. Setting `driver: docker` in a workflow's
`setup-buildx-action` step is worth trying where its features suffice; that change belongs in the workflow.
