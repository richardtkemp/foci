VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS = -X main.version=$(VERSION) \
          -X main.gitCommit=$(GIT_COMMIT) \
          -X main.buildTime=$(BUILD_TIME)

.PHONY: build cli all clean test

all: build cli

build:
	go build -ldflags "$(LDFLAGS)" -o clod .

cli:
	go build -ldflags "$(LDFLAGS)" -o clod-cli ./cmd/clod-cli

test:
	go test ./...

clean:
	rm -f clod clod-cli

vet:
	go vet ./...
