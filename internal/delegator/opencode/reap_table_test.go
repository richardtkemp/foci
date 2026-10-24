package opencode

import (
	"errors"
	"reflect"
	"testing"

	"foci/internal/procinfo"
)

// selectOrphans is the macOS orphan rule: the same predicate as the procfs
// scan (own UID, PPID 1, argv[0] "opencode" plus a subcommand), applied to a
// process table.
func TestSelectOrphans(t *testing.T) {
	procs := []procinfo.Proc{
		{PID: 10, PPID: 1, UID: 501, Comm: "opencode"},  // orphaned serve: reap
		{PID: 11, PPID: 99, UID: 501, Comm: "opencode"}, // has a live parent
		{PID: 12, PPID: 1, UID: 502, Comm: "opencode"},  // another user
		{PID: 13, PPID: 1, UID: 501, Comm: "node"},      // not opencode
		{PID: 14, PPID: 1, UID: 501, Comm: "opencode"},  // bare `opencode`, no subcommand
		{PID: 15, PPID: 1, UID: 501, Comm: "opencode"},  // argv unreadable
		{PID: 16, PPID: 1, UID: 501, Comm: "opencode"},  // self
		{PID: 17, PPID: 1, UID: 501, Comm: "opencode"},  // orphaned LSP child: reap
	}
	argv := map[int][]string{
		10: {"/opt/homebrew/bin/opencode", "serve", "--port", "0"},
		11: {"opencode", "serve"},
		12: {"opencode", "serve"},
		14: {"opencode"},
		16: {"opencode", "serve"},
		17: {"opencode", "run", "x", "language-server"},
	}
	cmdline := func(pid int) ([]string, error) {
		if a, ok := argv[pid]; ok {
			return a, nil
		}
		return nil, errors.New("gone")
	}
	got := selectOrphans(procs, cmdline, 501, 16)
	if want := []int{10, 17}; !reflect.DeepEqual(got, want) {
		t.Errorf("selectOrphans = %v, want %v", got, want)
	}
}
