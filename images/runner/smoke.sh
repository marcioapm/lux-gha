#!/bin/sh
# Local smoke test for the runner image: builds it for $PLATFORM
# (linux/arm64, the default, or linux/amd64) into the local Docker and checks
# its architecture, tools, user, file capabilities, the BuildKit archive
# against the Containerfile's pins, the entrypoint lifecycle
# (entrypoint_test.sh, run inside the image) and its failure modes against the
# real runner. Run it on a host of that architecture: under emulation the
# results say little about the image on real hardware.
#
# --privileged also starts rootless dockerd through the entrypoint, checks
# the BuildKit image it preloads (tags, architecture, digests), bootstraps a
# docker-container builder with no network (so from the preloaded image
# only), and with network checks that the pinned tag pulls nothing and
# builds. That is an approximation only: Lux nested Runs are never
# privileged, so it does not prove the Lux nested-container contract.
#
# --checksum-negative only rebuilds the runner layer with a wrong runner
# SHA-256 for $PLATFORM (RUNNER_SHA256_ARM64 or RUNNER_SHA256_AMD64) and
# requires that build to fail at the checksum check.
#
# Usage: [PLATFORM=linux/arm64|linux/amd64] [TAG=...] images/runner/smoke.sh [--privileged] [--keep] | --checksum-negative
set -eu

TAG=${TAG:-lux-gha-runner:smoke}
PLATFORM=${PLATFORM:-linux/arm64}
case $PLATFORM in
linux/arm64) arch=arm64 tc_arch=arm64 sha_arg=RUNNER_SHA256_ARM64 elf_machine=b7 bk_arg=BUILDKIT_DIGEST_ARM64 ;;
linux/amd64) arch=amd64 tc_arch=x64 sha_arg=RUNNER_SHA256_AMD64 elf_machine=3e bk_arg=BUILDKIT_DIGEST_AMD64 ;;
*) echo "PLATFORM must be linux/arm64 or linux/amd64, not $PLATFORM" >&2; exit 2 ;;
esac
dir=$(cd "$(dirname "$0")" && pwd)
# The BuildKit pins, from the Containerfile the image is built from.
containerfile_arg() { sed -n "s/^ARG $1=//p" "$dir/Containerfile"; }
bk_version=$(containerfile_arg BUILDKIT_VERSION)
bk_index=$(containerfile_arg BUILDKIT_DIGEST)
bk_manifest=$(containerfile_arg "$bk_arg")
# docker run options passing the pins to in-container checks. The values
# hold no whitespace, so the variable is split unquoted.
bk_env="-e BK_VERSION=$bk_version -e BK_INDEX=$bk_index -e BK_MANIFEST=$bk_manifest"
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
run() { docker run --rm --platform "$PLATFORM" "$@"; }

sh "$dir/../../scripts/check-license.sh" || fail "images/runner/LICENSE does not match the root LICENSE"

if $checksum_negative; then
	log=$(mktemp)
	trap 'rm -f "$log"' EXIT
	echo "== $PLATFORM build with $sha_arg set to 64 zeros"
	# No --load and no tag: a failed build must leave nothing behind.
	if docker buildx build --platform "$PLATFORM" --progress=plain \
		--build-arg "$sha_arg=0000000000000000000000000000000000000000000000000000000000000000" \
		-f "$dir/Containerfile" "$dir" >"$log" 2>&1; then
		fail "build succeeded with a wrong runner checksum"
	fi
	grep -E "runner.tar.gz: FAILED|did NOT match|ERROR" "$log"
	grep -q "/tmp/runner.tar.gz: FAILED" "$log" || fail "build failed, but not at the runner checksum check"
	grep -q "+ tar -xzf /tmp/runner.tar.gz" "$log" && fail "runner tarball was extracted"
	echo "smoke --checksum-negative: OK"
	exit 0
fi

docker buildx build --platform "$PLATFORM" --load -t "$TAG" -f "$dir/Containerfile" "$dir"
$keep || trap 'docker image rm "$TAG" >/dev/null 2>&1 || true' EXIT
runner_version=$(docker image inspect "$TAG" --format '{{index .Config.Labels "io.github.actions.runner.version"}}')

