// Package preload injects the nosgid LD_PRELOAD shim into the foci-gw process
// environment at startup, so every child process — shell-tool subprocesses and
// delegated backends alike — inherits it via os.Environ().
//
// The shim (deploy/nosgid/nosgid.c, installed to $HOME/.lib/nosgid.so) strips
// the setuid/setgid bits from chmod-family libc calls. Under the service's
// RestrictSUIDSGID=yes hardening those bits would otherwise raise EPERM instead
// of being silently dropped, breaking agent-run builds (npm/astro/…) that touch
// setgid directories. See the C source for the full rationale.
package preload

import (
	"os"
	"path/filepath"
	"strings"

	"foci/internal/log"
)

var preloadLog = log.NewComponentLogger("preload")

// RelPath is the nosgid shim's install location relative to the service user's
// home directory. Kept hidden (.lib) to stay out of the way. The Makefile's
// install-lib target writes bin/nosgid.so here.
const RelPath = ".lib/nosgid.so"

// Apply sets LD_PRELOAD on the current process to point at the nosgid shim, if
// it is installed. Because foci-gw's shell tools (append(os.Environ(), …)) and
// all delegated backends (ccstream/opencode/codex, each based on os.Environ())
// inherit this process's environment, one os.Setenv here reaches every child.
//
// It is a no-op (with a debug log) when the shim is absent — e.g. a build made
// without a C compiler — so foci still starts, just without the setgid fixup.
// An existing LD_PRELOAD is preserved: the shim is prepended, space-separated
// (ld.so accepts space- or colon-separated lists), and re-applying is idempotent.
func Apply() {
	home, err := os.UserHomeDir()
	if err != nil {
		preloadLog.Warnf("cannot resolve home dir; nosgid LD_PRELOAD shim not applied: %v", err)
		return
	}
	so := filepath.Join(home, RelPath)
	if _, err := os.Stat(so); err != nil {
		preloadLog.Debugf("nosgid shim not installed at %s; LD_PRELOAD unchanged: %v", so, err)
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
