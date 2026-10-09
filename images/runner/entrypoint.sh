#!/bin/sh
# Reference entrypoint for a lux-gha runner image: start the GitHub Actions
# runner once with the JIT config Lux placed as a file secret. The runner
# takes at most one job, deregisters itself and exits; the Run ends with it.
#
# When dockerd-rootless is installed and DOCKER_HOST is a unix socket, a
# rootless Docker engine is started first for the job's steps. It is required
# unless LUXGHA_DOCKER=optional.
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
	log=/tmp/dockerd.log
	mkdir -p "${XDG_RUNTIME_DIR:?XDG_RUNTIME_DIR is not set}"
	chmod 700 "$XDG_RUNTIME_DIR"
	# Own session, so a signal to the runner's process group does not reach it.
	setsid "$dockerd" </dev/null >"$log" 2>&1 &
	pid=$!
	i=0
	while [ "$i" -lt 60 ] && kill -0 "$pid" 2>/dev/null; do
		docker info >/dev/null 2>&1 && return 0
		sleep 1
		i=$((i + 1))
	done
	echo "lux-gha-runner: rootless dockerd did not come up within 60s; last lines of $log:" >&2
	tail -n 20 "$log" >&2 || true
	if [ "${LUXGHA_DOCKER:-required}" = optional ]; then
		echo "lux-gha-runner: continuing without Docker (LUXGHA_DOCKER=optional)" >&2
		return 0
	fi
	exit 1
}

start_docker

cd "$RUNNER_DIR"
# The runner refuses to run as root; the config reaches it as an argument
# only, never through the environment of the job's steps.
if [ "$(id -u)" -eq 0 ]; then
	exec setpriv --reuid="$RUNNER_USER" --regid="$RUNNER_USER" --init-groups ./run.sh --jitconfig "$jitconfig"
fi
exec ./run.sh --jitconfig "$jitconfig"
