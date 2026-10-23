#!/bin/sh
# Tests for patch-worker.sh against synthetic Worker DLLs and a minimal
# run-helper template, including a simulated runner self-update. Runs under
# any POSIX sh; needs perl (the runner's Ubuntu base has it).
#
# Usage: sh patch-worker_test.sh [path/to/patch-worker.sh]
#
# SC2016: every check condition is single-quoted so it expands when check
# evals it, as in entrypoint_test.sh. SC2034/SC2209: rc, ino and name are
# read inside those conditions.
# SC2012: ls -i is the portable way to read an inode.
# shellcheck disable=SC2016,SC2034,SC2209,SC2012
set -eu

pw=${1:-$(dirname "$0")/patch-worker.sh}
root=$PWD/.scratch/patch-worker-$$
mkdir -p "$root"
trap 'rm -rf "$root"' EXIT
failures=0
name=

check() {
	if eval "$2"; then
		echo "ok   [$name] $1"
	else
		echo "FAIL [$name] $1"
		[ ! -s "$root/err" ] || sed 's/^/     stderr: /' "$root/err"
		failures=$((failures + 1))
	fi
}

# dll FILE N M: a file with N UTF-16LE ACTIONS_RESULTS_URL and M
# ACTIONS_RESULTS_ORL literals between filler bytes, as in a .NET assembly.
dll() {
	perl -e '
		my ($f, $n, $m) = @ARGV;
		my $u = sub { join "", map { $_ . "\0" } split //, shift };
		my $d = "MZ\x90\0" . ("\xff" x 64);
		$d .= $u->("ACTIONS_RESULTS_URL") . ("\x01" x 16) for 1 .. $n;
		$d .= $u->("ACTIONS_RESULTS_ORL") . ("\x02" x 16) for 1 .. $m;
		$d .= $u->("ACTIONS_CACHE_URL") . ("\0" x 32);
		open(my $fh, ">:raw", $f) or die; print $fh $d; close $fh;
	' "$1" "$2" "$3"
}
# run CMD...: the script with stdout/stderr captured; sets rc.
run() { rc=0; sh "$pw" "$@" >"$root/out" 2>"$root/err" || rc=$?; }

name=count
dll "$root/a.dll" 1 0
run count "$root/a.dll"
check "one original, none patched" '[ "$rc" = 0 ] && [ "$(cat "$root/out")" = "1 0" ]'

name=check
run check 1 "$root/a.dll"
check "the pinned count passes" '[ "$rc" = 0 ]'
run check 2 "$root/a.dll"
check "a different pin fails, naming what it found" '[ "$rc" != 0 ] && grep -q "found \"1 0\"" "$root/err"'
dll "$root/patched.dll" 0 1
run check 1 "$root/patched.dll"
check "an already patched DLL fails the build check" '[ "$rc" != 0 ]'
run check x "$root/a.dll"
check "a non-numeric pin is refused" '[ "$rc" != 0 ]'

name=apply
cp "$root/a.dll" "$root/before.dll"
run apply "$root/a.dll"
check "patches one" '[ "$rc" = 0 ] && grep -q "patched (1)" "$root/out"'
check "counts are now 0 1" '[ "$(sh "$pw" count "$root/a.dll")" = "0 1" ]'
check "same size, one byte changed" \
	'[ "$(wc -c <"$root/a.dll")" = "$(wc -c <"$root/before.dll")" ] && [ "$(cmp -l "$root/before.dll" "$root/a.dll" | wc -l)" -eq 1 ]'
check "the other literals are untouched" \
	'perl -0777 -ne "exit(index(\$_, join(qq(), map { \$_ . qq(\\0) } split //, q(ACTIONS_CACHE_URL))) < 0)" "$root/a.dll"'
run apply "$root/a.dll"
check "applying again is a no-op" '[ "$rc" = 0 ] && grep -q "already patched (1)" "$root/out" && [ "$(sh "$pw" count "$root/a.dll")" = "0 1" ]'

dll "$root/three.dll" 3 0
run apply "$root/three.dll"
check "patches every occurrence" '[ "$rc" = 0 ] && [ "$(sh "$pw" count "$root/three.dll")" = "0 3" ]'

dll "$root/none.dll" 0 0
run apply "$root/none.dll"
check "no literal at all fails" '[ "$rc" != 0 ] && grep -q "unexpected state: 0 original and 0 patched" "$root/err"'

dll "$root/mixed.dll" 1 1
cp "$root/mixed.dll" "$root/mixed.before"
run apply "$root/mixed.dll"
check "a mixed state fails and leaves the file as it was" '[ "$rc" != 0 ] && cmp -s "$root/mixed.dll" "$root/mixed.before"'

run apply "$root/missing.dll"
check "a missing file fails" '[ "$rc" != 0 ]'

name=mode
dll "$root/m.dll" 1 0
chmod 640 "$root/m.dll"
ino=$(ls -i "$root/m.dll" | awk '{print $1}')
run apply "$root/m.dll"
check "the file keeps its inode and mode" \
	'[ "$(ls -i "$root/m.dll" | awk "{print \$1}")" = "$ino" ] && [ "$(ls -l "$root/m.dll" | cut -c1-10)" = "-rw-r-----" ]'

# A minimal run-helper template: the real one's anchor line, with the
# listener replaced by a script that records the Worker's counts.
name=helper
tpl=$root/run-helper.sh.template
cat >"$tpl" <<'EOF'
#!/bin/bash
SOURCE="${BASH_SOURCE[0]}"
DIR="$( cd -P "$( dirname "$SOURCE" )" && pwd )"
"$DIR"/bin/Runner.Listener run $*
returnCode=$?
exit $returnCode
EOF
cp "$tpl" "$root/tpl.orig"
run helper "$tpl"
check "inserts the step" '[ "$rc" = 0 ] && grep -q "inserted" "$root/out"'
check "exactly one marker, five lines before the listener line" \
	'[ "$(grep -c "^# lux-gha: point jobs at the Actions cache server" "$tpl")" = 1 ] &&
	 [ "$(grep -n "^# lux-gha: point jobs" "$tpl" | cut -d: -f1)" -eq "$(($(grep -n "bin/Runner.Listener run" "$tpl" | cut -d: -f1) - 5))" ]'
check "still valid bash" 'bash -n "$tpl"'
run helper "$tpl"
check "inserting again is a no-op" '[ "$rc" = 0 ] && grep -q "already present" "$root/out" && [ "$(grep -c "lux-gha: point jobs" "$tpl")" = 1 ]'
printf '#!/bin/bash\necho no listener here\n' >"$root/noanchor.tpl"
run helper "$root/noanchor.tpl"
check "a template without the anchor fails" '[ "$rc" != 0 ] && grep -q "found 0" "$root/err"'
cat "$root/tpl.orig" "$root/tpl.orig" >"$root/twice.tpl"
run helper "$root/twice.tpl"
check "a template with two anchors fails" '[ "$rc" != 0 ] && grep -q "found 2" "$root/err"'

# Simulated self-update: the runner moves bin to bin.<old>, unpacks the new
# version into bin.<new>, points bin at it and restarts the listener. The
# helper step must patch the new Worker before the listener starts.
name=self-update
rd=$root/runner
mkdir -p "$rd/bin.2.338.0" "$rd/bin.2.339.0"
listener() {
	cat >"$1/Runner.Listener" <<EOF
#!/bin/sh
sh "$pw" count "\$(dirname "\$0")/Runner.Worker.dll" >"$rd/seen"
EOF
	chmod 755 "$1/Runner.Listener"
}
listener "$rd/bin.2.338.0"
listener "$rd/bin.2.339.0"
dll "$rd/bin.2.338.0/Runner.Worker.dll" 1 0
dll "$rd/bin.2.339.0/Runner.Worker.dll" 1 0
ln -s bin.2.338.0 "$rd/bin"
cp "$tpl" "$rd/run-helper.sh"
start() { rc=0; env LUXGHA_PATCH_WORKER="$pw" "$@" bash "$rd/run-helper.sh" >"$root/out" 2>"$root/err" || rc=$?; }
start LUXGHA_ACTIONS_RESULTS_URL=https://cache.example.com
check "first start: the listener sees a patched Worker" '[ "$(cat "$rd/seen")" = "0 1" ]'
rm "$rd/bin" && ln -s bin.2.339.0 "$rd/bin"
check "after the update the new Worker is unpatched" '[ "$(sh "$pw" count "$rd/bin/Runner.Worker.dll")" = "1 0" ]'
start LUXGHA_ACTIONS_RESULTS_URL=https://cache.example.com
check "restart after the update: the listener sees the new Worker patched" '[ "$(cat "$rd/seen")" = "0 1" ]'
check "the old version stays as it was" '[ "$(sh "$pw" count "$rd/bin.2.338.0/Runner.Worker.dll")" = "0 1" ]'

name=unset
dll "$rd/bin.2.339.0/Runner.Worker.dll" 1 0
start
check "without LUXGHA_ACTIONS_RESULTS_URL the Worker is left as shipped" '[ "$(cat "$rd/seen")" = "1 0" ] && [ ! -s "$root/err" ]'

name=unpatchable
dll "$rd/bin.2.339.0/Runner.Worker.dll" 0 0
start LUXGHA_ACTIONS_RESULTS_URL=https://cache.example.com
check "an unpatchable Worker warns once and the listener still starts" \
	'[ "$(cat "$rd/seen")" = "0 0" ] && grep -q "is not patched; jobs use GitHub" "$root/err"'

if [ "$failures" -ne 0 ]; then
	echo "patch-worker_test: $failures failed"
	exit 1
fi
echo "patch-worker_test: OK"
