# --- Remote (Mac) offload, foci_todo #2525, several build hosts #2526 -----------
# Mirrors foci-client's mk/remote.mk. A dispatched target is a trio:
#   <target>          the first reachable build host in REMOTE_HOSTS, else local
#   remote-<target>   forces a build host (errors if none is reachable)
#   local-<target>    forces local
# Dispatched: $(REMOTE_DISPATCH) below. Every other target runs locally, as before.
#
# Config: a gitignored, machine-local .remote.mk, inherited by worktrees through
# git-common-dir (the local include comes second so a worktree copy overrides).
# The SAME format as foci-client's .remote.mk (see its .remote.mk.example):
#   REMOTE_HOSTS := pro air                    candidates, in order of preference
#   pro.REMOTE_HOST       := richardkemp@<ip>  ssh destination (default: the name)
#   pro.REMOTE_BUILD_LOCK := no                builds there run concurrently
#   air.REMOTE_BUILD_LOCK := yes               builds there queue (default yes)
#   REMOTE_USER := richardkemp                 a plain VAR applies to every host;
#   REMOTE_DIR  := ~/git/foci                  <host>.VAR overrides it for one
# The old single-host form (a bare REMOTE_HOST) is not read.
#
# HOW A HOST IS CHOSEN: all candidates are probed at once (a bounded `ssh true`
# each, in parallel), and the FIRST in REMOTE_HOSTS order that answers wins; none
# → local. An answering first host is chosen at once; dead ones cost one timeout. Overrides, on the command
# line or in the environment: REMOTE_HOSTS= forces local, REMOTE_HOSTS=air
# narrows the candidates, REMOTE_PICK=air uses air without probing.
#
# On the Mac itself the synced tree has no .git and no .remote.mk, and
# scripts/remote-run.sh passes REMOTE_PICK= , so its make never dispatches again.
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

# A .remote.mk still in the single-host form would otherwise be silently
# ignored, and every target would run locally.
ifeq ($(strip $(REMOTE_HOSTS)),)
ifeq ($(origin REMOTE_HOST),file)
ifneq ($(strip $(REMOTE_HOST)),)
$(warning .remote.mk sets REMOTE_HOST but not REMOTE_HOSTS: the single-host format is no longer read (#2526), so targets run LOCALLY. Convert it: see mk/remote.mk)
endif
endif
endif

# A host's ssh destination: <h>.REMOTE_HOST (default: the name, an ssh_config
# alias), prefixed with the host's REMOTE_USER when stated and not already there.
remote_cfg      = $(strip $(if $(filter undefined,$(origin $(1).$(2))),$($(2)),$($(1).$(2))))
remote_host_raw = $(strip $(or $($(1).REMOTE_HOST),$(1)))
remote_dest     = $(if $(findstring @,$(call remote_host_raw,$(1))),$(call remote_host_raw,$(1)),$(if $(call remote_cfg,$(1),REMOTE_USER),$(call remote_cfg,$(1),REMOTE_USER)@)$(call remote_host_raw,$(1)))

# The choice: ONE probe per make run, and only when a dispatched target (or a
# remote-* one) was asked for, so `make deploy`, `make land` etc. never touch the
# network. The choice is exported, so a recursive sub-make reuses it (origin is
# then "environment") instead of probing again; an empty choice propagates too.
# The probe script. Every candidate is probed at once, each writing ok/no to its own file; the
# loop then walks REMOTE_HOSTS IN ORDER and stops at the first host that answered, once every host
# before it has failed — so the choice follows the list, never the race, and it waits only as long
# as the choice needs: a dead LATER host costs nothing once an earlier one has answered. The probes
# write only to their files (not to stdout), so $(shell) returns without waiting for stragglers,
# which die at their own timeout. Prints "!<host>" for each host passed over, then the chosen one.
# $(shell) runs /bin/sh (this Makefile sets no SHELL), so the syntax stays POSIX.
remote_probe_sh = d=$$(mktemp -d) || exit 0; \
  $(foreach h,$(REMOTE_HOSTS),{ if timeout $(REMOTE_SSH_HARD_TIMEOUT) ssh -o ConnectTimeout=$(REMOTE_SSH_TIMEOUT) -o BatchMode=yes '$(call remote_dest,$(h))' true; then echo ok; else echo no; fi > "$$d/$(h)"; } </dev/null >/dev/null 2>&1 & ) \
  while :; do s=; p=; for h in $(REMOTE_HOSTS); do r=$$(cat "$$d/$$h" 2>/dev/null); case "$$r" in ok) p=$$h; break;; no) s="$$s !$$h";; *) p=-; break;; esac; done; [ "$$p" = - ] || break; sleep 0.1; done; \
  rm -rf "$$d"; echo "$$s $$p"