echo "== architecture"
got=$(docker image inspect "$TAG" --format '{{.Os}}/{{.Architecture}}')
echo "image $got"
[ "$got" = "$PLATFORM" ] || fail "image is $got, want $PLATFORM"

echo "== tools"
# shellcheck disable=SC2016 # the script expands inside the container
run -e WANT_RUNNER="$runner_version" -e WANT_GH="$(containerfile_arg GH_VERSION)" -e WANT_ARCH="$arch" -e WANT_ELF="$elf_machine" --entrypoint sh "$TAG" -euc '
	[ "$(dpkg --print-architecture)" = "$WANT_ARCH" ] || { echo "dpkg architecture $(dpkg --print-architecture), want $WANT_ARCH" >&2; exit 1; }
	# e_machine (low byte, offset 18): b7 aarch64, 3e x86_64. Under binfmt
	# emulation a runner tarball of the wrong architecture would still run.
	m=$(od -An -tx1 -j18 -N1 "$RUNNER_DIR/bin/Runner.Listener" | tr -d " ")
	[ "$m" = "$WANT_ELF" ] || { echo "Runner.Listener ELF machine $m, want $WANT_ELF" >&2; exit 1; }
	echo "Runner.Listener ELF machine $m"
	v=$(cd "$RUNNER_DIR" && ./config.sh --version)
	echo "runner $v"
	[ "$v" = "$WANT_RUNNER" ] || { echo "runner version $v, pinned $WANT_RUNNER" >&2; exit 1; }
	git --version
	docker --version
	docker buildx version
	docker compose version
	python3 --version
	socat -V
	session-manager-plugin --version
	aws --version
	# docker-credential-ecr-login for the image architecture, behind the
	# lux-gha wrapper, its AWS profile and credential_process.
	m=$(od -An -tx1 -j18 -N1 /usr/local/bin/docker-credential-ecr-login | tr -d " ")
	[ "$m" = "$WANT_ELF" ] || { echo "docker-credential-ecr-login ELF machine $m, want $WANT_ELF" >&2; exit 1; }
	docker-credential-ecr-login -v
	# gh for the image architecture, at the pinned version.
	m=$(od -An -tx1 -j18 -N1 /usr/local/bin/gh | tr -d " ")
	[ "$m" = "$WANT_ELF" ] || { echo "gh ELF machine $m, want $WANT_ELF" >&2; exit 1; }
	v=$(gh --version | head -1)
	echo "$v"
	echo "$v" | grep -q "^gh version $WANT_GH " || { echo "gh is $v, pinned $WANT_GH" >&2; exit 1; }
	for f in /usr/local/bin/docker-credential-lux-gha /usr/local/bin/lux-gha-aws-credentials /usr/local/lib/lux-gha/job-started.sh; do
		[ "$(stat -c "%U %a" "$f")" = "root 755" ] || { echo "$f is $(stat -c "%U %a" "$f"), want root 755" >&2; exit 1; }
	done
	grep -qx "credential_process = /usr/local/bin/lux-gha-aws-credentials" /etc/lux-gha/aws-config ||
		{ echo "/etc/lux-gha/aws-config lacks the credential_process" >&2; exit 1; }
	echo "registry credential helper, profile and hook installed"
	# The official Node.js linux-arm64 builds (actions/setup-node) link
	# libatomic; the image installs it on every architecture.
	ldconfig -p | grep -q "libatomic.so.1 " || { echo "libatomic.so.1 is missing" >&2; exit 1; }
	id
	[ "$(id -u)" = 1000 ] && [ "$(id -un)" = runner ]
	grep -x "runner:1:999" /etc/subuid && grep -x "runner:1001:64535" /etc/subgid
	getcap /usr/bin/newuidmap /usr/bin/newgidmap
	[ "$(getcap /usr/bin/newuidmap)" = "/usr/bin/newuidmap cap_setuid=ep" ] || { echo "newuidmap lacks exactly cap_setuid=ep" >&2; exit 1; }
	[ "$(getcap /usr/bin/newgidmap)" = "/usr/bin/newgidmap cap_setgid=ep" ] || { echo "newgidmap lacks exactly cap_setgid=ep" >&2; exit 1; }
	[ -z "$(find / -xdev -type f -perm /6000 2>/dev/null)" ] && echo "no setuid/setgid files"
	! command -v sudo >/dev/null && echo "no sudo"
	# The CPython that setup-python installs has its RUNPATH under /opt/hostedtoolcache.
	t=/opt/hostedtoolcache
	[ "${RUNNER_TOOL_CACHE:-}" = "$t" ] && [ "${AGENT_TOOLSDIRECTORY:-}" = "$t" ] ||
		{ echo "RUNNER_TOOL_CACHE=${RUNNER_TOOL_CACHE:-} AGENT_TOOLSDIRECTORY=${AGENT_TOOLSDIRECTORY:-}, want $t" >&2; exit 1; }
	[ "$(stat -c "%U:%G %a" "$t")" = "runner:runner 755" ] || { echo "$t is $(stat -c "%U:%G %a" "$t"), want runner:runner 755" >&2; exit 1; }
	mkdir "$t/smoke"
	rmdir "$t/smoke"
	echo "tool cache $t: runner:runner 755, writable as $(id -un)"
