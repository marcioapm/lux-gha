#!/bin/sh
# Reference entrypoint for a lux-gha runner image: start the GitHub Actions
# runner once with the JIT config Lux placed as a file secret. The runner
# takes at most one job, deregisters itself and exits; the Run ends with it.
#
# When dockerd-rootless is installed and DOCKER_HOST is a unix socket, a
# rootless Docker engine is started first for the job's steps, listening on
# DOCKER_HOST. It must answer `docker info` within LUXGHA_DOCKER_WAIT seconds
# (default 60) and is required unless LUXGHA_DOCKER=optional.
set -eu

RUNNER_DIR=${RUNNER_DIR:-/home/runner/actions-runner}
RUNNER_USER=${RUNNER_USER:-runner}
config_file=${LUXGHA_JITCONFIG_FILE:?LUXGHA_JITCONFIG_FILE is not set}

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
	setsid "$dockerd" --host "$DOCKER_HOST" </dev/null >"$log" 2>&1 &
	pid=$!
	deadline=$(($(date +%s) + wait_s))
	while kill -0 "$pid" 2>/dev/null; do
		left=$((deadline - $(date +%s)))
		[ "$left" -gt 0 ] || break
		# A daemon that accepts the connection but never answers must not
		# outlast the deadline: each probe gets at most 5 s.
		[ "$left" -le 5 ] || left=5
		timeout -k 1 "$left" docker info >/dev/null 2>&1 && return 0
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
