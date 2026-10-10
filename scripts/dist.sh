#!/usr/bin/env bash
# dist.sh builds the release tarballs and SHA256SUMS into dist/. Called by
# `make dist VERSION=vX.Y.Z`.
#
#   lux-gha_<version>_linux_{arm64,amd64}.tar.gz   bin/lux-gha, share/lux-gha/LICENSE
#
# Static, trimmed binaries versioned from VERSION (the release tag). The
# tarballs are reproducible: root-owned, sorted, and dated with the checked-out
# commit's date, so a rebuild of the same tag with the same Go toolchain and
# GNU gzip is byte-identical. Needs GNU tar (as `tar`, or `gtar` on macOS) and
# sha256sum.
set -euo pipefail

fail() { echo "dist.sh: $*" >&2; exit 1; }

VERSION="${VERSION:-}"
[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "VERSION must be vX.Y.Z, got '$VERSION'"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/dist"
# The Go linker ignores -X for a symbol that does not exist, so a wrong path
# here builds binaries that report "dev": checked per binary below, and
# TestVersionFlagPrintsLinkedVersion builds with these exact flags.
LDFLAGS="-s -w -X github.com/marcioapm/lux-gha/internal/version.Version=$VERSION"
if [ "${1:-}" = "--print-ldflags" ]; then
  printf '%s\n' "$LDFLAGS"
  exit 0
fi
HOST="$(go env GOHOSTOS)/$(go env GOHOSTARCH)"
MTIME="$(git -C "$ROOT" log -1 --format=%cI)"
# The runner image ships images/runner/LICENSE: a release must not differ.
sh "$ROOT/scripts/check-license.sh"

TAR=tar
if ! tar --version 2>/dev/null | grep -q 'GNU tar'; then
  command -v gtar >/dev/null || fail "GNU tar is required (tar or gtar)"
  TAR=gtar
fi
TAR_REPRO_FLAGS=(--owner=0 --group=0 --numeric-owner --sort=name --mtime="$MTIME")
WANT_MEMBERS=$'bin/\nbin/lux-gha\nshare/\nshare/lux-gha/\nshare/lux-gha/LICENSE'

rm -rf "$DIST"
mkdir -p "$DIST"

for arch in arm64 amd64; do
  name="lux-gha_${VERSION}_linux_${arch}.tar.gz"
  work="$DIST/work-linux-$arch"
  bin="$work/bin/lux-gha"
  mkdir -p "$work/bin" "$work/share/lux-gha"
  echo "building lux-gha for linux/$arch"
  # -buildvcs=false: in a git checkout Go would stamp the tag (or a
  # v0.0.0-<time>-<hash> pseudo-version) into the build info, which satisfies
  # the VERSION check whatever -X did.
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -C "$ROOT" -buildvcs=false -trimpath -ldflags "$LDFLAGS" \
    -o "$bin" ./cmd/lux-gha
  # -trimpath keeps -ldflags out of the build info, so the version string is in
  # the binary only if -X set it. Assert that premise rather than assume it.
  if go version -m "$bin" | grep -qF -- "$VERSION"; then
    fail "linux/$arch build info records $VERSION; the version check below would be vacuous"
  fi
  grep -qaF -- "$VERSION" "$bin" ||
    fail "linux/$arch binary does not contain $VERSION: is the -X path in LDFLAGS right?"
  if [ "$HOST" = "linux/$arch" ]; then
    got="$("$bin" --version)"
    [ "$got" = "$VERSION" ] || fail "linux/$arch binary reports '$got', want $VERSION"
  fi
  cp "$ROOT/LICENSE" "$work/share/lux-gha/LICENSE"
  chmod 0755 "$work/bin" "$work/share" "$work/share/lux-gha" "$bin"
  chmod 0644 "$work/share/lux-gha/LICENSE"
  # gzip -n: tar -z would let gzip stamp the header with the current time.
  "$TAR" "${TAR_REPRO_FLAGS[@]}" -C "$work" -cf - bin share | gzip -9 -n >"$DIST/$name"
  rm -rf "$work"

  # Check the tarball itself, not the tree it was made from.
  members="$("$TAR" -tzf "$DIST/$name")"
  [ "$members" = "$WANT_MEMBERS" ] ||
    fail "$(printf '%s has members:\n%s\nwant:\n%s' "$name" "$members" "$WANT_MEMBERS")"
  "$TAR" -xzOf "$DIST/$name" share/lux-gha/LICENSE | cmp -s - "$ROOT/LICENSE" ||
    fail "$name: share/lux-gha/LICENSE differs from LICENSE"
done

(cd "$DIST" && sha256sum lux-gha_*.tar.gz >SHA256SUMS)

echo "dist/:"
ls -la "$DIST"
