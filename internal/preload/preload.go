// Package preload injects the nosgid LD_PRELOAD shim into the foci-gw process
// environment at startup, so every child process — shell-tool subprocesses and
// delegated backends alike — inherits it via os.Environ().
//
// The shim (deploy/nosgid/nosgid.c, installed to Path) strips the
// setuid/setgid bits from chmod-family libc calls. Under the service's
// RestrictSUIDSGID=yes hardening those bits would otherwise raise EPERM instead
// of being silently dropped, breaking agent-run builds (npm/astro/…) that touch
// setgid directories. See the C source for the full rationale.
//
// PROTECTION (#1487). Whoever can write the shim's bytes runs code in every
// process that preloads it, so the shim is only injected when the agent (which
// runs as the same uid as foci-gw) cannot substitute it: the file AND every
// directory above it must be owned by someone other than the agent's uid and
// not writable by this process. A root-owned file is not enough on its own — a writable parent lets it be
// unlinked and recreated, and a writable grandparent lets the whole directory
// be renamed away and replaced. That is why the shim lives under /usr/local/lib
// and no longer under $HOME/.lib: $HOME is agent-writable, so nothing beneath
// it can be protected by ownership alone.
package preload

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"foci/internal/log"
)

var preloadLog = log.NewComponentLogger("preload")

// Path is the nosgid shim's install location. The Makefile's install-lib
// target writes bin/nosgid.so here root:root 0755, in a root:root 0755
// directory (NOSGID_LIB_DIR — TestPathMatchesMakefileInstallDir keeps the two
// in step).
const Path = "/usr/local/lib/foci/nosgid.so"

// fixHint is the verbatim remedy printed when the shim is present but not
// protected. install-lib is idempotent and needs only a built bin/nosgid.so.
const fixHint = "sudo make -C <foci repo> nosgid install-lib (installs it root:root 0755 into " +
	"/usr/local/lib/foci); every `sudo make update` does the same"

// trustView supplies the filesystem questions checkTrusted asks. Injected so
// tests can model a root-owned install without being root.
type trustView struct {
	lstat        func(string) (os.FileInfo, error)
	canWrite     func(string) bool
	trustedOwner func(uid uint32) bool
}

// liveView asks the kernel: access(2) accounts for ACLs, supplementary groups
// and read-only mounts, which a mode/owner reconstruction gets wrong. Any owner
// but this process's own uid is trusted (root, or an operator account such as
// the one that owns /usr/local on some hosts): the gateway runs as the agent's
// uid, so a file it owns is a file the agent can chmod back to writable. Root
// itself is always trusted, for a gateway that runs as root.
func liveView() trustView {
	self := uint32(os.Geteuid()) //nolint:gosec // uids are non-negative and fit in uint32
	return trustView{
		lstat:        os.Lstat,
		canWrite:     func(p string) bool { return unix.Access(p, unix.W_OK) == nil },
		trustedOwner: func(uid uint32) bool { return uid == 0 || uid != self },
	}
}

// Apply sets LD_PRELOAD on the current process to point at the nosgid shim, if
// it is installed and protected. Because foci-gw's shell tools
// (append(os.Environ(), …)) and all delegated backends (ccstream/opencode/codex,
// each based on os.Environ()) inherit this process's environment, one
// os.Setenv here reaches every child.
//
// It is a no-op (with a debug log) when the shim is absent — e.g. a build made
// without a C compiler — so foci still starts, just without the setgid fixup.
// A shim that is present but substitutable is refused with a WARN: foci starts
// without the fixup rather than hand every child an agent-controlled library.
// An existing LD_PRELOAD is preserved: the shim is prepended, space-separated
// (ld.so accepts space- or colon-separated lists), and re-applying is idempotent.
func Apply() {
	apply(Path, liveView())
}

func apply(so string, v trustView) {
	if _, err := os.Stat(so); err != nil {
		preloadLog.Debugf("nosgid shim not installed at %s; LD_PRELOAD unchanged: %v", so, err)
		return
	}
	if err := checkTrusted(so, v); err != nil {
		preloadLog.Warnf("nosgid LD_PRELOAD shim NOT applied — %s is not protected from the agent (%v), "+
			"so preloading it would run agent-replaceable code in every child process. "+
			"Setgid chmods in agent builds will EPERM until fixed. Fix: %s", so, err, fixHint)
		return
	}

	value := so
	if existing := os.Getenv("LD_PRELOAD"); existing != "" {
		for _, p := range strings.Fields(existing) {
			if p == so {
				return // already present — nothing to do
			}
		}
		value = so + " " + existing
	}
	if err := os.Setenv("LD_PRELOAD", value); err != nil {
		preloadLog.Warnf("set LD_PRELOAD failed: %v", err)
		return
	}
	preloadLog.Infof("nosgid LD_PRELOAD shim active (LD_PRELOAD=%s)", value)
}

// checkTrusted returns nil only if this process cannot substitute the bytes
// behind path. Every component from the file up to / is checked, along both
// the path as given and its symlink-resolved form (a link in a protected
// directory can still point into an unprotected one). A component fails if it
// is writable by this process, or owned by an untrusted uid — in practice this
// process's own (an owner can chmod its way back to writable).
func checkTrusted(path string, v trustView) error {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", path, err)
	}
	chains := []string{filepath.Clean(path)}
	if real != chains[0] {
		chains = append(chains, real)
	}
	for _, start := range chains {
		for c := start; ; c = filepath.Dir(c) {
			fi, err := v.lstat(c)
			if err != nil {
				return fmt.Errorf("stat %s: %w", c, err)
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				return fmt.Errorf("stat %s: no owner information", c)
			}
			if !v.trustedOwner(st.Uid) {
				return fmt.Errorf("%s is owned by uid %d, which this process (the agent's uid) controls", c, st.Uid)
			}
			if v.canWrite(c) {
				return fmt.Errorf("%s is writable by this process", c)
			}
			if parent := filepath.Dir(c); parent == c {
				break
			}
		}
	}
	return nil
}
