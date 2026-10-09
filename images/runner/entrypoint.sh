#!/bin/sh
# Reference entrypoint for a lux-gha runner image: start the GitHub Actions
# runner once with the JIT config Lux placed as a file secret. The runner
# takes at most one job, deregisters itself and exits; the Run ends with it.
set -eu

RUNNER_DIR=${RUNNER_DIR:-/home/runner/actions-runner}
RUNNER_USER=${RUNNER_USER:-runner}
config_file=${LUXGHA_JITCONFIG_FILE:?LUXGHA_JITCONFIG_FILE is not set}

[ -r "$config_file" ] || { echo "lux-gha-runner: JIT config file is not readable" >&2; exit 1; }
jitconfig=$(cat "$config_file")
[ -n "$jitconfig" ] || { echo "lux-gha-runner: JIT config file is empty" >&2; exit 1; }

cd "$RUNNER_DIR"
# The runner refuses to run as root; the config reaches it as an argument
# only, never through the environment of the job's steps.
if [ "$(id -u)" -eq 0 ]; then
	exec setpriv --reuid="$RUNNER_USER" --regid="$RUNNER_USER" --init-groups ./run.sh --jitconfig "$jitconfig"
fi
exec ./run.sh --jitconfig "$jitconfig"
