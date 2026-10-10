#!/bin/sh
# The runner image's build context (images/runner) cannot reach the root
# LICENSE, so it carries a copy; fail unless the two are byte-identical.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
if ! cmp "$root/LICENSE" "$root/images/runner/LICENSE"; then
	echo "FAIL: images/runner/LICENSE differs from LICENSE; copy the root file over it" >&2
	exit 1
fi
echo "check-license: OK"
