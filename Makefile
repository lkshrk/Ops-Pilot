BINARY ?= ops-pilot
ARGS ?= --help
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell git show -s --format=%cI HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

.PHONY: build test verify run

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/ops-pilot

test:
	go test ./...

verify:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go tool staticcheck ./...
	go tool govulncheck ./...
	go test -race -covermode=atomic -coverprofile=coverage.out ./...
	scripts/check-workflow-pins.sh
	scripts/test-check-workflow-pins.sh
	scripts/test-check-release-candidate.sh
	scripts/test-build-release-candidate.sh
	scripts/test-smoke-release-candidate.sh
	scripts/test-check-publish-state.sh

run:
	go run -ldflags="$(LDFLAGS)" ./cmd/ops-pilot $(ARGS)
