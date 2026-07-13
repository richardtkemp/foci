VERSION ?= $(shell git -c safe.directory=$(CURDIR) describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git -c safe.directory=$(CURDIR) rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

GOBIN ?= $(shell go env GOPATH)/bin
NPROC := $(shell nproc 2>/dev/null || echo 4)
# -parallel ceiling for L2 integration tests. Set well above the real
# governor — the weighted budget in internal/testharness/parallel.go
# (loadPerCPU * NumCPU) — so the flag never binds before the semaphore.
# Scales with the host; the semaphore caps actual concurrency.
IPARALLEL := $(shell expr $(NPROC) \* 8)

LDFLAGS = -s -w -X main.version=$(VERSION) \
          -X main.gitCommit=$(GIT_COMMIT) \
          -X main.buildTime=$(BUILD_TIME)

# CI test-history hook: CI_HOOK is the path to the shared results CSV
# (time,project,commit,target,result). When set it enables per-run recording in
# the test/integration targets below; unset (e.g. a fresh clone) records nothing.
# Lives in a machine-local .ci.mk (gitignored), inherited by every worktree via
# git-common-dir so all local runs record into one central store.
-include $(shell git rev-parse --git-common-dir 2>/dev/null)/../.ci.mk
-include .ci.mk

# Opt-in remote build offload to a Mac (see the remote-build target). REMOTE_HOST
# + REMOTE_DIR come from a gitignored .remote.mk, inherited by worktrees via
# git-common-dir. Empty by default → remote-build errors; nothing else is affected.
-include $(shell git rev-parse --git-common-dir 2>/dev/null)/../.remote.mk
-include .remote.mk

.PHONY: all build cli foci-call foci-cc-hook find-disconnected-tests find-static-config-reads test integration coverage coverage-report coverage-html coverage-check vet lint lint-fix lint-dupl lint-deadcode lint-static-config verify-persistence check clean setup-hooks

all: build cli foci-call foci-cc-hook find-disconnected-tests find-static-config-reads

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

# find-static-config-reads flags every read of the static, non-live
# *config.ResolvedAgentConfig snapshot (a `resolved`/`Resolved`-named
# field) instead of a config.LiveValue's Load()/LiveConfig(). Discovery
# tool for the ongoing bucket A-D live-config migration; lives in its own
# go.mod for the same reason as find-disconnected-tests.
find-static-config-reads:
	@mkdir -p bin
	cd scripts/find-static-config-reads && go build -o ../../bin/find-static-config-reads .

test:
	$(eval TESTDIR := /tmp/fgw/test-$(shell date +%s))
	$(eval LOGFILE := $(TESTDIR).log)
	@mkdir -p $(TESTDIR)
	@# /tmp/heavy serialises the test runner against any other heavy build
	@# (e.g. a concurrent `update.sh` deploy build) that holds the same lock,
	@# so they don't starve each other for CPU and trip deadline-sensitive
	@# waits. Other heavy builds should flock the same path reciprocally.
	@# Open the lock READ-ONLY (9<): /tmp is world-writable + sticky, and with
	@# fs.protected_regular=2 the kernel denies WRITE-opening a lock file there
	@# owned by the other shared account (rich vs foci). A read-only fd is exempt,
	@# and an exclusive flock on it is still mutually exclusive across users; the
	@# `|| : >` seeds the file only when missing (creator owns it, umask 0002 keeps
	@# it readable by all).
	@# The lock is held on FD 9 by THIS subshell only: `9<&-` closes it for the
	@# go test command, so go test and its spawned subprocesses (foci-gw,
	@# cc-stub) do NOT inherit the lock — a lingering child must not keep the
	@# lock held after the runner exits, or the next run would block forever.
	@# Full output → $(LOGFILE) on disk; stdout gets only the failure summary +
	@# a file ref (see the `integration` target for the rationale). Output is
	@# retained (no cleanup) under /tmp/fgw — a daily cron sweeps entries >24h.
	@[ -e /tmp/heavy ] || : > /tmp/heavy
	@( echo ">>> waiting for heavy lock (/tmp/heavy; another build may be running) ..." >&2; flock 9; echo ">>> acquired heavy lock" >&2; TMPDIR=$(TESTDIR) FOCI_TMPDIR=$(TESTDIR) FOCI_TEST_TMPDIR=$(TESTDIR) nice -n 19 go test -p=$(NPROC) -parallel=16 ./... > $(LOGFILE) 2>&1 9<&- ; STATUS=$$? ; \
	  if [ $$STATUS -eq 0 ]; then echo "PASS — full log: $(LOGFILE)"; \
	  else echo "FAILED — full log: $(LOGFILE)"; echo "--- failures ---"; grep -E '^(--- FAIL:|FAIL)|panic:|weight audit' $(LOGFILE) || true; fi ; \
	  if [ -n "$(CI_HOOK)" ]; then mkdir -p "$$(dirname "$(CI_HOOK)")" && printf '%s,foci,%s,unit,%s\n' "$$(date -Is)" "$(GIT_COMMIT)" "$$([ $$STATUS -eq 0 ] && echo pass || echo fail)" >> "$(CI_HOOK)" || true; fi ; \
	  exit $$STATUS ) 9</tmp/heavy

# Integration tests (L2): real foci-gw subprocess against stubbed CC and
# stubbed Telegram. Build-tagged so they only run under this target — not
# as part of `make test`. See test/integration/README.md for the
# architecture, and internal/testharness/ for the scaffolding.
integration:
	@echo "=== Integration tests (L2: real foci-gw against stubbed edges) ==="
	$(eval TESTDIR := /tmp/fgw/integration-$(shell date +%s))
	$(eval LOGFILE := $(TESTDIR).log)
	@mkdir -p $(TESTDIR)
	@# Full -v output (every RUN/PASS line + on-failure gateway stderr dumps) is
	@# LARGE — write it to $(LOGFILE) on disk and print only the failure summary
	@# + a file ref on stdout, so a caller (incl. an agent piping this into
	@# context) never ingests the whole transcript. Open $(LOGFILE) to review a
	@# run carefully. Both $(TESTDIR) and $(LOGFILE) are RETAINED (no cleanup) so
	@# a run's artifacts survive for inspection; they live under /tmp/fgw, which a
	@# daily cron sweeps for entries >24h.
	@# Read-only lock fd (9<) — see the `test` target above for why (fs.protected_regular).
	@[ -e /tmp/heavy ] || : > /tmp/heavy
	@( echo ">>> waiting for heavy lock (/tmp/heavy; another build may be running) ..." >&2; flock 9; echo ">>> acquired heavy lock" >&2; TMPDIR=$(TESTDIR) FOCI_TMPDIR=$(TESTDIR) FOCI_TEST_TMPDIR=$(TESTDIR) nice -n 19 go test -tags=integration -count=1 -timeout 480s -parallel=$(IPARALLEL) -v ./test/integration/... ./internal/testharness/... > $(LOGFILE) 2>&1 9<&- ; STATUS=$$? ; \
	  if [ $$STATUS -eq 0 ]; then echo "PASS — full log: $(LOGFILE)"; \
	  else echo "FAILED — full log: $(LOGFILE)"; echo "--- failures ---"; grep -E '^(--- FAIL:|FAIL)|panic:|weight audit' $(LOGFILE) || true; fi ; \
	  if [ -n "$(CI_HOOK)" ]; then mkdir -p "$$(dirname "$(CI_HOOK)")" && printf '%s,foci,%s,integration,%s\n' "$$(date -Is)" "$(GIT_COMMIT)" "$$([ $$STATUS -eq 0 ] && echo pass || echo fail)" >> "$(CI_HOOK)" || true; fi ; \
	  exit $$STATUS ) 9</tmp/heavy

# bucket-audit: the differential half of weight-bucket detection. Runs the
# L2 suite at low (-parallel=2) and high (-parallel=IPARALLEL) concurrency
# and surfaces, from each pass, the tests that FAILED plus the in-run weight
# auditor's advisories. A wait-weighted test that is green-and-flat at low
# but fails/inflates at high is contention-sensitive and wants a heavier
# weight; that is the signal a single run can't see. Advisory tool, not a
# gate — read the two passes side by side.
bucket-audit:
	@echo "=== bucket-audit: low vs high parallelism ==="
	$(eval TESTDIR := /tmp/foci/bktaudit-$(shell date +%s))
	@mkdir -p $(TESTDIR)
	@echo "--- low (-parallel=2) ---"
	-@TMPDIR=$(TESTDIR) FOCI_TMPDIR=$(TESTDIR) FOCI_TEST_TMPDIR=$(TESTDIR) nice -n 19 go test -tags=integration -count=1 -timeout 900s -parallel=2 -v ./test/integration/... 2>&1 | grep -E '^(--- FAIL|    .*weight audit)' || echo "  (clean)"
	@echo "--- high (-parallel=$(IPARALLEL)) ---"
	-@TMPDIR=$(TESTDIR) FOCI_TMPDIR=$(TESTDIR) FOCI_TEST_TMPDIR=$(TESTDIR) nice -n 19 go test -tags=integration -count=1 -timeout 480s -parallel=$(IPARALLEL) -v ./test/integration/... 2>&1 | grep -E '^(--- FAIL|    .*weight audit)' || echo "  (clean)"
	@rm -rf $(TESTDIR)

coverage:
	$(eval TESTDIR := /tmp/foci/test-$(shell date +%s))
	@mkdir -p $(TESTDIR)
	@echo "=== Test Coverage ==="
	@TMPDIR=$(TESTDIR) FOCI_TMPDIR=$(TESTDIR) FOCI_TEST_TMPDIR=$(TESTDIR) nice -n 19 go test -p=$(NPROC) -parallel=16 -cover ./... 2>&1 | grep -E '(coverage:|FAIL|PASS)' ; STATUS=$$? ; rm -rf $(TESTDIR) ; exit $$STATUS

coverage-report:
	$(eval TESTDIR := /tmp/foci/test-$(shell date +%s))
	@mkdir -p $(TESTDIR)
	@echo "=== Generating Coverage Report ==="
	@TMPDIR=$(TESTDIR) FOCI_TMPDIR=$(TESTDIR) FOCI_TEST_TMPDIR=$(TESTDIR) nice -n 19 go test -p=$(NPROC) -parallel=16 -coverprofile=coverage.out ./...
	@rm -rf $(TESTDIR)
	@go tool cover -func=coverage.out | tail -20
	@echo ""
	@echo "Total coverage:"
	@go tool cover -func=coverage.out | grep total | awk '{print $$3}'

coverage-html:
	$(eval TESTDIR := /tmp/foci/test-$(shell date +%s))
	@mkdir -p $(TESTDIR)
	@echo "=== Generating HTML Coverage Report ==="
	@TMPDIR=$(TESTDIR) FOCI_TMPDIR=$(TESTDIR) FOCI_TEST_TMPDIR=$(TESTDIR) nice -n 19 go test -p=$(NPROC) -parallel=16 -coverprofile=coverage.out ./...
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
	@TMPDIR=$(TESTDIR) FOCI_TMPDIR=$(TESTDIR) FOCI_TEST_TMPDIR=$(TESTDIR) nice -n 19 go test -p=$(NPROC) -parallel=16 -cover -coverprofile=coverage.out ./internal/... ./shared/... 2>&1 | tee .test-output.tmp
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

lint: find-disconnected-tests find-static-config-reads
	@echo "=== golangci-lint ==="
	@$(GOBIN)/golangci-lint run
	@echo "=== find-static-config-reads (static reads of *config.ResolvedAgentConfig) ==="
	@./bin/find-static-config-reads ./...
	@echo "=== deadcode (whole-program reachability, app code only) ==="
	@# internal/testharness and internal/testtemp are test-only scaffolding:
	# reachable solely from -tags=integration tests and _test.go files, which
	# deadcode ./... does not compile, so they always appear unreachable.
	@# Exclude them to keep this gate on app code only.
	@raw=$$($(GOBIN)/deadcode ./...) || { echo "deadcode failed or was killed (exit $$?) — reachability gate did NOT run"; exit 1; }; \
	output=$$(printf '%s' "$$raw" | grep -v -e '/testharness/' -e '/testtemp/' || true); \
	if [ -n "$$output" ]; then echo "$$output"; exit 1; fi
	@echo "=== find-disconnected-tests (Test* functions that don't touch prod) ==="
	@./bin/find-disconnected-tests ./...
	@echo "=== integration parallel-bucket gate (no bare t.Parallel) ==="
	@# Every L2 test must declare a concurrency weight via testharness.Parallel*
	@# (ParallelWait/ParallelHeavy/ParallelWeight) so it is throttled by the
	@# weighted budget and covered by the bucket auditor. A bare t.Parallel()
	@# runs unthrottled and silently escapes both — forbid it here since
	@# forbidigo is disabled for _test.go files (see .golangci.yml), so
	@# enforce this here via grep instead.
	@bad=$$(grep -rnE '^[[:space:]]*t\.Parallel\(\)' test/integration/ || true); \
	if [ -n "$$bad" ]; then \
		echo "bare t.Parallel() in L2 tests — use testharness.ParallelWait/ParallelHeavy/ParallelWeight:"; \
		echo "$$bad"; exit 1; \
	fi

lint-fix:
	@echo "=== golangci-lint --fix ==="
	@$(GOBIN)/golangci-lint run --fix

# Run specific linter
lint-dupl:
	@$(GOBIN)/golangci-lint run --disable-all -E dupl

# Also runs as part of `lint` — this target is for a quick standalone check.
lint-static-config: find-static-config-reads
	@./bin/find-static-config-reads ./...

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

# ============================================================
# Deploy — single source of truth (replaces setup.sh / update.sh)
# ============================================================
# Privileged targets run under `sudo make <target>`; aisudo gates the
# escalation, so the old emit-for-review model is redundant. The systemd unit
# is rendered from deploy/foci.service.tmpl — the ONE place it is defined.
#
#   sudo make -C <repo> update   # deploy new code to an existing install
#   sudo make -C <repo> setup    # first-time provision (usually via download.sh)

FOCI_USER     ?= foci
FOCI_HOME     ?= /home/$(FOCI_USER)
INSTALL_DIR   ?= /usr/local/bin
SECRETS_GROUP ?= foci-secrets
ASKGW_GROUP   ?= foci-askgw
SERVICE_NAME  ?= foci
SERVICE_FILE  := /etc/systemd/system/$(SERVICE_NAME).service
SECRETS_FILE  := $(FOCI_HOME)/config/secrets.toml
DEPLOY_BINS   := foci-gw foci foci-call foci-cc-hook

# Base PATH baked into the unit (shellenv layers the operator dotfile env on top
# at startup): FOCI_HOME/.local/bin plus the standard system dirs that exist.
SERVICE_PATH := $(FOCI_HOME)/.local/bin$(shell for d in /usr/local/sbin /usr/local/bin /usr/sbin /usr/bin /sbin /bin; do [ -d "$$d" ] && printf ':%s' "$$d"; done)

# first-run wizard: interactive by default; non-interactive when a token is set.
WIZARD_ARGS := --config-dir $(FOCI_HOME)/config
ifdef FOCI_TELEGRAM_TOKEN
WIZARD_ARGS += --non-interactive --telegram-bot-token $(FOCI_TELEGRAM_TOKEN) --telegram-user-id $(FOCI_TELEGRAM_USER)
ifdef FOCI_PROVIDER
WIZARD_ARGS += --provider $(FOCI_PROVIDER)
endif
ifdef FOCI_API_KEY
WIZARD_ARGS += --api-key $(FOCI_API_KEY)
endif
endif

.PHONY: deploy-build install-bin install-unit install-polkit provision install-shared install-docs wizard check-config stage-changelog reload restart enable setup update

# go build must not run as root under `sudo make`; drop to the service user
# (mirrors what update.sh did). Real /usr/bin/sudo here — root's PATH has no
# aisudo shim, so no re-prompt.
deploy-build:
	sudo -u $(FOCI_USER) bash -c "cd '$(CURDIR)' && $(MAKE) -s all"

install-bin:
	@for b in $(DEPLOY_BINS); do echo "  install $$b"; install -m 755 bin/$$b $(INSTALL_DIR)/$$b; done

install-unit:
	getent group $(SECRETS_GROUP) >/dev/null 2>&1 || groupadd $(SECRETS_GROUP)
	getent group $(ASKGW_GROUP) >/dev/null 2>&1 || groupadd $(ASKGW_GROUP)
	sed -e 's|@FOCI_USER@|$(FOCI_USER)|g' \
	    -e 's|@SECRETS_GROUP@|$(SECRETS_GROUP)|g' \
	    -e 's|@ASKGW_GROUP@|$(ASKGW_GROUP)|g' \
	    -e 's|@FOCI_HOME@|$(FOCI_HOME)|g' \
	    -e 's|@SERVICE_PATH@|$(SERVICE_PATH)|g' \
	    -e 's|@INSTALL_DIR@|$(INSTALL_DIR)|g' \
	    deploy/foci.service.tmpl > $(SERVICE_FILE)
	systemctl daemon-reload

install-polkit:
	@install -d /etc/polkit-1/rules.d
	@printf '%s\n' \
	  '// Allow $(FOCI_USER) to manage $(SERVICE_NAME).service without a password.' \
	  'polkit.addRule(function(action, subject) {' \
	  '    if (action.id === "org.freedesktop.systemd1.manage-units" &&' \
	  '        action.lookup("unit") === "$(SERVICE_NAME).service" &&' \
	  '        subject.user === "$(FOCI_USER)") {' \
	  '        return polkit.Result.YES;' \
	  '    }' \
	  '});' > /etc/polkit-1/rules.d/49-$(SERVICE_NAME).rules

provision:
	id $(FOCI_USER) >/dev/null 2>&1 || useradd --system --home-dir $(FOCI_HOME) --create-home --shell /bin/bash $(FOCI_USER)
	getent group $(SECRETS_GROUP) >/dev/null 2>&1 || groupadd $(SECRETS_GROUP)
	getent group $(ASKGW_GROUP) >/dev/null 2>&1 || groupadd $(ASKGW_GROUP)
	@if ! getent group crontab >/dev/null 2>&1; then \
	  if   command -v apt-get >/dev/null 2>&1; then DEBIAN_FRONTEND=noninteractive apt-get install -y cron || true; \
	  elif command -v dnf     >/dev/null 2>&1; then dnf install -y cronie || true; \
	  elif command -v pacman  >/dev/null 2>&1; then pacman -S --noconfirm cronie || true; \
	  fi; \
	  getent group crontab >/dev/null 2>&1 || groupadd crontab; \
	fi
	@id -nG $(FOCI_USER) 2>/dev/null | grep -qw $(SECRETS_GROUP) && gpasswd -d $(FOCI_USER) $(SECRETS_GROUP) || true
	mkdir -p $(FOCI_HOME)/config $(FOCI_HOME)/data $(FOCI_HOME)/logs
	chown $(FOCI_USER):$(FOCI_USER) $(FOCI_HOME)/config $(FOCI_HOME)/data $(FOCI_HOME)/logs

install-shared:
	mkdir -p $(FOCI_HOME)/shared/docs
	cp -r shared/* $(FOCI_HOME)/shared/
	cp -r docs/* $(FOCI_HOME)/shared/docs/
	cp README.md $(FOCI_HOME)/shared/docs/README.md
	chown -R $(FOCI_USER):$(FOCI_USER) $(FOCI_HOME)/shared

wizard:
	@runuser -u $(FOCI_USER) -- $(INSTALL_DIR)/foci first-run $(WIZARD_ARGS)
	@[ -f $(SECRETS_FILE) ] && chown root:$(SECRETS_GROUP) $(SECRETS_FILE) && chmod 0660 $(SECRETS_FILE) || true

check-config:
	@for svcfile in /etc/systemd/system/foci*.service; do \
	  [ -f "$$svcfile" ] || continue; \
	  cfg=$$(grep '^ExecStart=' "$$svcfile" | grep -oP '(?<=-config )\S+' || true); \
	  [ -z "$$cfg" ] && continue; \
	  home=$$(grep '^WorkingDirectory=' "$$svcfile" | cut -d= -f2); \
	  printf '  %s: %s ... ' "$$(basename $$svcfile .service)" "$$cfg"; \
	  HOME="$$home" bin/foci-gw -check-config -config "$$cfg" || { echo "ABORT: $$svcfile config incompatible — daemon untouched"; exit 1; }; \
	done

stage-changelog:
	@for svcfile in /etc/systemd/system/foci*.service; do \
	  [ -f "$$svcfile" ] || continue; \
	  home=$$(grep '^WorkingDirectory=' "$$svcfile" | cut -d= -f2); \
	  user=$$(grep '^User=' "$$svcfile" | cut -d= -f2); \
	  [ -n "$$home" ] || continue; \
	  cf="$$home/data/.foci-commit"; old=""; \
	  [ -r "$$cf" ] && old=$$(cat "$$cf" 2>/dev/null || true); \
	  if [ -n "$$old" ] && [ "$$old" != "$(GIT_COMMIT)" ]; then \
	    { echo "# Foci Updated"; echo; echo "Updated from \`$$old\` to \`$(GIT_COMMIT)\` on $$(date -u '+%Y-%m-%d %H:%M UTC')."; echo; echo "## Changes"; echo; \
	      git -C "$(CURDIR)" -c safe.directory="$(CURDIR)" log --format='- **%s**%n%n%w(0,2,2)%b' "$$old..$(GIT_COMMIT)" 2>/dev/null || echo "(could not read git log)"; echo; echo "## Instructions"; echo; echo "Tell your user what just changed. Summarise the updates above in a brief, friendly message."; } > "$$home/data/WELCOME.md"; \
	    chown "$$user:$$user" "$$home/data/WELCOME.md"; \
	    echo "  $$(basename $$svcfile .service): changelog staged ($$old -> $(GIT_COMMIT))"; \
	  fi; \
	  mkdir -p "$$(dirname "$$cf")"; echo "$(GIT_COMMIT)" > "$$cf"; chown "$$user:$$user" "$$cf"; \
	done

install-docs:
	@for svcfile in /etc/systemd/system/foci*.service; do \
	  [ -f "$$svcfile" ] || continue; \
	  home=$$(grep '^WorkingDirectory=' "$$svcfile" | cut -d= -f2); \
	  user=$$(grep '^User=' "$$svcfile" | cut -d= -f2); \
	  [ -n "$$home" ] && [ -n "$$user" ] || continue; \
	  mkdir -p "$$home/shared/docs"; \
	  rsync -a --delete docs/ "$$home/shared/docs/"; \
	  cp README.md "$$home/shared/docs/README.md"; \
	  chown -R "$$user:$$user" "$$home/shared/docs"; \
	done

reload:
	systemctl daemon-reload

restart:
	@for svc in $$(systemctl list-units --type=service --plain --no-legend 'foci*' | awk '{print $$1}'); do \
	  echo "  restarting $$svc"; systemctl restart --no-block "$$svc"; \
	done
	@echo ""
	@echo ">>> AGENT: the foci restart is queued (async, --no-block). END YOUR TURN NOW."
	@echo ">>> foci's graceful shutdown waits only ~30s for your in-flight turn before"
	@echo ">>> being force-stopped mid-cleanup. Do post-deploy checks in a NEW turn."

enable:
	systemctl enable $(SERVICE_NAME)

# First-time provision. Ordered explicitly (sub-make) so it is correct even
# under -j. Usually invoked by download.sh after fetching the repo.
setup:
	$(MAKE) deploy-build
	$(MAKE) provision
	$(MAKE) install-shared
	$(MAKE) install-bin
	$(MAKE) install-unit
	$(MAKE) install-polkit
	$(MAKE) wizard
	$(MAKE) enable
	systemctl start $(SERVICE_NAME)

# Deploy new code to an existing install. check-config runs against the FRESH
# binary before anything is installed, so a bad config aborts untouched.
update:
	$(MAKE) deploy-build
	$(MAKE) check-config
	$(MAKE) install-bin
	$(MAKE) install-docs
	$(MAKE) install-unit
	$(MAKE) stage-changelog
	$(MAKE) restart

# Offload a compile of the whole module to a Mac (opt-in), mirroring foci-client's
# remote-* pattern: rsync the working tree over, build there. Cross-compiles for
# the Linux deploy target (not the Mac's native darwin) so the linux-only files
# (e.g. internal/procx/cap_linux.go) are the ones checked. Config in a gitignored
# .remote.mk:  REMOTE_HOST := mac   REMOTE_DIR := ~/git/foci
# Additive — no existing target uses it. Requires Go on the remote.
REMOTE_SSH_TIMEOUT ?= 5
.PHONY: remote-build
remote-build:
	@[ -n "$(REMOTE_HOST)" ] || { echo ">>> set REMOTE_HOST + REMOTE_DIR in .remote.mk to enable remote builds"; exit 1; }
	@ssh -o ConnectTimeout=$(REMOTE_SSH_TIMEOUT) -o BatchMode=yes $(REMOTE_HOST) true 2>/dev/null || { echo ">>> '$(REMOTE_HOST)' not reachable"; exit 1; }
	@echo ">>> Syncing foci -> $(REMOTE_HOST):$(REMOTE_DIR) ..."
	rsync -az --delete --exclude='.git' --exclude='/bin/' --exclude='.remote.mk' --exclude='.ci.mk' ./ "$(REMOTE_HOST):$(REMOTE_DIR)/"
	@echo ">>> GOOS=linux GOARCH=amd64 go build ./... on $(REMOTE_HOST) ..."
	ssh $(REMOTE_HOST) 'cd $(REMOTE_DIR) && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...'
