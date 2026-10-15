#!/bin/sh
# Runs the release's publish scripts against a throwaway registry: $REPO, e.g.
# localhost:5000/lux-gha-runner, with the image smoke.sh --keep left as $TAG
# for $PLATFORM, and the buildx builder that built it.
#
# - push-by-digest.sh pushes the image and finds it equal to $TAG.
# - image-index.sh publishes $REPO:publish-test as an index over that one
#   digest, with PLATFORMS=$PLATFORM. A two-platform index needs both
#   architectures' digests in one job; the release's index job has them, and
#   the per-platform checks are the same code.
# - image-index.sh with that digest filed under the other architecture must
#   fail at the --dry-run check and leave $REPO:publish-test-swapped absent.
# - An image that differs from the build in one label only, given as the
#   tested image, must make push-by-digest.sh fail.
#
# Usage: REPO=... PLATFORM=linux/arm64|linux/amd64 TAG=... images/runner/publish_test.sh
set -eu

: "${REPO:?REPO is not set}" "${PLATFORM:?PLATFORM is not set}" "${TAG:?TAG is not set}"
case $PLATFORM in
linux/arm64) arch=arm64 other=amd64 ;;
linux/amd64) arch=amd64 other=arm64 ;;
*) echo "PLATFORM must be linux/arm64 or linux/amd64, not $PLATFORM" >&2; exit 2 ;;
esac
dir=$(cd "$(dirname "$0")" && pwd)
relabelled=lux-gha-runner:publish-test-relabelled
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"; docker image rm "$relabelled" >/dev/null 2>&1 || true' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

echo "== push-by-digest.sh"
mkdir "$tmp/digest"
OUT=$tmp/digest/$arch "$dir/push-by-digest.sh"
digest=$(cat "$tmp/digest/$arch")

echo "== image-index.sh, one platform"
IMAGE=$REPO:publish-test DIGESTS=$tmp/digest PLATFORMS=$PLATFORM OUT=$tmp/ref "$dir/image-index.sh"
ref=$(cat "$tmp/ref")
[ "$ref" = "$REPO@$(docker buildx imagetools inspect "$REPO:publish-test" --format '{{.Manifest.Digest}}')" ] ||
	fail "OUT does not hold the index reference"
[ "$(docker buildx imagetools inspect "$ref" --raw | jq -r '[.manifests[].digest]|join(" ")')" = "$digest" ] ||
	fail "the index $ref does not name exactly the pushed $digest"

echo "== image-index.sh, $PLATFORM digest filed as linux/$other (must fail before tagging)"
mkdir "$tmp/swapped"
printf '%s\n' "$digest" >"$tmp/swapped/$other"
if out=$(IMAGE=$REPO:publish-test-swapped DIGESTS=$tmp/swapped PLATFORMS=linux/$other "$dir/image-index.sh" 2>&1); then
	fail "image-index.sh accepted a $PLATFORM digest as linux/$other"
fi
echo "$out"
echo "$out" | grep -qF "index would hold $PLATFORM=$digest, want linux/$other=$digest; $REPO:publish-test-swapped not pushed" || fail "image-index.sh failed, but not at the --dry-run check"
if docker buildx imagetools inspect "$REPO:publish-test-swapped" >/dev/null 2>&1; then
	fail "$REPO:publish-test-swapped was pushed"
fi

echo "== push-by-digest.sh, tested image differs in one label (must fail)"
docker buildx build --platform "$PLATFORM" --load --label lux-gha.publish-test=differs -t "$relabelled" \
	-f "$dir/Containerfile" "$dir"
if out=$(TAG=$relabelled OUT=$tmp/negative "$dir/push-by-digest.sh" 2>&1); then
	fail "push-by-digest.sh accepted a tested image that differs from the pushed one"
fi
echo "$out" | tail -n 8
echo "$out" | grep -q "differs from the smoke-tested image" || fail "push-by-digest.sh failed, but not at the comparison"
echo "$out" | grep -q "lux-gha.publish-test" || fail "the comparison did not report the differing label"
[ ! -e "$tmp/negative" ] || fail "push-by-digest.sh wrote a digest for a mismatched image"
echo "publish_test: OK"
