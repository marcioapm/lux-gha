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
env | grep '^DOCKERD_ROOTLESS_' | sort >"$FAKE_CASE_DIR/dockerd.env" || :
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
check "dockerd listens on DOCKER_HOST, host-gateway is slirp4netns's 10.0.2.2" '[ "$(cat "$d/dockerd.args")" = "$(printf "%s\n%s\n%s" --host "unix://$d/custom.sock" --host-gateway-ip=10.0.2.2)" ]'
check "rootlesskit uses slirp4netns with host loopback allowed" '[ "$(cat "$d/dockerd.env")" = "$(printf "%s\n%s" DOCKERD_ROOTLESS_ROOTLESSKIT_DISABLE_HOST_LOOPBACK=false DOCKERD_ROOTLESS_ROOTLESSKIT_NET=slirp4netns)" ]'
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

# In-region registry. The scripts sit next to the entrypoint in the source
# tree and at their installed paths inside the image.
src=$(dirname "$ep")
locate() { if [ -f "$src/$1" ]; then echo "$src/$1"; else echo "$2"; fi; }
hook=$(locate job-started.sh /usr/local/lib/lux-gha/job-started.sh)
wrapper=$(locate docker-credential-lux-gha.sh /usr/local/bin/docker-credential-lux-gha)
creds=$(locate lux-gha-aws-credentials.sh /usr/local/bin/lux-gha-aws-credentials)
command -v jq >/dev/null || { echo "entrypoint_test: jq is required (the image has it)" >&2; exit 2; }
command -v curl >/dev/null || { echo "entrypoint_test: curl is required (the image has it)" >&2; exit 2; }
token=v1.lux-gha-test-1001-7-0.c2VudGluZWwtdG9rZW4tJCQ$$
reg=123456789012.dkr.ecr.eu-west-1.amazonaws.com

# Fake curl: records argv and the --config it reads on stdin, then answers
# the next status in FAKE_CURL (space-separated, the last one repeats) with
# FAKE_BODY. "refused" execs the real curl, which finds nothing listening.
cat >"$root/bin/curl" <<'EOF2'
#!/bin/sh
out=
prev=
for a in "$@"; do
	printf '%s\n' "$a" >>"$FAKE_CASE_DIR/curl.args"
	if [ "$prev" = --output ]; then out=$a; fi
	prev=$a
done
cat >>"$FAKE_CASE_DIR/curl.stdin"
n=$(($(cat "$FAKE_CASE_DIR/curl.count" 2>/dev/null || echo 0) + 1))
echo "$n" >"$FAKE_CASE_DIR/curl.count"
# shellcheck disable=SC2086 # FAKE_CURL is a list of statuses
set -- ${FAKE_CURL:-200}
while [ "$n" -gt 1 ] && [ "$#" -gt 1 ]; do shift; n=$((n - 1)); done
if [ "$1" = refused ]; then
	exec /usr/bin/curl --silent --output /dev/null --write-out '%{http_code}' --connect-timeout 2 -X POST http://127.0.0.1:1/
fi
if [ -n "$out" ] && [ "$1" = 200 ]; then printf '%s' "${FAKE_BODY:-}" >"$out"; fi
# With --output the hook reads the status (--write-out); without it the
# credentials script reads the body, and --fail turns an error into exit 22.
if [ -n "$out" ]; then printf '%s' "$1"; exit 0; fi
[ "$1" = 200 ] || exit 22
printf '%s' "${FAKE_BODY:-}"
EOF2
chmod 755 "$root/bin/curl"

check_no_token() {
	check "the identity token is in no output and no curl argument" \
		'! grep -qF "$token" "$d/out" "$d/err" $(ls "$d/curl.args" "$d/runner.env" 2>/dev/null)'
}

# run_hook NAME [VAR=value...]: run the job-started hook as the runner does
# (bash -e -o pipefail), with a fresh GITHUB_ENV. Sets d, rc and elapsed.
run_hook() {
	name=$1
	shift
	d=$root/$name
	mkdir -p "$d"
	printf '%s' "$token" >"$d/identity"
	: >"$d/github_env"
	start=$(date +%s)
	rc=0
	env -i PATH="$root/bin:/usr/bin:/bin" HOME="$d" FAKE_CASE_DIR="$d" GITHUB_ENV="$d/github_env" \
		LUXGHA_REGISTRY_HOST="$reg" LUXGHA_RUNNER_ENDPOINT=http://10.0.0.5:8081 LUXGHA_IDENTITY_FILE="$d/identity" \
		LUXGHA_REGISTRY_WAIT=2 "$@" \
		timeout -k 2 30 bash --noprofile --norc -e -o pipefail "$hook" >"$d/out" 2>"$d/err" || rc=$?
	elapsed=$(($(date +%s) - start))
}
nothing_written='[ ! -s "$d/github_env" ]'
one_line='[ "$(wc -l <"$d/out")" = 1 ] && [ ! -s "$d/err" ]'

