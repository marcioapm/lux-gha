#!/bin/sh
# docker-credential-lux-gha: the Docker credential helper the entrypoint
# names in credHelpers for the in-region registry host. It runs
# docker-credential-ecr-login with the bridge's credentials only: the job's
# own AWS environment is removed, and the lux-gha-registry profile gets its
# credentials from lux-gha-aws-credentials (credential_process). Nothing is
# exported to the job.
set -eu

region=${LUXGHA_REGISTRY_REGION:-}
[ -n "$region" ] || { echo "docker-credential-lux-gha: LUXGHA_REGISTRY_REGION is not set" >&2; exit 1; }

unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_PROFILE AWS_ROLE_ARN \
	AWS_WEB_IDENTITY_TOKEN_FILE AWS_CONFIG_FILE AWS_SHARED_CREDENTIALS_FILE AWS_DEFAULT_PROFILE
export AWS_CONFIG_FILE=/etc/lux-gha/aws-config
export AWS_PROFILE=lux-gha-registry
export AWS_ECR_DISABLE_CACHE=true
export AWS_REGION="$region"
exec docker-credential-ecr-login "$@"
