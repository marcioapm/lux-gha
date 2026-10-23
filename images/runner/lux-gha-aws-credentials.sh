#!/bin/sh
# lux-gha-aws-credentials: the credential_process of the lux-gha-registry
# AWS profile. It prints the bridge's credential_process JSON for the job
# this runner executes. The identity token goes to curl on stdin, never in
# argv, and is never printed.
set -eu

endpoint=${LUXGHA_RUNNER_ENDPOINT:-}
file=${LUXGHA_IDENTITY_FILE:-}
if [ -z "$endpoint" ] || [ ! -r "$file" ]; then
	echo "lux-gha-aws-credentials: LUXGHA_RUNNER_ENDPOINT or LUXGHA_IDENTITY_FILE is not set" >&2
	exit 1
fi
token=$(cat "$file")
case $token in '' | *[!A-Za-z0-9._-]*) echo "lux-gha-aws-credentials: identity token is malformed" >&2; exit 1 ;; esac
printf 'header = "Authorization: Bearer %s"\n' "$token" |
	curl --config - --fail --silent --show-error --connect-timeout 3 --max-time 10 \
		-X POST "$endpoint/runner/v1/aws-credentials"
