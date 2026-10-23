#!/bin/sh
# lux-gha job-started hook (ACTIONS_RUNNER_HOOK_JOB_STARTED).
#
# Cache server: with LUXGHA_ACTIONS_RESULTS_URL set and the runner's
# Runner.Worker.dll patched (patch-worker.sh, applied before each listener
# start), it appends ACTIONS_RESULTS_URL to $GITHUB_ENV so that every step,
# including steps in a job container, talks to the cache server. An
# unpatched Worker would overwrite that value for actions, so it then writes
# nothing and warns that the job uses GitHub's cache service.
#
# Registry: it asks the bridge's runner listener for the in-region registry
# paths of the job this runner is executing and, on 200, appends
# LUX_REGISTRY and, when the job may push, LUX_REGISTRY_PUSH to $GITHUB_ENV.
# A 409 (GitHub has not yet shown the job on this runner) is retried for
# LUXGHA_REGISTRY_WAIT seconds (default 20). Every other outcome writes
# nothing. It always exits 0: a non-zero hook fails the job, and the registry
# is an optimisation the workflow falls back from. It prints one line and
# never the token.

say() { echo "lux-gha registry: $*"; }

# no_cache REASON: the workflow warning for a job that uses GitHub's cache
# service instead of the Run's cache server.
no_cache() { echo "::warning::lux-gha: $1; this job uses GitHub's cache service"; }

# cache_url_ok URL: http(s), URL characters only, so no newline or space can
# add a second variable to GITHUB_ENV (the same test as in entrypoint.sh).
cache_url_ok() {
	case $1 in http://* | https://*) ;; *) return 1 ;; esac
	case $1 in *[!]A-Za-z0-9:/._~%[-]*) return 1 ;; esac
}

cache() {
	url=${LUXGHA_ACTIONS_RESULTS_URL:-}
	[ -n "$url" ] || return 0
	# The bridge validated it and the entrypoint checked it; this keeps a
	# malformed value out of GITHUB_ENV all the same.
	if ! cache_url_ok "$url"; then
		no_cache "this Run's cache server URL is malformed"
		return 0
	fi
	if [ -z "${GITHUB_ENV:-}" ]; then
		no_cache "GITHUB_ENV is not set"
		return 0
	fi
	dll=${RUNNER_DIR:-/home/runner/actions-runner}/bin/Runner.Worker.dll
	counts=$("${LUXGHA_PATCH_WORKER:-/usr/local/lib/lux-gha/patch-worker.sh}" count "$dll" 2>/dev/null) || counts=
	case $counts in
	"0 "[1-9]*) ;;
	*)
		no_cache "this runner's Runner.Worker.dll is not patched"
		return 0
		;;
	esac
	echo "ACTIONS_RESULTS_URL=$url" >>"$GITHUB_ENV" || {
		no_cache "could not write GITHUB_ENV"
		return 0
	}
	echo "lux-gha cache: $url"
}

main() {
	host=${LUXGHA_REGISTRY_HOST:-}
	[ -n "$host" ] || return 0
	endpoint=${LUXGHA_RUNNER_ENDPOINT:-}
	file=${LUXGHA_IDENTITY_FILE:-}
	if [ -z "$endpoint" ] || [ ! -r "$file" ] || [ -z "${GITHUB_ENV:-}" ]; then
		say "not configured for this Run; continuing without it"
		return 0
	fi
	token=$(cat "$file") || token=
	case $token in '' | *[!A-Za-z0-9._-]*)
		say "identity token unreadable; continuing without it"
		return 0 ;;
	esac
	wait_s=${LUXGHA_REGISTRY_WAIT:-20}
	case $wait_s in '' | *[!0-9]*) wait_s=20 ;; esac
	body=$(mktemp) || return 0
	trap 'rm -f "$body"' EXIT
	deadline=$(($(date +%s) + wait_s))
	while :; do
		code=$(printf 'header = "Authorization: Bearer %s"\n' "$token" |
			curl --config - --silent --output "$body" --write-out '%{http_code}' \
				--connect-timeout 3 --max-time 10 -X POST "$endpoint/runner/v1/registry" 2>/dev/null) || code=${code:-000}
		if [ "$code" != 409 ] || [ "$(date +%s)" -ge "$deadline" ]; then
			break
		fi
		sleep 1
	done
	if [ "$code" != 200 ]; then
		say "unavailable (HTTP ${code:-000}); continuing without it"
		return 0
	fi
	trust=$(jq -r '.trust // empty' "$body" 2>/dev/null) || trust=
	read_path=$(jq -r '.read // empty' "$body" 2>/dev/null) || read_path=
	push_path=$(jq -r '.push // empty' "$body" 2>/dev/null) || push_path=
	# Both land in $GITHUB_ENV: one line each, under the configured host.
	case $trust in trusted | untrusted | read-only) ;; *) trust= ;; esac
	if [ -z "$trust" ] || ! valid_path "$read_path" || { [ -n "$push_path" ] && ! valid_path "$push_path"; }; then
		say "unexpected answer; continuing without it"
		return 0
	fi
	{
		echo "LUX_REGISTRY=$read_path"
		if [ -n "$push_path" ]; then echo "LUX_REGISTRY_PUSH=$push_path"; fi
	} >>"$GITHUB_ENV" || { say "could not write GITHUB_ENV; continuing without it"; return 0; }
	say "trust=$trust"
}

valid_path() {
	case $1 in "$host"/*) ;; *) return 1 ;; esac
	case ${1#"$host"/} in '' | *[!a-z0-9._/-]* | */ | /* | *//*) return 1 ;; esac
}

cache || true
main || true
exit 0