run_hook hook-200-untrusted FAKE_CURL=200 \
	FAKE_BODY="{\"registry\":\"$reg\",\"read\":\"$reg/acme/widgets\",\"push\":\"$reg/acme/widgets/pr\",\"trust\":\"untrusted\",\"expiresAt\":\"2026-10-23T13:00:00Z\"}"
check "exits 0" '[ "$rc" = 0 ]'
check "GITHUB_ENV gets LUX_REGISTRY and LUX_REGISTRY_PUSH" \
	'[ "$(cat "$d/github_env")" = "$(printf "LUX_REGISTRY=%s\nLUX_REGISTRY_PUSH=%s" "$reg/acme/widgets" "$reg/acme/widgets/pr")" ]'
check "one line naming the trust level" '[ "$(cat "$d/out")" = "lux-gha registry: trust=untrusted" ] && [ ! -s "$d/err" ]'
check "POST to the registry route with the token in a curl config on stdin" \
	'grep -qx -- "-X" "$d/curl.args" && grep -qx "POST" "$d/curl.args" && grep -qx "http://10.0.0.5:8081/runner/v1/registry" "$d/curl.args" &&
	 [ "$(cat "$d/curl.stdin")" = "header = \"Authorization: Bearer $token\"" ]'
check_no_token

run_hook hook-200-read-only FAKE_CURL=200 \
	FAKE_BODY="{\"registry\":\"$reg\",\"read\":\"$reg/acme/widgets\",\"push\":\"\",\"trust\":\"read-only\",\"expiresAt\":\"x\"}"
check "exits 0" '[ "$rc" = 0 ]'
check "GITHUB_ENV gets LUX_REGISTRY only" '[ "$(cat "$d/github_env")" = "LUX_REGISTRY=$reg/acme/widgets" ]'
check "one line naming the trust level" '[ "$(cat "$d/out")" = "lux-gha registry: trust=read-only" ]'

run_hook hook-200-dotted-names FAKE_CURL=200 \
	FAKE_BODY="{\"registry\":\"$reg\",\"read\":\"$reg/acme-corp/my_widgets.go\",\"push\":\"$reg/acme-corp/my_widgets.go\",\"trust\":\"trusted\",\"expiresAt\":\"x\"}"
check "owner and repo names with . _ - pass through" \
	'[ "$(cat "$d/github_env")" = "$(printf "LUX_REGISTRY=%s\nLUX_REGISTRY_PUSH=%s" "$reg/acme-corp/my_widgets.go" "$reg/acme-corp/my_widgets.go")" ]'

# A body that would inject a second variable, point at another host, or
# name a path the registry cannot hold (upper case).
for bad in "{\"read\":\"$reg/acme/widgets\\nLD_PRELOAD=/tmp/x\",\"push\":\"\",\"trust\":\"trusted\"}" \
	"{\"read\":\"evil.example.com/acme/widgets\",\"push\":\"\",\"trust\":\"trusted\"}" \
	"{\"read\":\"$reg/Acme/widgets\",\"push\":\"\",\"trust\":\"trusted\"}" \
	"{\"read\":\"$reg/acme/widgets\",\"push\":\"\",\"trust\":\"admin\"}" "not json"; do
	run_hook hook-200-bad FAKE_CURL=200 FAKE_BODY="$bad"
	check "exits 0 on a malformed answer" '[ "$rc" = 0 ]'
	check "writes nothing" "$nothing_written"
	check "one line" "$one_line"
done

for status in 204 401 503 500; do
	run_hook "hook-$status" FAKE_CURL="$status"
	check "exits 0" '[ "$rc" = 0 ]'
	check "writes nothing" "$nothing_written"
	check "asks once" '[ "$(cat "$d/curl.count")" = 1 ]'
	check "one line with the status" 'grep -qx "lux-gha registry: unavailable (HTTP $status); continuing without it" "$d/out" && [ ! -s "$d/err" ]'
	check_no_token
done

run_hook hook-409-always FAKE_CURL=409
check "exits 0" '[ "$rc" = 0 ]'
check "writes nothing" "$nothing_written"
check "retries 409 until LUXGHA_REGISTRY_WAIT (2s) runs out" '[ "$(cat "$d/curl.count")" -ge 2 ] && [ "$elapsed" -ge 2 ] && [ "$elapsed" -le 5 ]'
check "one line" "$one_line"

