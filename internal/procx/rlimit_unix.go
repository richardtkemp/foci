//go:build unix

package procx

import (
	"fmt"
	"syscall"
)

// PassOpenFileLimitToChildren makes every process foci spawns from now on
// inherit foci's own raised open-file soft limit, and returns that limit.
// Call it once at foci-gw startup, before anything is spawned.
//
// Why it is needed: the Go runtime raises its OWN soft RLIMIT_NOFILE to the
// hard limit at start-up (capped at kern.maxfilesperproc on macOS), but it
// records the original soft limit and puts it back in every child it execs
// (syscall.origRlimitNofile, go.dev/issue/46279). Under a macOS LaunchAgent
// that original is launchd's 256. So foci-gw ran with ~184k while the codex
// app-server it spawns ran with 256 — and one long-lived app-server holding
// two files per loaded thread hit it (2026-11-05: "Too many open files").
//
// Any explicit syscall.Setrlimit(RLIMIT_NOFILE, …) tells the runtime to stop
// restoring the original in children. Setting the value the runtime already
// chose changes nothing for foci itself; children now get it too.
//
// If the hard limit is low, so is the result: a hard limit cannot be raised
// without privilege. The caller logs the value.
func PassOpenFileLimitToChildren() (uint64, error) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, fmt.Errorf("getrlimit NOFILE: %w", err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return lim.Cur, fmt.Errorf("setrlimit NOFILE %d: %w", lim.Cur, err)
	}
	return lim.Cur, nil
}
