VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

GOBIN ?= $(shell go env GOPATH)/bin

LDFLAGS = -X main.version=$(VERSION) \
          -X main.gitCommit=$(GIT_COMMIT) \
          -X main.buildTime=$(BUILD_TIME)

.PHONY: all build cli foci-call test coverage coverage-report coverage-html coverage-check vet lint lint-fix lint-dupl check clean

all: build cli foci-call

build:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/foci-gw ./cmd/foci-gw

cli:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/foci ./cmd/foci

foci-call:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/foci-call ./cmd/foci-call

test:
	go test -p=$(shell nproc 2>/dev/null || echo 4) ./...

coverage:
	@echo "=== Test Coverage ==="
	@go test -p=$(shell nproc 2>/dev/null || echo 4) -cover ./... 2>&1 | grep -E '(coverage:|FAIL|PASS)'

coverage-report:
	@echo "=== Generating Coverage Report ==="
	@go test -p=$(shell nproc 2>/dev/null || echo 4) -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -20
	@echo ""
	@echo "Total coverage:"
	@go tool cover -func=coverage.out | grep total | awk '{print $$3}'

coverage-html:
	@echo "=== Generating HTML Coverage Report ==="
	@go test -p=$(shell nproc 2>/dev/null || echo 4) -coverprofile=coverage.out ./...
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report saved to coverage.html"

# Enforce minimum coverage threshold (default: 70%)
COVERAGE_THRESHOLD ?= 70.0

coverage-check:
	@echo "=== Checking Coverage Threshold ($(COVERAGE_THRESHOLD)%) ==="
	@go test -p=$(shell nproc 2>/dev/null || echo 4) -coverprofile=coverage.out ./... > /dev/null 2>&1 || true
	@TOTAL=$$(go tool cover -func=coverage.out | grep total | awk '{print $$3}' | sed 's/%//'); \
	echo "Total coverage: $$TOTAL%"; \
	if [ "$$(echo "$$TOTAL < $(COVERAGE_THRESHOLD)" | bc -l)" -eq 1 ]; then \
		echo "❌ Coverage $$TOTAL% is below threshold $(COVERAGE_THRESHOLD)%"; \
		exit 1; \
	else \
		echo "✅ Coverage $$TOTAL% meets threshold $(COVERAGE_THRESHOLD)%"; \
	fi

clean:
	rm -rf bin

vet:
	go vet ./...

lint:
	@echo "=== golangci-lint ==="
	@$(GOBIN)/golangci-lint run

lint-fix:
	@echo "=== golangci-lint --fix ==="
	@$(GOBIN)/golangci-lint run --fix

# Run specific linter
lint-dupl:
	@$(GOBIN)/golangci-lint run --disable-all -E dupl

# Legacy complexity check (now included in lint)
complex: vet
	@echo "=== gocyclo (>75) ==="
	@$(GOBIN)/gocyclo -over 75 . || true
	@echo "=== gocognit (>100) ==="
	@$(GOBIN)/gocognit -over 100 . || true

check: test lint
