#!/bin/sh
# docker-credential-lux-gha: the Docker credential helper the entrypoint
# names in credHelpers for the in-region registry host. It runs
# docker-credential-ecr-login with the bridge's credentials only: every AWS_*
# variable of the job is removed (credentials, profiles, endpoint, CA bundle
# and container-credential overrides alike), and the lux-gha-registry
# profile gets its credentials from lux-gha-aws-credentials
# (credential_process). Nothing is exported to the job.
set -eu

region=${LUXGHA_REGISTRY_REGION:-}
[ -n "$region" ] || { echo "docker-credential-lux-gha: LUXGHA_REGISTRY_REGION is not set" >&2; exit 1; }

for v in $(env | sed -n 's/^\(AWS_[A-Za-z0-9_]*\)=.*/\1/p'); do unset "$v"; done
export AWS_CONFIG_FILE=/etc/lux-gha/aws-config
export AWS_PROFILE=lux-gha-registry
export AWS_ECR_DISABLE_CACHE=true
export AWS_REGION="$region"
exec docker-credential-ecr-login "$@"
