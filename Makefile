VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

GOBIN ?= $(shell go env GOPATH)/bin

LDFLAGS = -X main.version=$(VERSION) \
          -X main.gitCommit=$(GIT_COMMIT) \
          -X main.buildTime=$(BUILD_TIME)

.PHONY: all build cli foci-call test vet lint check clean

all: build cli foci-call

build:
	go build -ldflags "$(LDFLAGS)" -o foci-gw .

cli:
	go build -ldflags "$(LDFLAGS)" -o foci ./cmd/foci

foci-call:
	go build -o foci-call ./cmd/foci-call

test:
	go test ./...

clean:
	rm -f foci-gw foci foci-call

vet:
	go vet ./...

lint: vet
	@echo "=== errcheck (production only) ==="
	@$(GOBIN)/errcheck ./... 2>&1 | grep -v _test.go | tee /dev/stderr | { ! grep -q .; }
	@echo "=== gocyclo (>100) ==="
	@$(GOBIN)/gocyclo -over 100 . || true
	@echo "=== gocognit (>100) ==="
	@$(GOBIN)/gocognit -over 100 . || true

check: test lint
