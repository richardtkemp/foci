//go:build linux

package procx

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"

	"foci/internal/log"
)

var (
	execLog = log.NewComponentLogger("exec")
)

// clearAmbientCaps empties the ambient capability set on every OS thread of
// the process.
//
// foci-gw is granted CAP_SETGID via systemd AmbientCapabilities (or
// setpriv --ambient-caps in Docker) so the Spawn credential mechanism can call
// setgroups() to drop the foci-secrets group from children. But ambient
// capabilities are preserved across execve(2) for non-root processes, so every
// child would otherwise inherit effective CAP_SETGID — exactly the capability
// needed to setgroups() the foci-secrets GID back on and read secrets.toml.
//
// Clearing the ambient set (and only the ambient set) closes that hole: the
// parent keeps CAP_SETGID in its permitted/effective sets, so the fork-time
// setgroups in Spawn's children (performed before execve) still works, but the
// exec'd child — non-root, no file caps, empty ambient — has its permitted and
// effective sets stripped at execve. The child ends up with no CAP_SETGID.
//
// Capability sets are per-THREAD state, and a child is forked from whichever
// OS thread the spawning goroutine is running on. A plain prctl(2) clears only
// the calling thread; every thread the runtime started before Setup ran keeps
// CAP_SETGID and hands it to any child it forks (#2007: 21 of foci-gw's 22
// threads still held it in production and 63/64 probe children inherited it).
// AllThreadsSyscall runs the prctl on every runtime thread; threads created
// afterwards clone the (now empty) set from the thread that creates them.
//
// AllThreadsSyscall returns ENOTSUP in a cgo-linked binary (the runtime cannot
// see threads libc may own). Nothing here imports "C", but with CGO_ENABLED=1
// net and os/user pull runtime/cgo in, so the Makefile builds with
// CGO_ENABLED=0. The error is returned rather than downgraded so Setup fails
// closed instead of running with the hole open.
func clearAmbientCaps() error {
	_, _, errno := syscall.AllThreadsSyscall(unix.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0)
	switch {
	case errno == 0:
		return nil
	case errors.Is(errno, syscall.ENOTSUP):
		return fmt.Errorf("clear ambient capabilities on all threads: %w (binary links cgo; build with CGO_ENABLED=0)", errno)
	default:
		return fmt.Errorf("clear ambient capabilities on all threads: %w", errno)
	}
}
