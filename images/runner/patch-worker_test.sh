#!/bin/sh
# Tests for patch-worker.sh and pre-listener.sh against synthetic Worker
# DLLs and a minimal run-helper, including a simulated runner self-update.
# Runs under any POSIX sh; needs perl and bash 4 or later (the runner image
# has both). bash 3.2 (macOS) sets $0 to "bash" while it reads BASH_ENV, so
# there the pre-listener cases are skipped: make entrypoint-test runs them.
#
# Usage: sh patch-worker_test.sh [path/to/patch-worker.sh]
# pre-listener.sh is taken from the same directory.
#
# SC2016: every check condition is single-quoted so it expands when check
# evals it, as in entrypoint_test.sh. SC2034/SC2209: rc, ino and name are
# read inside those conditions. SC2012: ls -i is the portable way to read an
# inode.
# shellcheck disable=SC2016,SC2034,SC2209,SC2012
set -eu

pw=${1:-$(dirname "$0")/patch-worker.sh}
pl=$(dirname "$pw")/pre-listener.sh
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
check "an already patched DLL fails the build check" '[ "$rc" != 0 ] && grep -q "found \"0 1\"" "$root/err"'
dll "$root/mixed.dll" 1 1
run check 1 "$root/mixed.dll"
check "a mixed DLL fails the build check" '[ "$rc" != 0 ] && grep -q "found \"1 1\"" "$root/err"'
dll "$root/none.dll" 0 0
run check 0 "$root/none.dll"
check "a pin of 0 is refused: such a DLL cannot be patched" '[ "$rc" != 0 ] && grep -q "positive number" "$root/err"'
run check x "$root/a.dll"
check "a non-numeric pin is refused" '[ "$rc" != 0 ] && grep -q "positive number" "$root/err"'

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

run apply "$root/none.dll"
check "no literal at all fails" '[ "$rc" != 0 ] && grep -q "unexpected state: 0 original and 0 patched" "$root/err"'

cp "$root/mixed.dll" "$root/mixed.before"
run apply "$root/mixed.dll"
check "a mixed state is refused as such, the file left as it was" \
	'[ "$rc" != 0 ] && grep -q "unexpected state: 1 original and 1 patched" "$root/err" && cmp -s "$root/mixed.dll" "$root/mixed.before"'

run apply "$root/missing.dll"
check "a missing file fails" '[ "$rc" != 0 ] && grep -q "not found" "$root/err"'

name=mode
dll "$root/m.dll" 1 0
chmod 640 "$root/m.dll"
ino=$(ls -i "$root/m.dll" | awk '{print $1}')
run apply "$root/m.dll"
check "the file keeps its inode and mode" \
	'[ "$(ls -i "$root/m.dll" | awk "{print \$1}")" = "$ino" ] && [ "$(ls -l "$root/m.dll" | cut -c1-10)" = "-rw-r-----" ]'

# A runner directory as GitHub ships it, reduced: run-helper.sh is a copy of
# run-helper.sh.template (a bash script, as the real one) that starts
# bin/Runner.Listener. The fake listener records the Worker's counts and
# whether BASH_ENV reached it.
rd=$root/runner
mkdir -p "$rd/bin.2.338.0" "$rd/bin.2.339.0"
cat >"$rd/run-helper.sh.template" <<'EOF'
#!/bin/bash
SOURCE="${BASH_SOURCE[0]}"
DIR="$( cd -P "$( dirname "$SOURCE" )" && pwd )"
"$DIR"/bin/Runner.Listener run $*
exit $?
EOF
cp "$rd/run-helper.sh.template" "$root/template.stock"
listener() {
	cat >"$1/Runner.Listener" <<EOF
#!/bin/sh
sh "$pw" count "\$(dirname "\$0")/Runner.Worker.dll" >"$rd/seen"
echo "\${BASH_ENV-unset}" >"$rd/bash_env"
EOF
	chmod 755 "$1/Runner.Listener"
}
listener "$rd/bin.2.338.0"
listener "$rd/bin.2.339.0"
dll "$rd/bin.2.338.0/Runner.Worker.dll" 1 0
dll "$rd/bin.2.339.0/Runner.Worker.dll" 1 0
ln -s bin.2.338.0 "$rd/bin"
# start [VAR=value...]: one run.sh loop iteration: copy the template to
# run-helper.sh and run it, with BASH_ENV as the entrypoint sets it.
start() {
	cp -f "$rd/run-helper.sh.template" "$rd/run-helper.sh"
	chmod 755 "$rd/run-helper.sh"
	rc=0
	env LUXGHA_PATCH_WORKER="$pw" "$@" "$rd/run-helper.sh" >"$root/out" 2>"$root/err" || rc=$?
}