run_hook hook-409-then-200 FAKE_CURL="409 200" LUXGHA_REGISTRY_WAIT=10 \
	FAKE_BODY="{\"registry\":\"$reg\",\"read\":\"$reg/acme/widgets\",\"push\":\"$reg/acme/widgets\",\"trust\":\"trusted\",\"expiresAt\":\"x\"}"
check "exits 0" '[ "$rc" = 0 ]'
check "a 409 then a 200 writes both paths" '[ "$(cat "$d/curl.count")" = 2 ] && [ "$(wc -l <"$d/github_env")" = 2 ] && grep -qx "LUX_REGISTRY_PUSH=$reg/acme/widgets" "$d/github_env"'

run_hook hook-refused FAKE_CURL=refused
check "exits 0 when nothing listens" '[ "$rc" = 0 ]'
check "writes nothing" "$nothing_written"
check "one line with HTTP 000" 'grep -qx "lux-gha registry: unavailable (HTTP 000); continuing without it" "$d/out" && [ ! -s "$d/err" ]'

run_hook hook-no-host LUXGHA_REGISTRY_HOST=
check "exits 0 without LUXGHA_REGISTRY_HOST" '[ "$rc" = 0 ]'
check "asks nothing, writes nothing, prints nothing" '[ ! -f "$d/curl.count" ] && [ ! -s "$d/github_env" ] && [ ! -s "$d/out" ] && [ ! -s "$d/err" ]'

run_hook hook-no-identity LUXGHA_IDENTITY_FILE="$root/does-not-exist"
check "exits 0 without the identity file" '[ "$rc" = 0 ]'
check "asks nothing, writes nothing" '[ ! -f "$d/curl.count" ] && [ ! -s "$d/github_env" ]'

run_hook hook-github-env-unwritable FAKE_CURL=200 GITHUB_ENV=/proc/version \
	FAKE_BODY="{\"read\":\"$reg/acme/widgets\",\"push\":\"\",\"trust\":\"trusted\"}"
check "exits 0 when GITHUB_ENV cannot be written" '[ "$rc" = 0 ]'

# The credential helper wrapper strips the job's AWS environment and hands
# docker-credential-ecr-login the registry profile only.
cat >"$root/bin/docker-credential-ecr-login" <<'EOF2'
#!/bin/sh
env | grep '^AWS_' | sort >"$FAKE_CASE_DIR/helper.env"
printf '%s\n' "$@" >"$FAKE_CASE_DIR/helper.args"
cat >"$FAKE_CASE_DIR/helper.stdin"
echo '{"Username":"AWS","Secret":"x"}'
EOF2
chmod 755 "$root/bin/docker-credential-ecr-login"
name=wrapper
d=$root/$name
mkdir -p "$d"
rc=0
echo "$reg" | env -i PATH="$root/bin:/usr/bin:/bin" FAKE_CASE_DIR="$d" LUXGHA_REGISTRY_REGION=eu-west-1 \
	AWS_ACCESS_KEY_ID=job-key AWS_SECRET_ACCESS_KEY=job-secret AWS_SESSION_TOKEN=job-token AWS_PROFILE=job \
	AWS_ROLE_ARN=arn:aws:iam::123456789012:role/job AWS_WEB_IDENTITY_TOKEN_FILE=/job/token AWS_CONFIG_FILE=/job/config \
	AWS_SHARED_CREDENTIALS_FILE=/job/credentials AWS_DEFAULT_PROFILE=job AWS_REGION=us-east-1 \
	sh "$wrapper" get >"$d/out" 2>"$d/err" || rc=$?
check "exits with the helper's status" '[ "$rc" = 0 ]'
check "the helper sees exactly the registry profile's AWS env" '[ "$(cat "$d/helper.env")" = "$(printf "%s\n" AWS_CONFIG_FILE=/etc/lux-gha/aws-config AWS_ECR_DISABLE_CACHE=true AWS_PROFILE=lux-gha-registry AWS_REGION=eu-west-1)" ]'
check "arguments and stdin pass through" '[ "$(cat "$d/helper.args")" = get ] && [ "$(cat "$d/helper.stdin")" = "$reg" ] && grep -q Username "$d/out"'

name="wrapper-no-region"
d=$root/$name
mkdir -p "$d"
rc=0
env -i PATH="$root/bin:/usr/bin:/bin" FAKE_CASE_DIR="$d" sh "$wrapper" get </dev/null >"$d/out" 2>"$d/err" || rc=$?
check "fails without LUXGHA_REGISTRY_REGION, before the helper" '[ "$rc" = 1 ] && [ ! -f "$d/helper.env" ]'

