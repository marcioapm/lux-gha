.PHONY: check fmt license test vet

check: fmt vet license test

fmt:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

vet:
	go vet ./...

license:
	@sh scripts/check-license.sh

test:
	go test -race ./...
