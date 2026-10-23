.PHONY: check lint fmt vet license test entrypoint-test image dist

# The runner image's pinned base, read from its Containerfile.
RUNNER_BASE := $(shell sed -n 's/^ARG UBUNTU_IMAGE=//p' images/runner/Containerfile)
IMAGE_TAG ?= lux-gha-runner:dev

check: lint test

lint: fmt vet license

fmt:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

vet:
	go vet ./...

license:
	@sh scripts/check-license.sh

test:
	go test -race -count=1 ./...

# entrypoint_test.sh against the working tree's entrypoint.sh, as uid 1000 in
# the runner's base image plus the jq and curl the runner image installs: the
# invocation smoke.sh uses inside the built image.
ENTRYPOINT_TEST_IMAGE := lux-gha-entrypoint-test:local
entrypoint-test:
	printf 'FROM %s\nRUN apt-get update && apt-get install -y --no-install-recommends jq curl ca-certificates && rm -rf /var/lib/apt/lists/*\n' \
		'$(RUNNER_BASE)' | docker build -q -t $(ENTRYPOINT_TEST_IMAGE) - >/dev/null
	docker run --rm -i --workdir /home/ubuntu --user 1000:1000 -v "$(CURDIR)/images/runner:/src:ro" --entrypoint sh \
		$(ENTRYPOINT_TEST_IMAGE) -s /src/entrypoint.sh <images/runner/entrypoint_test.sh
	docker run --rm -i --workdir /home/ubuntu --user 0:0 -v "$(CURDIR)/images/runner:/src:ro" --entrypoint sh \
		$(ENTRYPOINT_TEST_IMAGE) -s /src/entrypoint.sh sigterm-root <images/runner/entrypoint_test.sh

# The runner image, loaded into the local Docker: the host's architecture, or
# PLATFORM=linux/arm64|linux/amd64.
image:
	docker buildx build $(if $(PLATFORM),--platform $(PLATFORM)) --load -t $(IMAGE_TAG) -f images/runner/Containerfile images/runner

# Release tarballs and SHA256SUMS in dist/ (README "Releases"). VERSION reaches
# dist.sh through the environment Make exports, never through the recipe line:
# a tag name may contain shell metacharacters.
dist:
	./scripts/dist.sh