if [ "$(bash -c 'echo "${BASH_VERSINFO[0]}"')" -lt 4 ]; then
	echo "skip [pre-listener] bash 3 does not set \$0 while reading BASH_ENV"
	echo "patch-worker_test: INCOMPLETE: pre-listener and self-update cases need bash 4 or later (make entrypoint-test runs them)"
	[ "$failures" -eq 0 ] || { echo "patch-worker_test: $failures failed"; exit 1; }
	exit 0
fi

name=pre-listener
start BASH_ENV="$pl"
check "the listener sees a patched Worker" '[ "$rc" = 0 ] && [ "$(cat "$rd/seen")" = "0 1" ]'
check "BASH_ENV does not reach the listener (or the job's steps)" '[ "$(cat "$rd/bash_env")" = unset ]'
check "the template is not modified" 'cmp -s "$rd/run-helper.sh.template" "$root/template.stock"'

# A self-update unpacks the new version into bin.<new>, points bin at it,
# and copies the new release's top-level files, run-helper.sh.template
# included, over the old ones; run.sh then starts run-helper.sh again.
name=self-update
rm "$rd/bin" && ln -s bin.2.339.0 "$rd/bin"
cp -f "$root/template.stock" "$rd/run-helper.sh.template"
check "after the update the new Worker is unpatched" '[ "$(sh "$pw" count "$rd/bin/Runner.Worker.dll")" = "1 0" ]'
start BASH_ENV="$pl"
check "the restarted listener sees the new Worker patched" '[ "$(cat "$rd/seen")" = "0 1" ]'
check "the old version stays as it was" '[ "$(sh "$pw" count "$rd/bin.2.338.0/Runner.Worker.dll")" = "0 1" ]'

name=other-scripts
dll "$rd/bin.2.339.0/Runner.Worker.dll" 1 0
printf '#!/bin/bash\n"%s"/bin/Runner.Listener\n' "$rd" >"$rd/run.sh"
chmod 755 "$rd/run.sh"
rc=0
env BASH_ENV="$pl" LUXGHA_PATCH_WORKER="$pw" "$rd/run.sh" >"$root/out" 2>"$root/err" || rc=$?
check "a bash script other than run-helper.sh patches nothing" '[ "$(cat "$rd/seen")" = "1 0" ] && [ ! -s "$root/err" ]'

name=unset
start
check "without BASH_ENV the Worker is left as shipped" '[ "$(cat "$rd/seen")" = "1 0" ] && [ ! -s "$root/err" ]'

name=unpatchable
dll "$rd/bin.2.339.0/Runner.Worker.dll" 0 0
start BASH_ENV="$pl"
check "an unpatchable Worker: one warning, and the listener still starts" \
	'[ "$(cat "$rd/seen")" = "0 0" ] && [ "$(grep -c "is not patched; jobs use GitHub" "$root/err")" = 1 ]'

if [ "$failures" -ne 0 ]; then
	echo "patch-worker_test: $failures failed"
	exit 1
fi
echo "patch-worker_test: OK"
