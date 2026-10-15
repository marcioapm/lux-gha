#!/bin/sh
# Lifecycle tests for entrypoint.sh. Fake process boundaries stand in for the
# real ones: RUNNER_DIR/run.sh for GitHub's runner, and dockerd-rootless and
# docker, first on PATH, for the engine. The entrypoint runs as a child of a
# supervisor shell, as it does under the Lux shim.
#
# Needs Linux with sh, setsid and timeout, and a non-root user (root can read
# a mode-000 file). smoke.sh runs it inside the built image against the
# installed entrypoint.
#
# Usage: sh entrypoint_test.sh [path/to/entrypoint]
#
# SC2016: the supervisor script and every check condition are single-quoted
# so they expand when run (sh -c, eval in check), after each case sets d, rc.
# shellcheck disable=SC2016
set -eu

ep=${1:-$(dirname "$0")/entrypoint.sh}
root_case=${2:-}
if [ "$root_case" = sigterm-root ]; then
	[ "$(id -u)" -eq 0 ] || { echo "sigterm-root requires uid 0" >&2; exit 2; }
else
	[ "$(id -u)" -ne 0 ] || { echo "entrypoint_test: run as a non-root user" >&2; exit 2; }
fi
root=$PWD/.scratch/entrypoint-$$
mkdir -p "$root"
chmod 755 "$root" "$PWD/.scratch"
sentinel=sentinel-jitconfig-$$-$(date +%s)
failures=0

