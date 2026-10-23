# shellcheck shell=bash
# Sourced by bash at the start of every non-interactive bash script, through
# BASH_ENV, which the entrypoint sets when the Run has a cache server
# (README "Using the cache server").
#
# run.sh restarts run-helper.sh, which starts the listener, after every
# self-update; an update replaces bin/ and every script in the runner
# directory, run-helper.sh.template included, before the job runs. BASH_ENV
# is in the environment, so no update removes it: each run-helper.sh start
# patches the Worker that bin/ names now.
#
# It acts only in run-helper.sh, and unsets BASH_ENV there, so the listener,
# the Worker and every job step start without it. A failed patch leaves the
# Worker as shipped; the job-started hook then warns that the job uses
# GitHub's cache service.
if [ "${0##*/}" = run-helper.sh ]; then
	unset BASH_ENV
	"${LUXGHA_PATCH_WORKER:-/usr/local/lib/lux-gha/patch-worker.sh}" apply "$(dirname "$0")/bin/Runner.Worker.dll" >&2 ||
		echo "lux-gha: warning: Runner.Worker.dll is not patched; jobs use GitHub's cache service" >&2
fi
