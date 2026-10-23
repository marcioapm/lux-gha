#!/bin/sh
# Reference entrypoint for a lux-gha runner image: start the GitHub Actions
# runner once with the JIT config Lux placed as a file secret. The runner
# takes at most one job, deregisters itself and exits; the Run ends with it.
#
# When dockerd-rootless is installed and DOCKER_HOST is a unix socket, a
# rootless Docker engine is started first for the job's steps, listening on
# DOCKER_HOST. It must answer `docker info` within LUXGHA_DOCKER_WAIT seconds
# (default 60) and is required unless LUXGHA_DOCKER=optional. Its containers
# reach the runner's own loopback through host-gateway (host.docker.internal),
# as they do with rootful Docker: see start_docker.
#
# Once the engine answers, the BuildKit image archive the image ships (if
# any) is loaded into its store, so that setup-buildx-action's
# docker-container builder does not pull moby/buildkit. A failed load only
# warns: the builder then pulls as it would without the archive.
# LUXGHA_BUILDKIT_ARCHIVE=none skips it; unset or empty means the default.
#
# With LUXGHA_REGISTRY_HOST set, it then merges a credHelpers entry for that
# host into the runner user's Docker config and points the runner's
# job-started hook at job-started.sh: see setup_registry and setup_hook.
set -eu

RUNNER_DIR=${RUNNER_DIR:-/home/runner/actions-runner}
RUNNER_USER=${RUNNER_USER:-runner}
config_file=${LUXGHA_JITCONFIG_FILE:?LUXGHA_JITCONFIG_FILE is not set}
docker_up=false

[ -r "$config_file" ] || { echo "lux-gha-runner: JIT config file is not readable" >&2; exit 1; }
jitconfig=$(cat "$config_file")
[ -n "$jitconfig" ] || { echo "lux-gha-runner: JIT config file is empty" >&2; exit 1; }

# Rootless dockerd needs an unprivileged user; as root the runner drops to
# RUNNER_USER below and the engine is left to the job.
start_docker() {
	# Docker's deb packages name it dockerd-rootless.sh; Alpine's drop the .sh.
	dockerd=$(command -v dockerd-rootless || command -v dockerd-rootless.sh) || return 0
	case ${DOCKER_HOST:-} in unix://*) ;; *) return 0 ;; esac
	[ "$(id -u)" -ne 0 ] || return 0
	# Seconds for the whole wait, probes included. Tests shorten it.
	wait_s=${LUXGHA_DOCKER_WAIT:-60}
	# Decimal digits, no leading zero: sh arithmetic reads 08 as bad octal.
	case $wait_s in '' | *[!0-9]* | 0*) echo "lux-gha-runner: LUXGHA_DOCKER_WAIT must be a positive integer without a leading zero" >&2; exit 1 ;; esac
	log=/tmp/dockerd.log
	mkdir -p "${XDG_RUNTIME_DIR:?XDG_RUNTIME_DIR is not set}"
	chmod 700 "$XDG_RUNTIME_DIR"
	# Own session, so a signal to the runner's process group does not reach it.
	# --host makes dockerd listen where the CLI and the job's steps look.
	# Rootless defaults block a container from the runner's 127.0.0.1, and its
	# host-gateway is an address the runner does not have. slirp4netns maps
	# 10.0.2.2 to the runner's loopback once host loopback is allowed, so
	# host.docker.internal:host-gateway reaches a server the job bound to
	# 127.0.0.1. That loopback is the Run's own network namespace, not the host's.
	DOCKERD_ROOTLESS_ROOTLESSKIT_NET=slirp4netns \
		DOCKERD_ROOTLESS_ROOTLESSKIT_DISABLE_HOST_LOOPBACK=false \
		setsid "$dockerd" --host "$DOCKER_HOST" --host-gateway-ip=10.0.2.2 </dev/null >"$log" 2>&1 &
	pid=$!
	deadline=$(($(date +%s) + wait_s))
	while kill -0 "$pid" 2>/dev/null; do
		left=$((deadline - $(date +%s)))
		[ "$left" -gt 0 ] || break
		# A daemon that accepts the connection but never answers must not
		# outlast the deadline: each probe gets at most 5 s.
		[ "$left" -le 5 ] || left=5
		timeout -k 1 "$left" docker info >/dev/null 2>&1 && { docker_up=true; return 0; }
		sleep 1
	done
	echo "lux-gha-runner: rootless dockerd did not come up within ${wait_s}s; last lines of $log:" >&2
	tail -n 20 "$log" >&2 || true
	if [ "${LUXGHA_DOCKER:-required}" = optional ]; then
		echo "lux-gha-runner: continuing without Docker (LUXGHA_DOCKER=optional)" >&2
		return 0
	fi
	exit 1
}

start_docker

