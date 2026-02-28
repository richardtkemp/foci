VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS = -X main.version=$(VERSION) \
          -X main.gitCommit=$(GIT_COMMIT) \
          -X main.buildTime=$(BUILD_TIME)

.PHONY: all build cli foci-call test vet clean

all: build cli foci-call

build:
	go build -ldflags "$(LDFLAGS)" -o focigw .

cli:
	go build -ldflags "$(LDFLAGS)" -o foci ./cmd/foci

foci-call:
	go build -o foci-call ./cmd/foci-call

test:
	go test ./...

clean:
	rm -f focigw foci foci-call

vet:
	go vet ./...
