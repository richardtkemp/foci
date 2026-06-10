VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

GOBIN ?= $(shell go env GOPATH)/bin
NPROC := $(shell nproc 2>/dev/null || echo 4)

LDFLAGS = -s -w -X main.version=$(VERSION) \
          -X main.gitCommit=$(GIT_COMMIT) \
          -X main.buildTime=$(BUILD_TIME)

.PHONY: all build cli foci-call foci-cc-hook find-disconnected-tests test integration coverage coverage-report coverage-html coverage-check vet lint lint-fix lint-dupl lint-deadcode verify-persistence check clean setup-hooks

all: build cli foci-call foci-cc-hook find-disconnected-tests

BUILDVCS := $(shell git rev-parse --git-dir >/dev/null 2>&1 && echo true || echo false)

build:
	@mkdir -p bin
	go build -buildvcs=$(BUILDVCS) -ldflags "$(LDFLAGS)" -o bin/foci-gw ./cmd/foci-gw
	@command -v upx >/dev/null 2>&1 && upx -q bin/foci-gw || true

cli:
	@mkdir -p bin
	go build -buildvcs=$(BUILDVCS) -ldflags "$(LDFLAGS)" -o bin/foci ./cmd/foci

foci-call:
	@mkdir -p bin
	go build -buildvcs=$(BUILDVCS) -ldflags "$(LDFLAGS)" -o bin/foci-call ./cmd/foci-call

foci-cc-hook:
	@mkdir -p bin
	go build -buildvcs=$(BUILDVCS) -ldflags "$(LDFLAGS)" -o bin/foci-cc-hook ./cmd/foci-cc-hook

# find-disconnected-tests is the test-side counterpart to deadcode: it
# reports Test* functions whose bodies do not (transitively, via test
# helpers in the same package) reach any identifier defined in the
# package under test. Lives in its own go.mod under scripts/ so x/tools
# stays out of foci's main module.
find-disconnected-tests:
	@mkdir -p bin
	cd scripts/find-disconnected-tests && go build -o ../../bin/find-disconnected-tests .

test:
	$(eval TESTDIR := /tmp/foci/test-$(shell date +%s))
	@mkdir -p $(TESTDIR)
	TMPDIR=$(TESTDIR) go test -p=$(NPROC) -parallel=16 ./... ; STATUS=$$? ; rm -rf $(TESTDIR) ; exit $$STATUS

# Integration tests (L2): real foci-gw subprocess against stubbed CC and
# stubbed Telegram. Build-tagged so they only run under this target — not
# as part of `make test`. See test/integration/README.md for the
# architecture, and internal/testharness/ for the scaffolding.
integration:
	@echo "=== Integration tests (L2: real foci-gw against stubbed edges) ==="
	$(eval TESTDIR := /tmp/foci/integration-$(shell date +%s))
	@mkdir -p $(TESTDIR)
	@TMPDIR=$(TESTDIR) go test -tags=integration -count=1 -timeout 300s ./test/integration/... ./internal/testharness/... ; STATUS=$$? ; rm -rf $(TESTDIR) ; exit $$STATUS

coverage:
	$(eval TESTDIR := /tmp/foci/test-$(shell date +%s))
	@mkdir -p $(TESTDIR)
	@echo "=== Test Coverage ==="
	@TMPDIR=$(TESTDIR) go test -p=$(NPROC) -parallel=16 -cover ./... 2>&1 | grep -E '(coverage:|FAIL|PASS)' ; STATUS=$$? ; rm -rf $(TESTDIR) ; exit $$STATUS

coverage-report:
	$(eval TESTDIR := /tmp/foci/test-$(shell date +%s))
	@mkdir -p $(TESTDIR)
	@echo "=== Generating Coverage Report ==="
	@TMPDIR=$(TESTDIR) go test -p=$(NPROC) -parallel=16 -coverprofile=coverage.out ./...
	@rm -rf $(TESTDIR)
	@go tool cover -func=coverage.out | tail -20
	@echo ""
	@echo "Total coverage:"
	@go tool cover -func=coverage.out | grep total | awk '{print $$3}'

coverage-html:
	$(eval TESTDIR := /tmp/foci/test-$(shell date +%s))
	@mkdir -p $(TESTDIR)
	@echo "=== Generating HTML Coverage Report ==="
	@TMPDIR=$(TESTDIR) go test -p=$(NPROC) -parallel=16 -coverprofile=coverage.out ./...
	@rm -rf $(TESTDIR)
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report saved to coverage.html"

# Enforce minimum coverage thresholds (excludes cmd packages)
COVERAGE_TOTAL_MIN ?= 75.0
COVERAGE_PKG_MIN ?= 45.0

coverage-check:
	$(eval TESTDIR := /tmp/foci/test-$(shell date +%s))
	@mkdir -p $(TESTDIR)
	@echo "=== Testing with Coverage (total>=$(COVERAGE_TOTAL_MIN)%, per-package>=$(COVERAGE_PKG_MIN)%) ==="
	@TMPDIR=$(TESTDIR) go test -p=$(NPROC) -parallel=16 -cover -coverprofile=coverage.out ./internal/... ./prompts/... 2>&1 | tee .test-output.tmp
	@rm -rf $(TESTDIR)
	@if grep -q '^FAIL' .test-output.tmp; then \
		rm -f .test-output.tmp; \
		exit 1; \
	fi
	@TOTAL=$$(go tool cover -func=coverage.out | grep total | awk '{print $$3}' | sed 's/%//'); \
	echo ""; \
	echo "Total coverage: $$TOTAL%"; \
	FAILED=0; \
	if [ "$$(echo "$$TOTAL < $(COVERAGE_TOTAL_MIN)" | bc -l)" -eq 1 ]; then \
		echo "❌ Total coverage $$TOTAL% is below $(COVERAGE_TOTAL_MIN)%"; \
		FAILED=1; \
	else \
		echo "✅ Total coverage $$TOTAL% meets $(COVERAGE_TOTAL_MIN)%"; \
	fi; \
	echo ""; \
	echo "Per-package coverage (internal only):"; \
	grep "^ok" .test-output.tmp | grep "foci/internal/" | while read -r line; do \
		PKG=$$(echo "$$line" | awk '{print $$2}'); \
		COV=$$(echo "$$line" | grep -oP 'coverage: \K[0-9.]+' || echo "0"); \
		if [ "$$(echo "$$COV < $(COVERAGE_PKG_MIN)" | bc -l)" -eq 1 ]; then \
			echo "  ❌ $$PKG: $$COV% (below $(COVERAGE_PKG_MIN)%)"; \
			FAILED=1; \
		else \
			echo "  ✅ $$PKG: $$COV%"; \
		fi; \
	done; \
	rm -f .test-output.tmp; \
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

lint: find-disconnected-tests
	@echo "=== golangci-lint ==="
	@$(GOBIN)/golangci-lint run
	@echo "=== deadcode (whole-program reachability, app code only) ==="
	@# internal/testharness is test-only scaffolding: it is reachable solely
	@# from -tags=integration tests, which deadcode ./... does not compile, so it
	@# always appears unreachable. Exclude it to keep this gate on app code only.
	@output=$$($(GOBIN)/deadcode ./... | grep -v '/testharness/'); \
	if [ -n "$$output" ]; then echo "$$output"; exit 1; fi
	@echo "=== find-disconnected-tests (Test* functions that don't touch prod) ==="
	@./bin/find-disconnected-tests ./...

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

verify-persistence:
	@echo "=== CodeQL Persistence Verification ==="
	@./scripts/verify-persistence.sh

check: lint coverage-check verify-persistence
