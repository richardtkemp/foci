# --- Remote (Mac) offload, foci_todo #2525 -------------------------------------
# Mirrors foci-client's mk/remote.mk. A dispatched target is a trio:
#   <target>          remote if the Mac is defined AND reachable, else local
#   remote-<target>   forces the Mac (errors if it is not reachable)
#   local-<target>    forces local
# Dispatched: $(REMOTE_DISPATCH) below. Every other target runs locally, as before.
#
# Config: a gitignored, machine-local .remote.mk, inherited by worktrees through
# git-common-dir (the local include comes second so a worktree copy overrides):
#   REMOTE_HOST := richardkemp@<mac>   (user@ optional when REMOTE_USER is set)
#   REMOTE_USER := richardkemp         stated, so nothing probes for it
#   REMOTE_DIR  := ~/git/foci
# On the Mac itself the synced tree has no .git and no .remote.mk, so its make
# sees no REMOTE_HOST and never dispatches again.
-include $(shell git rev-parse --git-common-dir 2>/dev/null)/../.remote.mk
-include .remote.mk

# What is in scope, and why (#2525):
#   test-one, vet   dispatched: an agent's iteration loop; Go builds and tests on
#                   macOS since #2521, and a remote run spares the NUC.
#   test            NOT dispatched. It is make land's gate (LAND_TEST_TARGET) and
#                   the CI_HOOK history row; production is Linux, so a Mac pass
#                   cannot stand in for it. remote-test runs it on the Mac on demand.
#   lint            NOT dispatched: golangci-lint and deadcode are not installed on
#                   the Mac (and agents never install there).
#   build           NOT dispatched: it makes the local bin/foci-gw (a Linux
#                   binary for this host). remote-build is the cross-compile check.
#   integration     NOT dispatched: real foci-gw + tmux + cc-stub, Linux-tuned.
# A caller that runs its own heavy-lock scheme (FGW_HEAVY_LOCK=external, the
# factory gates) always runs locally: that scheme is about THIS host's lock.
REMOTE_DISPATCH := test-one vet

# Probe timeouts. Defined BEFORE the probe on purpose: the probe is a := and
# expands them there and then (foci-client mk/remote.mk explains the bug a later
# definition caused). ConnectTimeout bounds the TCP connect only; name
# resolution comes first, so the outer `timeout` is the real wall-clock cap.
REMOTE_SSH_TIMEOUT ?= 5
REMOTE_SSH_HARD_TIMEOUT ?= 8

# The ssh destination: REMOTE_HOST as given if it already names a user, else
# REMOTE_USER@REMOTE_HOST when REMOTE_USER is stated.
REMOTE_SSH_HOST := $(if $(findstring @,$(REMOTE_HOST)),$(REMOTE_HOST),$(if $(strip $(REMOTE_USER)),$(strip $(REMOTE_USER))@)$(REMOTE_HOST))

# Availability: ONE probe per make run, and only when a dispatched target (or a
# remote-* one) was asked for, so `make deploy`, `make land` etc. never touch the
# network. The verdict is exported, so a recursive sub-make reuses it (origin is
# then "environment") instead of probing again — an empty verdict propagates too.
REMOTE_PROBE_GOALS := $(REMOTE_DISPATCH) $(addprefix remote-,$(REMOTE_DISPATCH) test)
ifeq ($(origin REMOTE_AVAIL),undefined)
ifeq ($(strip $(REMOTE_HOST)),)
REMOTE_AVAIL :=
else ifeq ($(filter $(REMOTE_PROBE_GOALS),$(MAKECMDGOALS)),)
REMOTE_AVAIL :=
else
REMOTE_AVAIL := $(shell timeout $(REMOTE_SSH_HARD_TIMEOUT) ssh -o ConnectTimeout=$(REMOTE_SSH_TIMEOUT) -o BatchMode=yes $(REMOTE_SSH_HOST) true >/dev/null 2>&1 && echo yes)
endif
endif
export REMOTE_AVAIL

# The bare dispatch decision.
REMOTE_OK := $(if $(FGW_HEAVY_LOCK),,$(if $(and $(strip $(REMOTE_HOST)),$(REMOTE_AVAIL)),yes,))
REMOTE_WHY := $(if $(FGW_HEAVY_LOCK),FGW_HEAVY_LOCK=external callers run locally,$(if $(strip $(REMOTE_HOST)),remote host '$(REMOTE_SSH_HOST)' unreachable,no REMOTE_HOST in .remote.mk))

