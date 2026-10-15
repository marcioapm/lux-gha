#!/bin/sh
# Pushes the runner image for $PLATFORM to $REPO by digest only, with no tag,
# and writes that digest (sha256:...) to the file $OUT.
#
# The push is a rebuild with the current buildx builder. Run it with the
# builder that built $TAG (smoke.sh --keep), whose cache then serves the
# rebuild. The script fails unless the pushed image equals $TAG, the tested
# image, in platform, layer diff IDs and these config fields: User,
# ExposedPorts, Env, Entrypoint, Cmd, Volumes, WorkingDir, Labels,
# StopSignal, ArgsEscaped (the OCI image config; unset and empty compare
# equal). Not compared: Created; Hostname, Domainname, Image, Attach*, Tty,
# OpenStdin and StdinOnce, which `docker image inspect` reports but an image
# config does not hold; Shell, Healthcheck and OnBuild, which `buildx
# imagetools inspect` does not return.
#
# Usage: REPO=... PLATFORM=linux/arm64|linux/amd64 TAG=... OUT=... images/runner/push-by-digest.sh
set -eu

: "${REPO:?REPO is not set}" "${PLATFORM:?PLATFORM is not set}" "${TAG:?TAG is not set}" "${OUT:?OUT is not set}"
dir=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# No provenance or SBOM: the index then holds exactly one image manifest per
# architecture.
docker buildx build --platform "$PLATFORM" --provenance=false --sbom=false \
	--output "type=image,name=$REPO,push-by-digest=true,name-canonical=true,push=true,oci-mediatypes=true" \
	--metadata-file "$tmp/meta.json" -f "$dir/Containerfile" "$dir"
digest=$(jq -r '."containerimage.digest" // empty' "$tmp/meta.json")
printf '%s\n' "$digest" | grep -Eqx 'sha256:[0-9a-f]{64}' || { echo "no digest in the build metadata: $digest" >&2; exit 1; }

oci='def oci: {User, ExposedPorts, Env, Entrypoint, Cmd, Volumes, WorkingDir, Labels, StopSignal, ArgsEscaped}
	| with_entries(select(.value | . != null and . != "" and . != false and . != [] and . != {}));'
docker image inspect "$TAG" | jq -S "$oci"'.[0] | {platform: "\(.Os)/\(.Architecture)",
	variant: (.Variant // ""), layers: .RootFS.Layers, config: (.Config | oci)}' >"$tmp/tested"
docker buildx imagetools inspect "$REPO@$digest" --format '{{json .Image}}' | jq -S "$oci"'{platform: "\(.os)/\(.architecture)",
	variant: (.variant // ""), layers: .rootfs.diff_ids, config: (.config | oci)}' >"$tmp/pushed"
if ! cmp -s "$tmp/tested" "$tmp/pushed"; then
	echo "pushed $REPO@$digest differs from the smoke-tested image $TAG (< tested, > pushed):" >&2
	diff "$tmp/tested" "$tmp/pushed" >&2 || true
	exit 1
fi
[ "$(jq -r .platform "$tmp/pushed")" = "$PLATFORM" ] || { echo "pushed $REPO@$digest is $(jq -r .platform "$tmp/pushed"), want $PLATFORM" >&2; exit 1; }
printf '%s\n' "$digest" >"$OUT"
echo "pushed $REPO@$digest ($PLATFORM): platform, layers and compared config fields equal $TAG's"
