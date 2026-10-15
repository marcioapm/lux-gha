#!/bin/sh
# Local smoke test for the runner image: builds it for linux/arm64 into the
# local Docker and checks its tools, user, file capabilities, the entrypoint
# lifecycle (entrypoint_test.sh, run inside the image) and its failure modes
# against the real runner.
#
# --privileged also starts rootless dockerd and builds with a docker-container
# buildx builder. That is an approximation only: Lux nested Runs are never
# privileged, so it does not prove the Lux nested-container contract.
#
# --checksum-negative only rebuilds the runner layer with a wrong
# RUNNER_SHA256_ARM64 and requires that build to fail at the checksum check.
#
# Usage: images/runner/smoke.sh [--privileged] [--keep] | --checksum-negative
set -eu

TAG=${TAG:-lux-gha-runner:smoke}
dir=$(cd "$(dirname "$0")" && pwd)
privileged=false keep=false checksum_negative=false
for arg in "$@"; do
	case $arg in
	--privileged) privileged=true ;;
	--keep) keep=true ;;
	--checksum-negative) checksum_negative=true ;;
	*) echo "usage: $0 [--privileged] [--keep] | --checksum-negative" >&2; exit 2 ;;
	esac
done

fail() { echo "FAIL: $*" >&2; exit 1; }
run() { docker run --rm --platform linux/arm64 "$@"; }

sh "$dir/../../scripts/check-license.sh" || fail "images/runner/LICENSE does not match the root LICENSE"

if $checksum_negative; then
	log=$(mktemp)
	trap 'rm -f "$log"' EXIT
	echo "== build with RUNNER_SHA256_ARM64 set to 64 zeros"
	# No --load and no tag: a failed build must leave nothing behind.
	if docker buildx build --platform linux/arm64 --progress=plain \
		--build-arg RUNNER_SHA256_ARM64=0000000000000000000000000000000000000000000000000000000000000000 \
		-f "$dir/Containerfile" "$dir" >"$log" 2>&1; then
		fail "build succeeded with a wrong runner checksum"
	fi
	grep -E "runner.tar.gz: FAILED|did NOT match|ERROR" "$log"
	grep -q "/tmp/runner.tar.gz: FAILED" "$log" || fail "build failed, but not at the runner checksum check"
	grep -q "+ tar -xzf /tmp/runner.tar.gz" "$log" && fail "runner tarball was extracted"
	echo "smoke --checksum-negative: OK"
	exit 0
fi

docker buildx build --platform linux/arm64 --load -t "$TAG" -f "$dir/Containerfile" "$dir"
$keep || trap 'docker image rm "$TAG" >/dev/null 2>&1 || true' EXIT
runner_version=$(docker image inspect "$TAG" --format '{{index .Config.Labels "io.github.actions.runner.version"}}')

echo "== tools"
# shellcheck disable=SC2016 # the script expands inside the container
run -e WANT_RUNNER="$runner_version" --entrypoint sh "$TAG" -euc '
	v=$(cd "$RUNNER_DIR" && ./config.sh --version)
	echo "runner $v"
	[ "$v" = "$WANT_RUNNER" ] || { echo "runner version $v, pinned $WANT_RUNNER" >&2; exit 1; }
	git --version
	docker --version
	docker buildx version
	docker compose version
	python3 --version
	# The official Node.js linux-arm64 builds (actions/setup-node) link libatomic.
	ldconfig -p | grep -q "libatomic.so.1 " || { echo "libatomic.so.1 is missing" >&2; exit 1; }
	id
	[ "$(id -u)" = 1000 ] && [ "$(id -un)" = runner ]
	grep -x "runner:1:999" /etc/subuid && grep -x "runner:1001:64535" /etc/subgid
	getcap /usr/bin/newuidmap /usr/bin/newgidmap
	[ "$(getcap /usr/bin/newuidmap)" = "/usr/bin/newuidmap cap_setuid=ep" ] || { echo "newuidmap lacks exactly cap_setuid=ep" >&2; exit 1; }
	[ "$(getcap /usr/bin/newgidmap)" = "/usr/bin/newgidmap cap_setgid=ep" ] || { echo "newgidmap lacks exactly cap_setgid=ep" >&2; exit 1; }
	[ -z "$(find / -xdev -type f -perm /6000 2>/dev/null)" ] && echo "no setuid/setgid files"
	! command -v sudo >/dev/null && echo "no sudo"
'

echo "== licence"
[ "$(docker image inspect "$TAG" --format '{{index .Config.Labels "org.opencontainers.image.licenses"}}')" = Elastic-2.0 ] ||
	fail "image lacks the org.opencontainers.image.licenses=Elastic-2.0 label"
# shellcheck disable=SC2016 # the script expands inside the container
run --entrypoint sh "$TAG" -euc '
	f=/usr/share/lux-gha/LICENSE
	[ -f "$f" ] || { echo "$f missing" >&2; exit 1; }
	[ "$(stat -c %a "$f")" = 644 ] || { echo "$f mode $(stat -c %a "$f"), want 644" >&2; exit 1; }
	cat "$f"
