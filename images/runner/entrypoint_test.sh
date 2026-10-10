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
set -eu

ep=${1:-$(cd "$(dirname "$0")" && pwd)/entrypoint.sh}
[ "$(id -u)" -ne 0 ] || { echo "entrypoint_test: run as a non-root user" >&2; exit 2; }
root=$(mktemp -d)
sentinel=sentinel-jitconfig-$$-$(date +%s)
failures=0

stop_daemons() {
	for f in "$root"/*/dockerd.pid; do
		[ -f "$f" ] && kill "$(cat "$f")" 2>/dev/null || true
		rm -f "$f"
	done
}
trap 'stop_daemons; rm -rf "$root"' EXIT

mkdir -p "$root/bin" "$root/runner"

cat >"$root/runner/run.sh" <<'EOF'
#!/bin/sh
env >"$FAKE_CASE_DIR/runner.env"
printf '%s\n' "$@" >"$FAKE_CASE_DIR/runner.args"
mode=${FAKE_RUN:-exit:0}
case $mode in
signal-parent) kill -USR1 "$PPID" ;;
exit:*) exit "${mode#exit:}" ;;
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

cat >"$root/bin/docker" <<'EOF'
#!/bin/sh
[ "${1:-}" = info ] || exit 2
case ${FAKE_DOCKER:-socket} in
hang) exec sleep 300 ;;
fail) exit 1 ;;
esac
[ -e "${DOCKER_HOST#unix://}" ]
EOF
chmod 755 "$root/runner/run.sh" "$root/bin/dockerd-rootless" "$root/bin/docker"

printf %s "$sentinel" >"$root/unreadable"
chmod 000 "$root/unreadable"
: >"$root/empty"

# The supervisor exits with the entrypoint's status and records a USR1 sent
# to it. A trapped signal interrupts wait with status >128, so it waits
# again until a wait returns without the trap having fired.
supervisor='
trap "trapped=1; echo USR1 >\"\$FAKE_CASE_DIR/signal\"" USR1
"$1" &
child=$!
while :; do
	trapped=0
	wait "$child" && rc=0 || rc=$?
	[ "$trapped" = 1 ] || exit "$rc"
done'

# run_case NAME [VAR=value...]: run the entrypoint in a fresh case dir with
# the defaults below, overridden by the arguments. Sets d, rc and elapsed.
run_case() {
	name=$1
	shift
	d=$root/$name
	mkdir -p "$d/xdg"
	printf %s "$sentinel" >"$d/jit"
	start=$(date +%s)
	rc=0
	env -i PATH="$root/bin:/usr/bin:/bin" HOME="$d" RUNNER_DIR="$root/runner" \
		XDG_RUNTIME_DIR="$d/xdg" DOCKER_HOST="unix://$d/custom.sock" \
		LUXGHA_JITCONFIG_FILE="$d/jit" FAKE_CASE_DIR="$d" FAKE_DOCKERD=ready "$@" \
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

run_case exec FAKE_RUN=signal-parent
check "exits 0" '[ "$rc" = 0 ]'
check "run.sh replaces the entrypoint: its parent is the supervisor" '[ -f "$d/signal" ]'
check "runner gets exactly --jitconfig <config>" '[ "$(cat "$d/runner.args")" = "$(printf "%s\n%s" --jitconfig "$sentinel")" ]'
check "dockerd listens on DOCKER_HOST" '[ "$(cat "$d/dockerd.args")" = "$(printf "%s\n%s" --host "unix://$d/custom.sock")" ]'
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

if [ "$failures" -ne 0 ]; then
	echo "entrypoint_test: $failures failed"
	exit 1
fi
echo "entrypoint_test: OK"