# Where a checkout runs on the Mac: one directory per worktree, beside (never
# inside) REMOTE_DIR, so two worktrees cannot rsync --delete over each other and
# a git checkout at REMOTE_DIR is left alone. ~/ is relative to the remote home.
REMOTE_RUN_DIR ?= $(REMOTE_DIR)-wt/$(notdir $(CURDIR))

# Variables a dispatched target forwards to the Mac's make, when set.
REMOTE_FWD_VARS := PKG RUN V COUNT TIMEOUT

# Entry guard of every remote-* target: expands only when that recipe runs.
REMOTE_GUARD = $(if $(strip $(REMOTE_AVAIL)),@:,$(error remote host $(if $(strip $(REMOTE_HOST)),'$(REMOTE_SSH_HOST)' is UNREACHABLE,is NOT CONFIGURED (no REMOTE_HOST in .remote.mk)). $@ has no local fallback: run $(patsubst remote-%,%,$@) (dispatches) or local-$(patsubst remote-%,%,$@). Probe: ssh -o ConnectTimeout=$(REMOTE_SSH_TIMEOUT) -o BatchMode=yes $(REMOTE_SSH_HOST) true))

.PHONY: $(REMOTE_DISPATCH) $(addprefix remote-,$(REMOTE_DISPATCH) test) remote-build

# Says where it ran, every time: a silent fallback would build somewhere other
# than where the caller thinks.
$(REMOTE_DISPATCH):
	@if [ -n "$(REMOTE_OK)" ]; then \
	  echo ">>> $@: running on the Mac ($(REMOTE_SSH_HOST)); 'make local-$@' forces local" >&2; \
	  $(MAKE) --no-print-directory remote-$@; \
	else \
	  echo ">>> $@: running LOCALLY ($(REMOTE_WHY))" >&2; \
	  $(MAKE) --no-print-directory local-$@; \
	fi

# rsync this checkout to the Mac, run local-<target> there, copy its log back and
# exit with the Mac's make status (scripts/remote-run.sh).
$(addprefix remote-,$(REMOTE_DISPATCH) test):
	$(REMOTE_GUARD)
	@REMOTE_SSH_TIMEOUT='$(REMOTE_SSH_TIMEOUT)' FGW_ROOT='$(FGW_ROOT)' bash scripts/remote-run.sh '$(REMOTE_SSH_HOST)' '$(REMOTE_RUN_DIR)' local-$(patsubst remote-%,%,$@) \
	  $(foreach v,$(REMOTE_FWD_VARS),$(if $(value $(v)),'$(v)=$(value $(v))'))

# Offload a compile of the whole module to a Mac, cross-compiled for the Linux
# deploy target (not darwin) so the linux-only files (e.g.
# internal/procx/cap_linux.go) are the ones checked. Requires Go on the remote.
# ServerAlive bounds an ESTABLISHED session whose peer silently vanishes — the
# failure that otherwise hangs forever (no EOF arrives). It is not a wall-clock
# cap: sshd answers the probes while the remote command prints nothing.
# Measured on foci-client#1749: without them ssh still hung at 87s; with them it
# died on schedule, and a healthy 60s-silent command survived. 15 x 8 = 2 min.
REMOTE_LIVE_OPTS ?= -o ConnectTimeout=$(REMOTE_SSH_TIMEOUT) -o BatchMode=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=8
remote-build:
	@[ -n "$(REMOTE_HOST)" ] || { echo ">>> set REMOTE_HOST + REMOTE_DIR in .remote.mk to enable remote builds"; exit 1; }
	@ssh -o ConnectTimeout=$(REMOTE_SSH_TIMEOUT) -o BatchMode=yes $(REMOTE_SSH_HOST) true 2>/dev/null || { echo ">>> '$(REMOTE_SSH_HOST)' not reachable"; exit 1; }
	@echo ">>> Syncing foci -> $(REMOTE_SSH_HOST):$(REMOTE_DIR) ..."
	rsync -e 'ssh $(REMOTE_LIVE_OPTS)' --timeout=300 -az --delete --exclude='.git' --exclude='/bin/' --exclude='.remote.mk' --exclude='.ci.mk' ./ "$(REMOTE_SSH_HOST):$(REMOTE_DIR)/"
	@echo ">>> GOOS=linux GOARCH=amd64 go build ./... on $(REMOTE_SSH_HOST) ..."
	ssh $(REMOTE_LIVE_OPTS) $(REMOTE_SSH_HOST) 'cd $(REMOTE_DIR) && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...'