' | cmp - "$dir/LICENSE" || fail "/usr/share/lux-gha/LICENSE in the image differs from images/runner/LICENSE"
echo "/usr/share/lux-gha/LICENSE present, mode 644, matches images/runner/LICENSE"

echo "== entrypoint lifecycle (entrypoint_test.sh against the installed entrypoint)"
run -i --entrypoint sh "$TAG" -s /usr/local/bin/lux-gha-runner <"$dir/entrypoint_test.sh" ||
	fail "entrypoint lifecycle test"

echo "== entrypoint without LUXGHA_JITCONFIG_FILE"
if out=$(run "$TAG" 2>&1); then fail "entrypoint succeeded without LUXGHA_JITCONFIG_FILE"; fi
echo "$out"
echo "$out" | grep -q "LUXGHA_JITCONFIG_FILE is not set" || fail "unexpected message"

echo "== entrypoint with an empty JIT config file"
if out=$(run -e LUXGHA_JITCONFIG_FILE=/dev/null "$TAG" 2>&1); then fail "entrypoint succeeded with an empty config"; fi
echo "$out"
echo "$out" | grep -q "JIT config file is empty" || fail "unexpected message"

secret=not-a-real-jitconfig-$$
# run_with_config [docker run options...]: the entrypoint with $secret as its JIT config file.
run_with_config() {
	run "$@" --entrypoint sh "$TAG" -c "printf %s $secret > /tmp/jit; LUXGHA_JITCONFIG_FILE=/tmp/jit exec lux-gha-runner"
}

echo "== entrypoint when dockerd cannot start (unprivileged container), Docker required"
if out=$(run_with_config 2>&1); then
	fail "entrypoint succeeded without a working dockerd"
fi
echo "$out" | tail -n 3
echo "$out" | grep -q "did not come up within 60s" || fail "unexpected message"
echo "$out" | grep -q "$secret" && fail "JIT config leaked into output"

# The real runner rejects the fake config, which shows run.sh was reached
# with invalid input. Exec and exit propagation are covered by the lifecycle
# test above.
echo "== entrypoint when dockerd cannot start, LUXGHA_DOCKER=optional"
out=$(run_with_config -e LUXGHA_DOCKER=optional 2>&1) || true
echo "$out" | tail -n 4
echo "$out" | grep -q "continuing without Docker" || fail "did not continue without Docker"
echo "$out" | grep -q "Base-64" || fail "run.sh was not reached"
echo "$out" | grep -q "$secret" && fail "JIT config leaked into output"

if $privileged; then
	echo "== APPROXIMATION (--privileged, not the Lux nested contract): entrypoint starts rootless dockerd"
	out=$(run_with_config --privileged 2>&1) || true
	echo "$out" | tail -n 3
	echo "$out" | grep -q "did not come up" && fail "dockerd did not come up under --privileged"
	echo "$out" | grep -q "Base-64" || fail "run.sh was not reached"
	echo "$out" | grep -q "$secret" && fail "JIT config leaked into output"

	echo "== APPROXIMATION (--privileged, not the Lux nested contract): rootless dockerd + docker-container buildx builder"
	marker=smoke-marker-$$-$(date +%s)
	# The store gets its own volume, as Lux mounts one there: overlay on
	# top of the container's overlay root fails to mount BuildKit's layers.
	# shellcheck disable=SC2016 # the script expands inside the container
	run --privileged -v /home/runner/.local/share/docker -e MARKER="$marker" --entrypoint sh "$TAG" -euc '
		mkdir -p "$XDG_RUNTIME_DIR"
		setsid dockerd-rootless.sh --host "$DOCKER_HOST" </dev/null >/tmp/dockerd.log 2>&1 &
		for i in $(seq 60); do timeout 5 docker info >/dev/null 2>&1 && break; sleep 1; done
		docker info >/dev/null 2>&1 || { tail -n 20 /tmp/dockerd.log; exit 1; }
		echo "rootless=$(docker info --format "{{.SecurityOptions}}" | grep -o rootless) driver=$(docker info --format "{{.Driver}}")"
		docker buildx create --name smoke --driver docker-container --use
		docker buildx inspect --bootstrap >/tmp/bootstrap.log 2>&1 || { cat /tmp/bootstrap.log; exit 1; }
		grep -E "^(Driver|Status):" /tmp/bootstrap.log
		grep -Eq "^Driver: +docker-container$" /tmp/bootstrap.log || { echo "builder is not docker-container" >&2; exit 1; }
		mkdir -p /tmp/b
		printf "FROM busybox:1.37\nARG MARKER\nRUN printf %%s \"\$MARKER\" > /marker\nCMD [\"cat\", \"/marker\"]\n" >/tmp/b/Dockerfile
		docker buildx build --builder smoke --load --build-arg MARKER="$MARKER" -t smoke-marker /tmp/b >/tmp/build.log 2>&1 ||
			{ cat /tmp/build.log; exit 1; }
		got=$(docker run --rm smoke-marker)
		[ "$got" = "$MARKER" ] || { echo "built image printed \"$got\", want \"$MARKER\"" >&2; exit 1; }
		echo "docker-container builder: built, loaded and ran smoke-marker; marker matches"
	'
fi
echo "smoke: OK"
