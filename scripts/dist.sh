#!/usr/bin/env bash
# dist.sh builds the release tarballs and SHA256SUMS into dist/. Called by
# `make dist VERSION=vX.Y.Z`.
#
#   lux-gha_<version>_linux_{arm64,amd64}.tar.gz   bin/lux-gha, share/lux-gha/LICENSE
#
# Static, trimmed binaries versioned from VERSION (the release tag). The
# tarballs are reproducible: root-owned, sorted, and dated with the checked-out
# commit's date, so a rebuild of the same tag is byte-identical. Needs GNU tar
# (as `tar`, or `gtar` on macOS) and sha256sum.
set -euo pipefail

VERSION="${VERSION:-}"
if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "dist.sh: VERSION must be vX.Y.Z, got '$VERSION'" >&2
  exit 1
fi
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/dist"
LDFLAGS="-s -w -X github.com/marcioapm/lux-gha/internal/version.Version=$VERSION"
MTIME="$(git -C "$ROOT" log -1 --format=%cI)"
# The runner image ships images/runner/LICENSE: a release must not differ.
sh "$ROOT/scripts/check-license.sh"

TAR=tar
if ! tar --version 2>/dev/null | grep -q 'GNU tar'; then
  command -v gtar >/dev/null || { echo "dist.sh: GNU tar is required (tar or gtar)" >&2; exit 1; }
  TAR=gtar
fi
TAR_REPRO_FLAGS=(--owner=0 --group=0 --numeric-owner --sort=name --mtime="$MTIME")
WANT_MEMBERS=$'bin/\nbin/lux-gha\nshare/\nshare/lux-gha/\nshare/lux-gha/LICENSE'

rm -rf "$DIST"
mkdir -p "$DIST"

for arch in arm64 amd64; do
  name="lux-gha_${VERSION}_linux_${arch}.tar.gz"
  work="$DIST/work-linux-$arch"
  mkdir -p "$work/bin" "$work/share/lux-gha"
  echo "building lux-gha for linux/$arch"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -C "$ROOT" -trimpath -ldflags "$LDFLAGS" \
    -o "$work/bin/lux-gha" ./cmd/lux-gha
  cp "$ROOT/LICENSE" "$work/share/lux-gha/LICENSE"
  chmod 0755 "$work/bin" "$work/share" "$work/share/lux-gha" "$work/bin/lux-gha"
  chmod 0644 "$work/share/lux-gha/LICENSE"
  # gzip -n: tar -z would let gzip stamp the header with the current time.
  "$TAR" "${TAR_REPRO_FLAGS[@]}" -C "$work" -cf - bin share | gzip -9 -n >"$DIST/$name"
  rm -rf "$work"

  # Check the tarball itself, not the tree it was made from.
  members="$("$TAR" -tzf "$DIST/$name")"
  if [ "$members" != "$WANT_MEMBERS" ]; then
    printf 'dist.sh: %s has members:\n%s\nwant:\n%s\n' "$name" "$members" "$WANT_MEMBERS" >&2
    exit 1
  fi
  if ! "$TAR" -xzOf "$DIST/$name" share/lux-gha/LICENSE | cmp -s - "$ROOT/LICENSE"; then
    echo "dist.sh: $name: share/lux-gha/LICENSE differs from LICENSE" >&2
    exit 1
  fi
done

(cd "$DIST" && sha256sum lux-gha_*.tar.gz >SHA256SUMS)

echo "dist/:"
ls -la "$DIST"