REMOTE_PROBE_GOALS := $(REMOTE_DISPATCH) $(addprefix remote-,$(REMOTE_DISPATCH) test build)
ifeq ($(origin REMOTE_PICK),undefined)
ifeq ($(strip $(REMOTE_HOSTS)),)
REMOTE_PICK :=
else ifeq ($(filter $(REMOTE_PROBE_GOALS),$(MAKECMDGOALS)),)
REMOTE_PICK :=
else
REMOTE_PROBED := $(shell $(remote_probe_sh))
REMOTE_PICK := $(filter-out !%,$(REMOTE_PROBED))
ifneq ($(filter !%,$(REMOTE_PROBED)),)
$(info >>> build host(s) $(patsubst !%,%,$(filter !%,$(REMOTE_PROBED))) unreachable$(if $(REMOTE_PICK),; using $(REMOTE_PICK)))
endif
endif
endif
export REMOTE_PICK
REMOTE_PICK := $(strip $(REMOTE_PICK))
ifneq ($(REMOTE_PICK),)
ifeq ($(filter $(REMOTE_PICK),$(REMOTE_HOSTS)),)
$(error REMOTE_PICK=$(REMOTE_PICK) is not one of REMOTE_HOSTS ($(strip $(REMOTE_HOSTS))) in .remote.mk)
endif
# Every <pick>.<VAR> becomes <VAR>, used exactly as written there.
$(foreach v,$(filter $(REMOTE_PICK).%,$(.VARIABLES)),$(eval $(patsubst $(REMOTE_PICK).%,%,$(v)) = $$($(v))))
endif
REMOTE_SSH_HOST := $(if $(REMOTE_PICK),$(call remote_dest,$(REMOTE_PICK)))
REMOTE_AVAIL := $(if $(REMOTE_PICK),yes,)

# Whether runs on the chosen host take its build lock. Default yes: a host that
# does not say queues, which is slow but never corrupt.
REMOTE_BUILD_LOCK ?= yes
ifeq ($(filter yes no,$(REMOTE_BUILD_LOCK)),)
$(error $(REMOTE_PICK).REMOTE_BUILD_LOCK must be yes or no, not "$(REMOTE_BUILD_LOCK)")
endif
# The Mac-side lock file. A locked host: one file per HOST, the same one
# foci-client's $(REMOTE_LOCK) takes, so foci and foci-client runs on that Mac
# queue behind each other. An unlocked host: one file per WORKTREE, beside its
# run dir (outside it: rsync --delete would remove a lock file inside), so only
# two runs of the same checkout, which would clobber one tree, queue. Relative
# to the run dir, where the Mac's make starts.
REMOTE_HEAVY_LOCK ?= /tmp/foci-heavy-remote.lock
REMOTE_LOCK_PATH = $(if $(filter no,$(REMOTE_BUILD_LOCK)),../$(notdir $(REMOTE_RUN_DIR)).lock,$(REMOTE_HEAVY_LOCK))
# What the Mac's make is told: the lock file, and on an unlocked host a run tag
# so the per-user test dir and source symlink are per worktree too (concurrent
# runs there would otherwise rm -rf each other's test dir).
REMOTE_LOCK_ARGS = 'HEAVY_LOCK_PATH=$(REMOTE_LOCK_PATH)'$(if $(filter no,$(REMOTE_BUILD_LOCK)), 'FGW_RUN_TAG=wt-$(notdir $(REMOTE_RUN_DIR))')

# The one-line answer to "where did this run": name, destination, lock mode.
REMOTE_WHERE = $(REMOTE_PICK): $(REMOTE_SSH_HOST), $(if $(filter no,$(REMOTE_BUILD_LOCK)),no build lock,build lock)