# credential_process: POSTs the token from the identity file to the
# credentials route, failing on any HTTP error.
name=aws-credentials
d=$root/$name
mkdir -p "$d"
printf '%s' "$token" >"$d/identity"
rc=0
env -i PATH="$root/bin:/usr/bin:/bin" FAKE_CASE_DIR="$d" FAKE_CURL=200 FAKE_BODY='{"Version":1}' LUXGHA_RUNNER_ENDPOINT=http://10.0.0.5:8081 \
	LUXGHA_IDENTITY_FILE="$d/identity" sh "$creds" >"$d/out" 2>"$d/err" || rc=$?
check "exits 0" '[ "$rc" = 0 ]'
check "POST to the credentials route with --fail and a timeout" \
	'grep -qx "http://10.0.0.5:8081/runner/v1/aws-credentials" "$d/curl.args" && grep -qx -- "--fail" "$d/curl.args" && grep -qx -- "--max-time" "$d/curl.args" && grep -qx POST "$d/curl.args"'
check "token only in the curl config on stdin" '[ "$(cat "$d/curl.stdin")" = "header = \"Authorization: Bearer $token\"" ]'
check "prints the body for credential_process" '[ "$(cat "$d/out")" = "{\"Version\":1}" ]'
check_no_token

name=aws-credentials-503
d=$root/$name
mkdir -p "$d"
printf '%s' "$token" >"$d/identity"
rc=0
env -i PATH="$root/bin:/usr/bin:/bin" FAKE_CASE_DIR="$d" FAKE_CURL=503 LUXGHA_RUNNER_ENDPOINT=http://10.0.0.5:8081 \
	LUXGHA_IDENTITY_FILE="$d/identity" sh "$creds" >"$d/out" 2>"$d/err" || rc=$?
check "fails on an HTTP error, printing no credentials" '[ "$rc" != 0 ] && [ ! -s "$d/out" ]'

# The entrypoint merges credHelpers into the runner user's Docker config,
# keeps every other key, and points the runner at the hook.
existing='{"auths":{"ghcr.io":{"auth":"eDp5"}},"credHelpers":{"other.example.com":"pass"},"currentContext":"default"}'
mkdir -p "$root/registry-merge/.docker"
printf '%s\n' "$existing" >"$root/registry-merge/.docker/config.json"
run_case registry-merge LUXGHA_REGISTRY_HOST="$reg" LUXGHA_JOB_STARTED_HOOK="$hook"
check "exits 0" '[ "$rc" = 0 ]'
check "credHelpers gains the registry host; existing keys are kept" \
	'[ "$(jq -cS . "$d/.docker/config.json")" = "$(echo "$existing" | jq -cS --arg h "$reg" ".credHelpers[\$h] = \"lux-gha\"")" ]'
check "ACTIONS_RUNNER_HOOK_JOB_STARTED names the hook" 'grep -qx "ACTIONS_RUNNER_HOOK_JOB_STARTED=$hook" "$d/runner.env"'
check "nothing on stderr" '[ ! -s "$d/err" ]'
check_no_leak

run_case registry-fresh LUXGHA_REGISTRY_HOST="$reg" LUXGHA_JOB_STARTED_HOOK="$hook"
check "exits 0" '[ "$rc" = 0 ]'
check "a new config holds only credHelpers" '[ "$(jq -c . "$d/.docker/config.json")" = "{\"credHelpers\":{\"$reg\":\"lux-gha\"}}" ]'

mkdir -p "$root/registry-bad-config/.docker"
printf 'not json\n' >"$root/registry-bad-config/.docker/config.json"
run_case registry-bad-config LUXGHA_REGISTRY_HOST="$reg" LUXGHA_JOB_STARTED_HOOK="$hook"
check "exits 0 and reaches the runner" '[ "$rc" = 0 ] && [ -f "$d/runner.args" ]'
check "an unparsable config is left as it was" '[ "$(cat "$d/.docker/config.json")" = "not json" ]'
check "one warning line" '[ "$(wc -l <"$d/err")" = 1 ] && grep -q "^lux-gha-runner: warning: " "$d/err"'
check "no hook without the helper" '! grep -q "^ACTIONS_RUNNER_HOOK_JOB_STARTED=" "$d/runner.env"'

run_case registry-unset
check "exits 0" '[ "$rc" = 0 ]'
check "without LUXGHA_REGISTRY_HOST: no Docker config and no hook" '[ ! -e "$d/.docker" ] && ! grep -q "^ACTIONS_RUNNER_HOOK_JOB_STARTED=" "$d/runner.env"'

if [ "$failures" -ne 0 ]; then
	echo "entrypoint_test: $failures failed"
	exit 1
fi
echo "entrypoint_test: OK"
