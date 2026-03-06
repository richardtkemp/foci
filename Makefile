VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

GOBIN ?= $(shell go env GOPATH)/bin

LDFLAGS = -s -w -X main.version=$(VERSION) \
          -X main.gitCommit=$(GIT_COMMIT) \
          -X main.buildTime=$(BUILD_TIME)

.PHONY: all build cli foci-call test coverage coverage-report coverage-html coverage-check vet lint lint-fix lint-dupl check clean setup-hooks

all: build cli foci-call

build:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/foci-gw ./cmd/foci-gw
	@command -v upx >/dev/null 2>&1 && upx -q bin/foci-gw || true

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

# Enforce minimum coverage thresholds (excludes cmd packages)
COVERAGE_TOTAL_MIN ?= 75.0
COVERAGE_PKG_MIN ?= 45.0

coverage-check:
	@echo "=== Checking Coverage (total>=$(COVERAGE_TOTAL_MIN)%, per-package>=$(COVERAGE_PKG_MIN)%) [internal only] ==="
	@go test -p=$(shell nproc 2>/dev/null || echo 4) -coverprofile=coverage.out ./internal/... > /dev/null 2>&1 || true
	@TOTAL=$$(go tool cover -func=coverage.out | grep total | awk '{print $$3}' | sed 's/%//'); \
	echo "Total coverage: $$TOTAL%"; \
	FAILED=0; \
	if [ "$$(echo "$$TOTAL < $(COVERAGE_TOTAL_MIN)" | bc -l)" -eq 1 ]; then \
		echo "❌ Total coverage $$TOTAL% is below $(COVERAGE_TOTAL_MIN)%"; \
		FAILED=1; \
	else \
		echo "✅ Total coverage $$TOTAL% meets $(COVERAGE_TOTAL_MIN)%"; \
	fi; \
	echo ""; \
	echo "Per-package coverage:"; \
	go test -p=$(shell nproc 2>/dev/null || echo 4) -cover ./internal/... 2>&1 | grep "^ok" | while read -r line; do \
		PKG=$$(echo "$$line" | awk '{print $$2}'); \
		COV=$$(echo "$$line" | grep -oP 'coverage: \K[0-9.]+' || echo "0"); \
		if [ "$$(echo "$$COV < $(COVERAGE_PKG_MIN)" | bc -l)" -eq 1 ]; then \
			echo "  ❌ $$PKG: $$COV% (below $(COVERAGE_PKG_MIN)%)"; \
			FAILED=1; \
		else \
			echo "  ✅ $$PKG: $$COV%"; \
		fi; \
	done; \
	if [ "$$FAILED" -eq 1 ]; then \
		exit 1; \
	fi

clean:
	rm -rf bin

setup-hooks:
	@echo "=== Setting up Git hooks ==="
	@git config core.hooksPath .githooks
	@echo "✅ Git hooks configured to use .githooks/"

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

check: test lint coverage-check