'

echo "== preinstalled tool cache"
# Versions from the Containerfile. Each entry must sit where
# @actions/tool-cache's find() looks (<tool>/<version>/<arch> plus
# <arch>.complete), run as runner, and run with an empty environment, which
# catches a binary that only loads its libraries through LD_LIBRARY_PATH.
# The JDK directory name, derived as in the Containerfile: Adoptium's semver
# for jdk-A.B.C[.P]+N is A.B.C+(P*100+N), "+" becoming "-" in the tool cache.
java_release=$(containerfile_arg JAVA_RELEASE)
v=${java_release#jdk-}
jtc=$(echo "$v" | awk -F+ '{split($1, a, "."); printf "%d.%d.%d-%d", a[1], a[2], a[3], a[4] * 100 + $2}')
# shellcheck disable=SC2016 # the script expands inside the container
run --network none -e PY="$(containerfile_arg PYTHON_VERSION)" -e A="$tc_arch" \
	-e JV="$(containerfile_arg JAVA_TOOLCACHE_VERSION)" -e JR="$java_release" -e JTC="$jtc" \
	-e NV="$(containerfile_arg NODE_VERSION)" \
	--entrypoint sh "$TAG" -euc '
	t=/opt/hostedtoolcache
	entry() {
		[ -d "$t/$1/$2/$3" ] || { echo "$t/$1/$2/$3 missing" >&2; exit 1; }
		[ -f "$t/$1/$2/$3.complete" ] || { echo "$t/$1/$2/$3.complete missing" >&2; exit 1; }
		echo "$1/$2/$3 and $3.complete present"
	}
	clean() { env -i "$@"; }
	entry Python "$PY" "$A"
	p=$t/Python/$PY/$A/bin/python${PY%.*}
	"$p" -c "import ssl, sqlite3, sys; print(sys.version.split()[0], ssl.OPENSSL_VERSION, sqlite3.sqlite_version)"
	[ "$("$p" -c "import platform; print(platform.python_version())")" = "$PY" ] || { echo "$p is not $PY" >&2; exit 1; }
	clean "$p" -c "import ssl, sqlite3" || { echo "$p fails with an empty environment" >&2; exit 1; }
	clean "$t/Python/$PY/$A/python" -m pip --version
	echo "python $PY runs with env -i"
	# Stripped at build time; the stdlib a job uses must still work offline.
	l=$t/Python/$PY/$A/lib/python${PY%.*}
	for x in "$l/test" "$l"/config-*/libpython*.a "$t/Java_Temurin-Hotspot_jdk"/*/"$A"/lib/src.zip; do
		[ ! -e "$x" ] || { echo "$x is present" >&2; exit 1; }
	done
	clean "$p" -c "import ssl, sqlite3, venv, ensurepip" || { echo "$p: stdlib import failed" >&2; exit 1; }
	venv=$(mktemp -d)/venv
	"$p" -m venv "$venv" || { echo "$p -m venv failed" >&2; exit 1; }
	"$venv/bin/python" -m pip --version
	echo "python test suite, static libpython and JDK src.zip absent; python -m venv works offline"
	# The only JDK directory must be the name the Containerfile derives from
	# JAVA_RELEASE (JTC), plus .0.LTS when the release file says -LTS.
	jdir=$(ls "$t/Java_Temurin-Hotspot_jdk")
	j=$t/Java_Temurin-Hotspot_jdk/$jdir/$A
	jtc=$JTC
	if grep -qx "JAVA_RUNTIME_VERSION=\"${JR#jdk-}-LTS\"" "$j/release"; then jtc=$jtc.0.LTS
	else grep -qx "JAVA_RUNTIME_VERSION=\"${JR#jdk-}\"" "$j/release" || { echo "$j/release does not name $JR" >&2; exit 1; }; fi
	[ "$jdir" = "$jtc" ] || { echo "JDK directory [$jdir], want $jtc for $JR" >&2; exit 1; }
	[ "$JV" = "$jtc" ] || { echo "JAVA_TOOLCACHE_VERSION $JV, want $jtc for $JR" >&2; exit 1; }
	entry Java_Temurin-Hotspot_jdk "$jtc" "$A"
	if [ "$A" = arm64 ]; then
		entry Java_Temurin-Hotspot_jdk "$jtc" aarch64
		[ "$(readlink "$t/Java_Temurin-Hotspot_jdk/$jtc/aarch64")" = arm64 ] || { echo "aarch64 is not a symlink to arm64" >&2; exit 1; }
	fi
	echo "$JR: JDK directory $jtc, $(grep JAVA_RUNTIME_VERSION= "$j/release")"
	"$j/bin/java" -version
	clean "$j/bin/java" -version 2>/dev/null || { echo "java fails with an empty environment" >&2; exit 1; }
	echo "java runs with env -i"
	entry node "$NV" "$A"
	n=$t/node/$NV/$A/bin/node
	[ "$("$n" -v)" = "v$NV" ] || { echo "$n is not v$NV" >&2; exit 1; }
	[ "$(clean "$n" -v)" = "v$NV" ] || { echo "node fails with an empty environment" >&2; exit 1; }
	clean PATH="${n%/*}" "${n%/*}/npm" -v >/dev/null || { echo "npm fails" >&2; exit 1; }
	echo "node v$NV and npm run with env -i"
	bad=$(find "$t" \( ! -user runner -o ! -group runner \) -print -quit)
	[ -z "$bad" ] || { echo "$bad is not owned by runner:runner" >&2; exit 1; }
	echo "everything under $t is owned by runner:runner; running as $(id -un)"
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

echo "== BuildKit archive"
[ "$(docker image inspect "$TAG" --format '{{index .Config.Labels "io.github.marcioapm.lux-gha.buildkit"}}')" = "moby/buildkit:$bk_version@$bk_index" ] ||
	fail "image lacks the label io.github.marcioapm.lux-gha.buildkit=moby/buildkit:$bk_version@$bk_index"
# Offline and without Docker: the archive must hold the pinned index under
# both tags, and the pinned $PLATFORM manifest with every blob it names.
# shellcheck disable=SC2016,SC2086 # the script expands inside the container; bk_env splits
run --network none $bk_env -e WANT_ARCH="$arch" \
	--entrypoint sh "$TAG" -euc '
	a=/usr/share/lux-gha/buildkit.tar
	[ "$(stat -c %a "$a")" = 644 ] || { echo "$a mode $(stat -c %a "$a"), want 644" >&2; exit 1; }
	x=$(mktemp -d)
	tar -xf "$a" -C "$x"
	refs=$(jq -r --arg d "$BK_INDEX" ".manifests[] | select(.digest == \$d) | .annotations[\"io.containerd.image.name\"]" "$x/index.json" | sort | paste -sd " " -)
	[ "$refs" = "docker.io/moby/buildkit:buildx-stable-1 docker.io/moby/buildkit:$BK_VERSION" ] || { echo "index.json names [$refs] for $BK_INDEX" >&2; exit 1; }
	[ "$(jq ".manifests | length" "$x/index.json")" = 2 ] || { echo "index.json has other entries" >&2; exit 1; }
	b=$x/blobs/sha256
	got=$(jq -r --arg a "$WANT_ARCH" ".manifests[] | select(.platform.os == \"linux\" and .platform.architecture == \$a) | .digest" "$b/${BK_INDEX#sha256:}")
	[ "$got" = "$BK_MANIFEST" ] || { echo "index lists $got for linux/$WANT_ARCH, pinned $BK_MANIFEST" >&2; exit 1; }
	m=$b/${BK_MANIFEST#sha256:}
	cfg=$(jq -r .config.digest "$m")
	[ "$(jq -r .architecture "$b/${cfg#sha256:}")" = "$WANT_ARCH" ] || { echo "config architecture is not $WANT_ARCH" >&2; exit 1; }
	for d in "$BK_INDEX" "$BK_MANIFEST" "$cfg" $(jq -r ".layers[].digest" "$m"); do
		echo "${d#sha256:}  $b/${d#sha256:}"
	done | sha256sum -c --quiet -
	# Only the $PLATFORM image: the index, its manifest, config and layers.
	[ "$(ls "$b" | wc -l)" = $((3 + $(jq ".layers | length" "$m"))) ] || { echo "archive holds other blobs" >&2; exit 1; }
	echo "buildkit.tar: moby/buildkit:$BK_VERSION and :buildx-stable-1 -> $BK_INDEX, linux/$WANT_ARCH $BK_MANIFEST, $(stat -c %s "$a") bytes; blobs verified"
'

echo "== cache server patch (README \"Using the cache server\")"
pin=$(containerfile_arg RUNNER_RESULTS_URL_LITERALS)
# shellcheck disable=SC2016 # the script expands inside the container
run -e PIN="$pin" --entrypoint sh "$TAG" -euc '
	pw=/usr/local/lib/lux-gha/patch-worker.sh
	[ "$(stat -c "%U %a" "$pw")" = "root 755" ] || { echo "$pw is $(stat -c "%U %a" "$pw"), want root 755" >&2; exit 1; }
	pl=/usr/local/lib/lux-gha/pre-listener.sh
	[ "$(stat -c "%U %a" "$pl")" = "root 644" ] || { echo "$pl is $(stat -c "%U %a" "$pl"), want root 644" >&2; exit 1; }
	bash -n "$pl"
	# Shipped unpatched: pre-listener.sh patches at every run-helper.sh start.
	"$pw" check "$PIN" "$RUNNER_DIR/bin/Runner.Worker.dll"
	echo "Runner.Worker.dll unpatched with $PIN literal(s); pre-listener.sh installed"
	# The shipped run-helper.sh.template under BASH_ENV, as run.sh starts it,
	# against a copy of the Worker and a listener that reports what it sees.
	r=$(mktemp -d)
	cp "$RUNNER_DIR/run-helper.sh.template" "$r/run-helper.sh"
	mkdir "$r/bin"
	cp "$RUNNER_DIR/bin/Runner.Worker.dll" "$r/bin/"
	# The listener writes what it sees to files: run-helper.sh prints its
	# own lines after the listener exits.
	printf "#!/bin/sh\n%s count \"\$(dirname \"\$0\")/Runner.Worker.dll\" >%s/seen\necho \"\${BASH_ENV-unset}\" >%s/bash_env\n" "$pw" "$r" "$r" >"$r/bin/Runner.Listener"
	chmod 755 "$r/run-helper.sh" "$r/bin/Runner.Listener"
	BASH_ENV=$pl "$r/run-helper.sh" >/dev/null 2>&1 || true
	got="$(cat "$r/seen" 2>/dev/null || :) $(cat "$r/bash_env" 2>/dev/null || :)"
	[ "$got" = "0 $PIN unset" ] || { echo "run-helper.sh under BASH_ENV: listener saw \"$got\", want \"0 $PIN unset\"" >&2; exit 1; }
	echo "shipped run-helper.sh patches through BASH_ENV; the listener sees no BASH_ENV"
	# A copy patches cleanly, as pre-listener.sh would.
	cp "$RUNNER_DIR/bin/Runner.Worker.dll" /tmp/w.dll
	"$pw" apply /tmp/w.dll
	[ "$("$pw" count /tmp/w.dll)" = "0 $PIN" ] || { echo "patched copy counts $("$pw" count /tmp/w.dll)" >&2; exit 1; }
' || fail "cache server patch"
run -i --entrypoint sh "$TAG" -s /usr/local/lib/lux-gha/patch-worker.sh <"$dir/patch-worker_test.sh" ||
	fail "patch-worker test"

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
	# privileged_entrypoint MODE [docker run options...]: the installed
	# entrypoint under --privileged, with RUNNER_DIR holding the run.sh
	# below in place of GitHub's runner. It runs after the entrypoint's
	# load, checks the store against the pins, then does MODE's checks.
	# The store gets its own volume, as Lux mounts one there: overlay on
	# top of the container's overlay root fails to mount BuildKit's layers.
	privileged_entrypoint() {
		mode=$1
		shift
		# shellcheck disable=SC2016,SC2086 # the scripts expand inside the container; bk_env splits
		run -i --privileged -v /home/runner/.local/share/docker -e MODE="$mode" -e WANT_PLATFORM="$PLATFORM" $bk_env \
			-e MARKER="smoke-marker-$$-$(date +%s)" "$@" \
			--entrypoint sh "$TAG" -euc '
			mkdir -p /tmp/fake-runner
			cat >/tmp/fake-runner/run.sh
			chmod 755 /tmp/fake-runner/run.sh
			echo smoke >/tmp/jit
			RUNNER_DIR=/tmp/fake-runner LUXGHA_JITCONFIG_FILE=/tmp/jit exec lux-gha-runner
		' <<'EOF'
#!/bin/sh
set -eu
img=moby/buildkit:buildx-stable-1
pinned=moby/buildkit:$BK_VERSION
echo "rootless=$(docker info --format "{{.SecurityOptions}}" | grep -o rootless) driver=$(docker info --format "{{.Driver}}")"
# With the containerd image store an image's ID is the digest of what was
# pulled or loaded: here the multi-arch index.
for ref in "$img" "$pinned"; do
	got=$(docker image inspect "$ref" --format "{{.Id}}") || { echo "$ref is not in the store" >&2; exit 1; }
	[ "$got" = "$BK_INDEX" ] || { echo "$ref is $got, pinned index $BK_INDEX" >&2; exit 1; }
done
got=$(docker image inspect "$img" --format "{{.Os}}/{{.Architecture}}")
[ "$got" = "$WANT_PLATFORM" ] || { echo "$img is $got, want $WANT_PLATFORM" >&2; exit 1; }
got=$(docker image inspect --platform "$WANT_PLATFORM" "$img" --format "{{.Descriptor.Digest}}")
[ "$got" = "$BK_MANIFEST" ] || { echo "$img $WANT_PLATFORM manifest is $got, pinned $BK_MANIFEST" >&2; exit 1; }
echo "store: $img and $pinned are $BK_INDEX; $WANT_PLATFORM manifest $BK_MANIFEST"

case $MODE in
offline)
	# buildx always asks dockerd to pull the builder image first and falls
	# back to a local one when that fails. With no route out, a builder that
	# comes up ran the preloaded image.
	if timeout 20 docker pull -q "$img" >/dev/null 2>&1; then echo "docker pull succeeded: not offline" >&2; exit 1; fi
	pin=
	;;
online)
	# The same pull buildx makes on create, run first so that the store
	# holds only what the entrypoint loaded: it resolves the tag at Docker
	# Hub and must find every blob it names already there.
	docker pull "$pinned" >/tmp/pull.log 2>&1 || { cat /tmp/pull.log; exit 1; }
	if ! grep -q "Image is up to date" /tmp/pull.log || grep -Eq "Downloading|Download complete|Pull complete" /tmp/pull.log; then
		cat /tmp/pull.log
		echo "docker pull $pinned fetched content" >&2
		exit 1
	fi
	echo "online: docker pull $pinned is up to date, nothing downloaded"
	pin=$pinned
	;;
esac
start=$(date +%s)
docker buildx create --name smoke --driver docker-container --driver-opt network=host \
	${pin:+--driver-opt "image=$pin"} --bootstrap >/tmp/create.log 2>&1 || { cat /tmp/create.log; exit 1; }
got=$(docker container inspect buildx_buildkit_smoke0 --format "{{.Image}}")
[ "$got" = "$BK_INDEX" ] || { echo "builder container runs $got, want $BK_INDEX" >&2; exit 1; }
echo "$MODE: docker-container builder (network=host, image ${pin:-$img by default}) bootstrapped from the preloaded image in $(($(date +%s) - start))s"
[ "$MODE" = online ] || exit 0
mkdir -p /tmp/b
printf "FROM busybox:1.37\nARG MARKER\nRUN printf %%s \"\$MARKER\" > /marker\nCMD [\"cat\", \"/marker\"]\n" >/tmp/b/Dockerfile
docker buildx build --builder smoke --load --build-arg MARKER="$MARKER" -t smoke-marker /tmp/b >/tmp/build.log 2>&1 ||
	{ cat /tmp/build.log; exit 1; }
got=$(docker run --rm smoke-marker)
[ "$got" = "$MARKER" ] || { echo "built image printed \"$got\", want \"$MARKER\"" >&2; exit 1; }
echo "docker-container builder: built, loaded and ran smoke-marker; marker matches"
# A server bound only to the runner's loopback, reached from a container via
# host-gateway, as an SSM port-forward relay is.
mkdir -p /tmp/lo && printf %s "$MARKER" >/tmp/lo/marker
python3 -m http.server 18080 --bind 127.0.0.1 --directory /tmp/lo >/tmp/lo.log 2>&1 &
i=0; until python3 -c "import urllib.request as u; u.urlopen(\"http://127.0.0.1:18080/marker\")" 2>/dev/null; do
	i=$((i + 1)); [ "$i" -lt 20 ] || { cat /tmp/lo.log; exit 1; }; sleep 0.5
done
got=$(docker run --rm --add-host host.docker.internal:host-gateway busybox:1.37 wget -T 5 -qO- http://host.docker.internal:18080/marker) ||
	{ echo "a container could not reach the runner's 127.0.0.1:18080 via host-gateway" >&2; exit 1; }
[ "$got" = "$MARKER" ] || { echo "host-gateway returned \"$got\", want \"$MARKER\"" >&2; exit 1; }
echo "host-gateway: a container reached a server bound to the runner's 127.0.0.1"
EOF
	}

	echo "== APPROXIMATION (--privileged, not the Lux nested contract): entrypoint starts rootless dockerd and preloads BuildKit; docker-container builder with no network"
	out=$(privileged_entrypoint offline --network none 2>&1) || { echo "$out"; fail "offline BuildKit checks"; }
	echo "$out"
	echo "$out" | grep -q "^lux-gha-runner: preloaded moby/buildkit:buildx-stable-1 moby/buildkit:$bk_version in [0-9]*s$" ||
		fail "entrypoint did not preload moby/buildkit:buildx-stable-1 and :$bk_version"
	echo "== APPROXIMATION (--privileged): preloaded BuildKit, docker-container builder and a build"
	privileged_entrypoint online || fail "online BuildKit checks"
fi
echo "smoke: OK"