# The store is the per-Run volume Lux mounts over ~/.local/share/docker, so
# the image cannot carry the BuildKit image in it; it carries an archive.
load_buildkit() {
	archive=${LUXGHA_BUILDKIT_ARCHIVE:-/usr/share/lux-gha/buildkit.tar}
	[ "$archive" != none ] || return 0
	[ -f "$archive" ] || return 0
	limit=${LUXGHA_BUILDKIT_LOAD_TIMEOUT:-30}
	log=/tmp/buildkit-load.log
	case $limit in '' | *[!0-9]* | 0*)
		echo "lux-gha-runner: warning: LUXGHA_BUILDKIT_LOAD_TIMEOUT must be a positive integer without a leading zero; BuildKit not preloaded" >&2
		return 0 ;;
	esac
	start=$(date +%s)
	if timeout -k 2 "$limit" docker load -i "$archive" >"$log" 2>&1; then
		echo "lux-gha-runner: preloaded $(sed -n 's/^Loaded image: //p' "$log" | paste -sd ' ' -) in $(($(date +%s) - start))s"
	else
		echo "lux-gha-runner: warning: docker load -i $archive failed or exceeded ${limit}s (see $log); setup-buildx will pull moby/buildkit" >&2
	fi
}

if $docker_up; then load_buildkit; fi

# With LUXGHA_REGISTRY_HOST (set by the bridge when it has a registry), the
# runner user's Docker config names docker-credential-lux-gha for that host,
# which covers the runner's own services:/container: pulls too, and the
# job-started hook exports the job's registry paths. Neither step can stop
# the runner: the registry is an optimisation and jobs fall back without it.
setup_registry() {
	host=${LUXGHA_REGISTRY_HOST:-}
	[ -n "$host" ] || return 0
	home=$HOME
	if [ "$(id -u)" -eq 0 ]; then
		home=$(getent passwd "$RUNNER_USER" | cut -d: -f6) || home=
	fi
	dir=${DOCKER_CONFIG:-$home/.docker}
	cfg=$dir/config.json
	if [ -z "$home" ] || ! command -v jq >/dev/null; then
		echo "lux-gha-runner: warning: cannot set up the registry credential helper; jobs run without LUX_REGISTRY" >&2
		return 0
	fi
	mkdir -p "$dir"
	[ -s "$cfg" ] || echo '{}' >"$cfg"
	if jq --arg h "$host" '.credHelpers = ((.credHelpers // {}) + {($h): "lux-gha"})' "$cfg" >"$cfg.tmp" 2>/dev/null; then
		mv "$cfg.tmp" "$cfg"
	else
		rm -f "$cfg.tmp"
		echo "lux-gha-runner: warning: $cfg is not a JSON object; left unchanged, jobs run without LUX_REGISTRY" >&2
		return 0
	fi
	if [ "$(id -u)" -eq 0 ]; then chown -R "$RUNNER_USER:" "$dir"; fi
	registry_ready=true
}

# The job-started hook exports LUX_REGISTRY (when the registry is set up)
# and points the job at the Actions cache server (when the bridge set
# LUXGHA_ACTIONS_RESULTS_URL; README "Using the cache server"). A hook the
# image's own environment already names is kept.
setup_hook() {
	[ "$registry_ready" = true ] || [ -n "${LUXGHA_ACTIONS_RESULTS_URL:-}" ] || return 0
	hook=${LUXGHA_JOB_STARTED_HOOK:-/usr/local/lib/lux-gha/job-started.sh}
	if [ -n "${ACTIONS_RUNNER_HOOK_JOB_STARTED:-}" ]; then
		echo "lux-gha-runner: warning: ACTIONS_RUNNER_HOOK_JOB_STARTED is already set; jobs run without LUX_REGISTRY and the cache server" >&2
	elif [ -f "$hook" ]; then
		export ACTIONS_RUNNER_HOOK_JOB_STARTED="$hook"
	fi
}

registry_ready=false
setup_registry || echo "lux-gha-runner: warning: registry setup failed; jobs run without LUX_REGISTRY" >&2
setup_hook
cd "$RUNNER_DIR"
# run.sh forwards SIGTERM to the listener as SIGINT only with this set; the
# listener then cancels the running job and uploads its logs before exiting.
# Lux stops a Run (disk limit, timeout, preemption, drain, terminate) with
# SIGTERM to this process's group (exec makes this run.sh), then SIGKILL after
# workload.grace. dockerd runs in its own session and keeps serving the
# job's containers while it is cancelled.
export RUNNER_MANUALLY_TRAP_SIG=1
# The runner refuses to run as root; the config reaches it as an argument
# only, never through the environment of the job's steps.
if [ "$(id -u)" -eq 0 ]; then
	exec setpriv --reuid="$RUNNER_USER" --regid="$RUNNER_USER" --init-groups ./run.sh --jitconfig "$jitconfig"
fi
exec ./run.sh --jitconfig "$jitconfig"
