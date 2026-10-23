#!/bin/sh
# lux-gha job-started hook (ACTIONS_RUNNER_HOOK_JOB_STARTED). It asks the
# bridge's runner listener for the in-region registry paths of the job this
# runner is executing and, on 200, appends LUX_REGISTRY and, when the job
# may push, LUX_REGISTRY_PUSH to $GITHUB_ENV. A 409 (GitHub has not yet
# shown the job on this runner) is retried for LUXGHA_REGISTRY_WAIT seconds
# (default 20). Every other outcome writes nothing. It always exits 0: a
# non-zero hook fails the job, and the registry is an optimisation the
# workflow falls back from. It prints one line and never the token.

say() { echo "lux-gha registry: $*"; }

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
		[ "$code" = 409 ] && [ "$(date +%s)" -lt "$deadline" ] || break
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

main || true
exit 0