stop_daemons() {
	for f in "$root"/*/dockerd.pid; do
		if [ -f "$f" ]; then kill "$(cat "$f")" 2>/dev/null || true; fi
		rm -f "$f"
	done
}
trap 'stop_daemons; rm -rf "$root"' EXIT

mkdir -p "$root/bin" "$root/runner"

cat >"$root/runner/run.sh" <<'EOF'
#!/bin/sh
env >"$FAKE_CASE_DIR/runner.env"
id -u >"$FAKE_CASE_DIR/runner.uid"
printf '%s\n' "$@" >"$FAKE_CASE_DIR/runner.args"
echo runner >>"$FAKE_CASE_DIR/order"
mode=${FAKE_RUN:-exit:0}
case $mode in
signal-parent) kill -USR1 "$PPID" ;;
exit:*) exit "${mode#exit:}" ;;
await-term)
	# As GitHub's run.sh: only with RUNNER_MANUALLY_TRAP_SIG set does it
	# trap TERM (and pass it to the listener as INT); otherwise TERM kills it.
	if [ -n "${RUNNER_MANUALLY_TRAP_SIG:-}" ]; then
		trap 'echo TERM >"$FAKE_CASE_DIR/term"; exit 0' TERM
	fi
	: >"$FAKE_CASE_DIR/ready"
	i=0
	while [ "$i" -lt 100 ]; do sleep 0.1; i=$((i + 1)); done
	exit 9
	;;
esac
EOF

# Listens (creates the socket path as a plain file) on --host, else on the
# rootless default under XDG_RUNTIME_DIR.
cat >"$root/bin/dockerd-rootless" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" >"$FAKE_CASE_DIR/dockerd.args"
sock=$XDG_RUNTIME_DIR/docker.sock
[ "${1:-}" != --host ] || sock=${2#unix://}
[ "$FAKE_DOCKERD" != exit ] || exit 1
echo $$ >"$FAKE_CASE_DIR/dockerd.pid"
case $FAKE_DOCKERD in
ready) : >"$sock" ;;
ready-later) (sleep 2; : >"$sock") & ;;
esac
exec sleep 300
EOF

# info answers once the socket exists and records each answer in the call
# order. load records its arguments and the call order, then succeeds, fails
# or hangs as FAKE_DOCKER_LOAD says; a hung load ignores SIGTERM, so only
# timeout's -k SIGKILL ends it.
cat >"$root/bin/docker" <<'EOF'
#!/bin/sh
if [ "${1:-}" = load ]; then
	printf '%s\n' "$@" >"$FAKE_CASE_DIR/load.args"
	echo load >>"$FAKE_CASE_DIR/order"
	case ${FAKE_DOCKER_LOAD:-ok} in
	fail) echo "Error response from daemon: fake load failure" >&2; exit 1 ;;
	hang) trap '' TERM; exec sleep 300 ;;
	esac
	printf 'Loaded image: moby/buildkit:buildx-stable-1\nLoaded image: moby/buildkit:v0.0.0\n'
	exit 0
fi
[ "${1:-}" = info ] || exit 2
case ${FAKE_DOCKER:-socket} in
hang) exec sleep 300 ;;
fail) exit 1 ;;
esac
[ -e "${DOCKER_HOST#unix://}" ] || exit 1
echo info >>"$FAKE_CASE_DIR/order"
EOF
chmod 755 "$root/runner/run.sh" "$root/bin/dockerd-rootless" "$root/bin/docker"

printf %s "$sentinel" >"$root/unreadable"
chmod 000 "$root/unreadable"
: >"$root/empty"
printf 'not a real archive\n' >"$root/buildkit.tar"

# The supervisor exits with the entrypoint's status and records a USR1 sent
# to it. A trapped signal interrupts wait with status >128, so it waits
# again until a wait returns without the trap having fired. With FAKE_TERM
# set it stops the entrypoint as Lux does: the entrypoint leads its own
# process group, and once run.sh is up SIGTERM goes to that group. On exit
# it records whether dockerd survived the entrypoint.
supervisor='
trap "trapped=1; echo USR1 >\"\$FAKE_CASE_DIR/signal\"" USR1
if [ -n "${FAKE_TERM:-}" ]; then
	setsid "$1" &
	child=$!
	(
		i=0
		while [ ! -f "$FAKE_CASE_DIR/ready" ] && [ "$i" -lt 100 ]; do sleep 0.1; i=$((i + 1)); done
		kill -TERM -"$child"
	) &
else
	"$1" &
	child=$!
fi
while :; do
	trapped=0
	wait "$child" && rc=0 || rc=$?
	[ "$trapped" = 1 ] && continue
	if [ -f "$FAKE_CASE_DIR/dockerd.pid" ] && kill -0 "$(cat "$FAKE_CASE_DIR/dockerd.pid")" 2>/dev/null; then
		: >"$FAKE_CASE_DIR/dockerd.alive"
	fi
	exit "$rc"
done'

# run_case NAME [--default-archive] [VAR=value...]: run the entrypoint in a
# fresh case dir with the defaults below, overridden by the arguments. Sets
# d, rc and elapsed. LUXGHA_BUILDKIT_ARCHIVE defaults to a path that does not
# exist, so only the buildkit-* cases load one, also when this runs inside
# the image that ships one; --default-archive leaves it unset.
run_case() {
	name=$1
	shift
	d=$root/$name
	bk_archive=LUXGHA_BUILDKIT_ARCHIVE=$d/no-archive.tar
	if [ "${1:-}" = --default-archive ]; then
		bk_archive=
		shift
	fi
	mkdir -p "$d/xdg"
	if [ "$root_case" = sigterm-root ]; then chown -R 1000:1000 "$d"; fi
	printf %s "$sentinel" >"$d/jit"
	start=$(date +%s)
	rc=0
	env -i PATH="$root/bin:/usr/bin:/bin" HOME="$d" RUNNER_DIR="$root/runner" \
		XDG_RUNTIME_DIR="$d/xdg" DOCKER_HOST="unix://$d/custom.sock" \
		LUXGHA_JITCONFIG_FILE="$d/jit" FAKE_CASE_DIR="$d" FAKE_DOCKERD=ready \
		${bk_archive:+"$bk_archive"} "$@" \
		timeout -k 2 30 sh -c "$supervisor" supervisor "$ep" >"$d/out" 2>"$d/err" || rc=$?
	elapsed=$(($(date +%s) - start))
	stop_daemons
}

check() {
	if eval "$2"; then
		echo "ok   [$name] $1"
	else
		echo "FAIL [$name] $1 (rc=$rc elapsed=${elapsed}s)"
		sed 's/^/     stderr: /' "$d/err"
		failures=$((failures + 1))
	fi
}

# The config may reach the runner only as its --jitconfig argument.
check_no_leak() {
	check "config absent from stdout, stderr, runner env and dockerd args" \
		'! grep -qF "$sentinel" "$d/out" "$d/err" $(ls "$d/runner.env" "$d/dockerd.args" 2>/dev/null)'
}
reached='[ -f "$d/runner.args" ]'
not_reached='[ ! -f "$d/runner.args" ]'

if [ "$root_case" = sigterm-root ]; then
	run_case sigterm-root FAKE_RUN=await-term FAKE_TERM=1 RUNNER_USER=ubuntu
	check "exits 0 through setpriv" '[ "$rc" = 0 ]'
	check "RUNNER_MANUALLY_TRAP_SIG is set for run.sh" 'grep -qx "RUNNER_MANUALLY_TRAP_SIG=1" "$d/runner.env"'
	check "SIGTERM reached run.sh's trap" '[ "$(cat "$d/term" 2>/dev/null)" = TERM ]'
	check "run.sh runs as uid 1000" 'grep -qx "1000" "$d/runner.uid"'
	check "root path leaves Docker to the job" '[ ! -f "$d/dockerd.pid" ]'
	check_no_leak
	[ "$failures" -eq 0 ] || exit 1
	echo "entrypoint_test: OK"
	exit 0
fi

run_case exec FAKE_RUN=signal-parent
check "exits 0" '[ "$rc" = 0 ]'
check "run.sh replaces the entrypoint: its parent is the supervisor" '[ -f "$d/signal" ]'
check "runner gets exactly --jitconfig <config>" '[ "$(cat "$d/runner.args")" = "$(printf "%s\n%s" --jitconfig "$sentinel")" ]'
check "dockerd listens on DOCKER_HOST" '[ "$(cat "$d/dockerd.args")" = "$(printf "%s\n%s" --host "unix://$d/custom.sock")" ]'
check_no_leak

# Lux stops a Run with SIGTERM to its workload's process group, then SIGKILL
# after workload.grace. run.sh must get it with RUNNER_MANUALLY_TRAP_SIG set,
# so it hands it to the listener, which cancels the job and uploads logs.
run_case sigterm FAKE_RUN=await-term FAKE_TERM=1
check "exits 0: run.sh, not a shell around it, took the signal" '[ "$rc" = 0 ]'
check "RUNNER_MANUALLY_TRAP_SIG is set for run.sh" 'grep -qx "RUNNER_MANUALLY_TRAP_SIG=1" "$d/runner.env"'
check "SIGTERM reached run.sh's trap" '[ "$(cat "$d/term" 2>/dev/null)" = TERM ]'
check "rootless dockerd outlives the signal" '[ -f "$d/dockerd.alive" ]'
check_no_leak

run_case exit-code FAKE_RUN=exit:37
check "run.sh exit status propagates" '[ "$rc" = 37 ]'
check "runner reached" "$reached"
check_no_leak

run_case unreadable-config LUXGHA_JITCONFIG_FILE="$root/unreadable"
check "exits 1" '[ "$rc" = 1 ]'
check "runner not reached" "$not_reached"
check "dockerd not started" '[ ! -f "$d/dockerd.args" ]'
check_no_leak

run_case missing-config LUXGHA_JITCONFIG_FILE="$root/does-not-exist"
check "exits 1" '[ "$rc" = 1 ]'
check "runner not reached" "$not_reached"

run_case empty-config LUXGHA_JITCONFIG_FILE="$root/empty"
check "exits 1" '[ "$rc" = 1 ]'
check "runner not reached" "$not_reached"

run_case daemon-exits-required FAKE_DOCKERD=exit LUXGHA_DOCKER_WAIT=20
check "exits 1" '[ "$rc" = 1 ]'
check "runner not reached" "$not_reached"
check "fails without waiting out the deadline" '[ "$elapsed" -le 5 ]'
check_no_leak

run_case daemon-exits-optional FAKE_DOCKERD=exit LUXGHA_DOCKER=optional
check "exits 0" '[ "$rc" = 0 ]'
check "runner reached" "$reached"
check_no_leak

run_case probe-fails-required FAKE_DOCKERD=alive LUXGHA_DOCKER_WAIT=3
check "exits 1" '[ "$rc" = 1 ]'
check "runner not reached" "$not_reached"
check "gives up at the 3s deadline" '[ "$elapsed" -ge 3 ] && [ "$elapsed" -le 6 ]'
check_no_leak

run_case probe-hangs-required FAKE_DOCKER=hang LUXGHA_DOCKER_WAIT=3
check "exits 1" '[ "$rc" = 1 ]'
check "runner not reached" "$not_reached"
check "a hung probe does not outlast the 3s deadline" '[ "$elapsed" -ge 3 ] && [ "$elapsed" -le 6 ]'

run_case probe-hangs-optional FAKE_DOCKER=hang LUXGHA_DOCKER_WAIT=3 LUXGHA_DOCKER=optional
check "exits 0" '[ "$rc" = 0 ]'
check "runner reached" "$reached"

run_case daemon-ready-later FAKE_DOCKERD=ready-later
check "exits 0" '[ "$rc" = 0 ]'
check "runner reached" "$reached"
check "waited for readiness" '[ "$elapsed" -ge 2 ] && [ "$elapsed" -le 6 ]'
check_no_leak

for bad in 08 010 0 x5; do
	run_case "bad-wait-$bad" LUXGHA_DOCKER_WAIT="$bad"
	check "exits 1" '[ "$rc" = 1 ]'
	check "runner not reached" "$not_reached"
	check "says why" 'grep -q "LUXGHA_DOCKER_WAIT must be" "$d/err"'
done

archive=$root/buildkit.tar
default_archive=/usr/share/lux-gha/buildkit.tar
loaded_then_runner='[ "$(cat "$d/order")" = "$(printf "info\nload\nrunner")" ]'
loaded_from() { [ "$(cat "$d/load.args" 2>/dev/null)" = "$(printf "load\n-i\n%s" "$1")" ]; }
one_warning='[ "$(wc -l <"$d/err")" = 1 ] && grep -q "^lux-gha-runner: warning: " "$d/err"'

run_case buildkit-load-ok LUXGHA_BUILDKIT_ARCHIVE="$archive"
check "exits 0" '[ "$rc" = 0 ]'
check "docker load -i <archive>" 'loaded_from "$archive"'
check "loads after docker info, before run.sh" "$loaded_then_runner"
check "names both loaded tags on stdout" 'grep -q "preloaded moby/buildkit:buildx-stable-1 moby/buildkit:v0.0.0 in [0-9]*s" "$d/out"'
check "nothing on stderr" '[ ! -s "$d/err" ]'
check_no_leak

# The image's own archive, at the path the entrypoint uses by default; it
# exists only inside the built image, so these run under smoke.sh only.
if [ -f "$default_archive" ]; then
	run_case buildkit-default-path --default-archive
	check "exits 0" '[ "$rc" = 0 ]'
	check "LUXGHA_BUILDKIT_ARCHIVE unset: docker load -i $default_archive" 'loaded_from "$default_archive"'
	check "loads after docker info, before run.sh" "$loaded_then_runner"

	run_case buildkit-default-path-empty LUXGHA_BUILDKIT_ARCHIVE=
	check "LUXGHA_BUILDKIT_ARCHIVE empty: docker load -i $default_archive" 'loaded_from "$default_archive"'
fi

# A file named none in the entrypoint's working directory, so that the case
# fails if none is treated as a path.
: >none
run_case buildkit-none-file LUXGHA_BUILDKIT_ARCHIVE=none
rm -f none
check "exits 0" '[ "$rc" = 0 ]'
check "LUXGHA_BUILDKIT_ARCHIVE=none with ./none present: load not attempted" '[ ! -f "$d/load.args" ]'
check "runs info, then run.sh" '[ "$(cat "$d/order")" = "$(printf "info\nrunner")" ]'
check "nothing on stderr" '[ ! -s "$d/err" ]'

rm -f /tmp/buildkit-load.log
run_case buildkit-load-fails LUXGHA_BUILDKIT_ARCHIVE="$archive" FAKE_DOCKER_LOAD=fail
check "exits 0" '[ "$rc" = 0 ]'
check "load attempted, then run.sh" "$loaded_then_runner"
check "one warning line on stderr" "$one_warning"
check "docker's error goes to the log, not stderr" '! grep -q "fake load failure" "$d/err"'
check "the log holds docker's error" 'grep -q "fake load failure" /tmp/buildkit-load.log'
check_no_leak

# The hung fake ignores SIGTERM: only timeout -k's SIGKILL, 2 s after the
# 2 s limit, ends it.
run_case buildkit-load-hangs LUXGHA_BUILDKIT_ARCHIVE="$archive" FAKE_DOCKER_LOAD=hang LUXGHA_BUILDKIT_LOAD_TIMEOUT=2
check "exits 0" '[ "$rc" = 0 ]'
check "runner reached" "$reached"
check "a hung load that ignores SIGTERM is killed 2s after the 2s timeout" '[ "$elapsed" -ge 4 ] && [ "$elapsed" -le 7 ]'
check "one warning line on stderr" "$one_warning"

run_case buildkit-bad-timeout LUXGHA_BUILDKIT_ARCHIVE="$archive" LUXGHA_BUILDKIT_LOAD_TIMEOUT=08
check "exits 0" '[ "$rc" = 0 ]'
check "runner reached" "$reached"
check "load not attempted" '[ ! -f "$d/load.args" ]'
check "one warning line on stderr" "$one_warning"

run_case buildkit-no-archive
check "exits 0" '[ "$rc" = 0 ]'
check "load not attempted" '[ ! -f "$d/load.args" ]'
check "nothing on stderr" '[ ! -s "$d/err" ]'

run_case buildkit-docker-down LUXGHA_BUILDKIT_ARCHIVE="$archive" FAKE_DOCKERD=exit LUXGHA_DOCKER=optional
check "exits 0" '[ "$rc" = 0 ]'
check "runner reached" "$reached"
check "load not attempted without an engine" '[ ! -f "$d/load.args" ]'

if [ "$failures" -ne 0 ]; then
	echo "entrypoint_test: $failures failed"
	exit 1
fi
echo "entrypoint_test: OK"
