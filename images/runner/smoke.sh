#!/bin/sh
# Local smoke test for the runner image: builds it for linux/arm64 into the
# local Docker and checks its tools, user and entrypoint failure modes. With
# --privileged it also starts rootless dockerd and builds an image with
# buildx. That is an approximation only: Lux nested Runs are never
# privileged, so it does not prove the Lux nested-container contract.
#
# Usage: images/runner/smoke.sh [--privileged] [--keep]
set -eu

TAG=${TAG:-lux-gha-runner:smoke}
dir=$(cd "$(dirname "$0")" && pwd)
privileged=false keep=false
for arg in "$@"; do
	case $arg in
	--privileged) privileged=true ;;
	--keep) keep=true ;;
	*) echo "usage: $0 [--privileged] [--keep]" >&2; exit 2 ;;
	esac
done

fail() { echo "FAIL: $*" >&2; exit 1; }
run() { docker run --rm --platform linux/arm64 "$@"; }

docker buildx build --platform linux/arm64 --load -t "$TAG" -f "$dir/Containerfile" "$dir"
$keep || trap 'docker image rm "$TAG" >/dev/null 2>&1 || true' EXIT

echo "== tools"
run --entrypoint sh "$TAG" -euc '
	cd "$RUNNER_DIR" && printf "runner %s\n" "$(./config.sh --version)"
	git --version
	docker --version
	docker buildx version
	docker compose version
	python3 --version
	id
	[ "$(id -u)" = 1000 ] && [ "$(id -un)" = runner ]
	grep -x "runner:1:999" /etc/subuid && grep -x "runner:1001:64535" /etc/subgid
	getcap /usr/bin/newuidmap /usr/bin/newgidmap
	[ -z "$(find / -xdev -type f -perm /6000 2>/dev/null)" ] && echo "no setuid/setgid files"
	! command -v sudo >/dev/null && echo "no sudo"
'

echo "== entrypoint without LUXGHA_JITCONFIG_FILE"
if out=$(run "$TAG" 2>&1); then fail "entrypoint succeeded without LUXGHA_JITCONFIG_FILE"; fi
echo "$out"
echo "$out" | grep -q "LUXGHA_JITCONFIG_FILE is not set" || fail "unexpected message"

echo "== entrypoint with an empty JIT config file"
if out=$(run -e LUXGHA_JITCONFIG_FILE=/dev/null "$TAG" 2>&1); then fail "entrypoint succeeded with an empty config"; fi
echo "$out"
echo "$out" | grep -q "JIT config file is empty" || fail "unexpected message"

echo "== entrypoint when dockerd cannot start (unprivileged container), Docker required"
secret=not-a-real-jitconfig-$$
if out=$(run --entrypoint sh "$TAG" -c "printf %s $secret > /tmp/jit; LUXGHA_JITCONFIG_FILE=/tmp/jit exec lux-gha-runner" 2>&1); then
	fail "entrypoint succeeded without a working dockerd"
fi
echo "$out" | tail -n 3
echo "$out" | grep -q "did not come up within 60s" || fail "unexpected message"
echo "$out" | grep -q "$secret" && fail "JIT config leaked into output"

# The runner itself rejects the fake config, which shows the entrypoint got
# as far as exec'ing run.sh.
echo "== entrypoint when dockerd cannot start, LUXGHA_DOCKER=optional"
out=$(run -e LUXGHA_DOCKER=optional --entrypoint sh "$TAG" -c "printf %s $secret > /tmp/jit; LUXGHA_JITCONFIG_FILE=/tmp/jit exec lux-gha-runner" 2>&1) || true
echo "$out" | tail -n 4
echo "$out" | grep -q "continuing without Docker" || fail "did not continue without Docker"
echo "$out" | grep -q "Base-64" || fail "run.sh was not reached"
echo "$out" | grep -q "$secret" && fail "JIT config leaked into output"

if $privileged; then
	echo "== APPROXIMATION (--privileged, not the Lux nested contract): entrypoint starts rootless dockerd"
	out=$(run --privileged --entrypoint sh "$TAG" -c "printf %s $secret > /tmp/jit; LUXGHA_JITCONFIG_FILE=/tmp/jit exec lux-gha-runner" 2>&1) || true
	echo "$out" | tail -n 3
	echo "$out" | grep -q "did not come up" && fail "dockerd did not come up under --privileged"
	echo "$out" | grep -q "Base-64" || fail "run.sh was not reached"
	echo "$out" | grep -q "$secret" && fail "JIT config leaked into output"

	echo "== APPROXIMATION (--privileged, not the Lux nested contract): rootless dockerd + buildx"
	run --privileged --entrypoint sh "$TAG" -euc '
		mkdir -p "$XDG_RUNTIME_DIR"
		setsid dockerd-rootless.sh </dev/null >/tmp/dockerd.log 2>&1 &
		for i in $(seq 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
		docker info >/dev/null 2>&1 || { tail -n 20 /tmp/dockerd.log; exit 1; }
		echo "rootless=$(docker info --format "{{.SecurityOptions}}" | grep -o rootless) driver=$(docker info --format "{{.Driver}}")"
		mkdir -p /tmp/b && printf "FROM scratch\nCOPY f /f\n" > /tmp/b/Dockerfile && echo built > /tmp/b/f
		docker buildx build --load -t smoke-scratch /tmp/b 2>&1 | tail -n 2
		docker image inspect smoke-scratch --format "loaded {{.Id}}"
	'
fi
echo "smoke: OK"