# The bare dispatch decision.
REMOTE_OK := $(if $(FGW_HEAVY_LOCK),,$(if $(REMOTE_AVAIL),yes,))
REMOTE_WHY := $(if $(FGW_HEAVY_LOCK),FGW_HEAVY_LOCK=external callers run locally,$(if $(strip $(REMOTE_HOSTS)),no build host reachable (tried: $(strip $(REMOTE_HOSTS))),no REMOTE_HOSTS in .remote.mk))

# Where a checkout runs on the Mac: one directory per worktree, beside (never
# inside) REMOTE_DIR, so two worktrees cannot rsync --delete over each other and
# a git checkout at REMOTE_DIR is left alone. ~/ is relative to the remote home.
REMOTE_RUN_DIR ?= $(REMOTE_DIR)-wt/$(notdir $(CURDIR))

# Variables a dispatched target forwards to the Mac's make, when set.
REMOTE_FWD_VARS := PKG RUN V COUNT TIMEOUT

# Entry guard of every remote-* target: expands only when that recipe runs.
REMOTE_GUARD = $(if $(strip $(REMOTE_AVAIL)),@:,$(error $(if $(strip $(REMOTE_HOSTS)),every remote build host is UNREACHABLE (tried: $(strip $(REMOTE_HOSTS))),remote build host is NOT CONFIGURED (no REMOTE_HOSTS in .remote.mk)). $@ has no local fallback: run $(patsubst remote-%,%,$@) (dispatches) or local-$(patsubst remote-%,%,$@). Probe: $(foreach h,$(REMOTE_HOSTS),ssh -o ConnectTimeout=$(REMOTE_SSH_TIMEOUT) -o BatchMode=yes $(call remote_dest,$(h)) true;)))

.PHONY: $(REMOTE_DISPATCH) $(addprefix remote-,$(REMOTE_DISPATCH) test) remote-build

# remote-test runs `make local-test` on the Mac (remote-run.sh passes local-<target>),
# but `test` is not dispatched, so no local-test existed and remote-test failed with
# "No rule to make target `local-test'" (found 2026-10-24). It is `test` itself.
.PHONY: local-test
local-test: test

# Says where it ran, every time: a silent fallback would build somewhere other
# than where the caller thinks.
$(REMOTE_DISPATCH):
	@if [ -n "$(REMOTE_OK)" ]; then \
	  echo ">>> $@: running on build host $(REMOTE_WHERE); 'make local-$@' forces local" >&2; \
	  $(MAKE) --no-print-directory remote-$@; \
	else \
	  echo ">>> $@: running LOCALLY ($(REMOTE_WHY))" >&2; \
	  $(MAKE) --no-print-directory local-$@; \
	fi

# rsync this checkout to the Mac, run local-<target> there, copy its log back and
# exit with the Mac's make status (scripts/remote-run.sh).
$(addprefix remote-,$(REMOTE_DISPATCH) test):
	$(REMOTE_GUARD)
	@REMOTE_SSH_TIMEOUT='$(REMOTE_SSH_TIMEOUT)' FGW_ROOT='$(FGW_ROOT)' REMOTE_NAME='$(REMOTE_WHERE)' bash scripts/remote-run.sh '$(REMOTE_SSH_HOST)' '$(REMOTE_RUN_DIR)' local-$(patsubst remote-%,%,$@) \
	  $(REMOTE_LOCK_ARGS) $(foreach v,$(REMOTE_FWD_VARS),$(if $(value $(v)),'$(v)=$(value $(v))'))

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
	$(REMOTE_GUARD)
	@echo ">>> remote-build on build host $(REMOTE_WHERE): syncing -> $(REMOTE_RUN_DIR) ..."
	rsync -e 'ssh $(REMOTE_LIVE_OPTS)' --timeout=300 --rsync-path='mkdir -p $(REMOTE_RUN_DIR) && rsync' -az --delete --exclude='.git' --exclude='/bin/' --exclude='.remote.mk' --exclude='.ci.mk' ./ "$(REMOTE_SSH_HOST):$(REMOTE_RUN_DIR)/"
	@echo ">>> GOOS=linux GOARCH=amd64 go build ./... on $(REMOTE_PICK) ($(REMOTE_SSH_HOST)), lock $(REMOTE_LOCK_PATH) ..."
	ssh $(REMOTE_LIVE_OPTS) $(REMOTE_SSH_HOST) 'cd $(REMOTE_RUN_DIR) && exec 9>>$(REMOTE_LOCK_PATH) && flock -x 9 && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...'
