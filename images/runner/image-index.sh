#!/bin/sh
# Publishes the tag $IMAGE as an image index over per-platform image digests
# in $REPO. $PLATFORMS lists the platforms (linux/arm64, linux/amd64) the index
# must hold, exactly; the digest for linux/<arch> is read from the file
# $DIGESTS/<arch>. The index is computed with --dry-run and its platform=digest
# set checked before the tag is pushed, then read back by its digest and
# checked again. Writes the index reference, $REPO@sha256:..., to the file
# $OUT if set.
#
# Usage: REPO=... IMAGE=... DIGESTS=dir PLATFORMS="linux/arm64 linux/amd64" [OUT=...] images/runner/image-index.sh
set -eu

: "${REPO:?REPO is not set}" "${IMAGE:?IMAGE is not set}" "${DIGESTS:?DIGESTS is not set}" "${PLATFORMS:?PLATFORMS is not set}"

fail() { echo "$*" >&2; exit 1; }
# set_of: the sorted platform=digest pairs of the index JSON on stdin.
set_of() {
	jq -r 'if .mediaType == "application/vnd.oci.image.index.v1+json" or .mediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
		then [.manifests[] | "\(.platform.os)/\(.platform.architecture)=\(.digest)"] | sort | join(" ")
		else "not an index: \(.mediaType)" end'
}

pairs=
set --
for p in $PLATFORMS; do
	case $p in
	linux/arm64 | linux/amd64) ;;
	*) fail "PLATFORMS holds $p; want linux/arm64 or linux/amd64" ;;
	esac
	d=$(cat "$DIGESTS/${p#linux/}") || fail "no digest file for $p"
	printf '%s\n' "$d" | grep -Eqx 'sha256:[0-9a-f]{64}' || fail "$DIGESTS/${p#linux/} holds \"$d\", not a sha256 digest"
	pairs="$pairs $p=$d"
	set -- "$@" "$REPO@$d"
done
# shellcheck disable=SC2086 # one pair per word
want=$(printf '%s\n' $pairs | jq -Rrn '[inputs] | sort | join(" ")')

got=$(docker buildx imagetools create --dry-run "$@" | set_of)
[ "$got" = "$want" ] || fail "index would hold $got, want $want; $IMAGE not pushed"

docker buildx imagetools create -t "$IMAGE" "$@"
digest=$(docker buildx imagetools inspect "$IMAGE" --format '{{.Manifest.Digest}}')
printf '%s\n' "$digest" | grep -Eqx 'sha256:[0-9a-f]{64}' || fail "$IMAGE has no index digest: $digest"
# By digest: that is what a route pins, and the tag may move.
got=$(docker buildx imagetools inspect "$REPO@$digest" --raw | set_of)
[ "$got" = "$want" ] || fail "pushed index $REPO@$digest holds $got, want $want"

[ -z "${OUT:-}" ] || printf '%s\n' "$REPO@$digest" >"$OUT"
echo "pushed $IMAGE = $REPO@$digest ($got)"
