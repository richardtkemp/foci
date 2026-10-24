package shell

import (
	"os"
	"testing"
	"time"
)

// TestMain isolates exec/shell tests from a live foci agent's bridge: a running
// agent's session exports FOCI_SOCK (the production exec-bridge socket) and
// BASH_ENV (the foci_* shell functions), which ExecTool subprocesses would
// otherwise inherit and hit the production bridge. Tests that need a bridge set
// FOCI_SOCK themselves. (Before 2.1's package split this lived in the unified
// tools TestMain.)
func TestMain(m *testing.M) {
	for _, k := range []string{"FOCI_SOCK", "BASH_ENV", "FOCI_GW_SOCK", "FOCI_ADDR", "FOCI_SESSION_KEY"} {
		os.Unsetenv(k)
	}
	// The leaked-child tests wait out the reap grace once each; 2s of it
	// proved nothing a fifth of a second does not (#2539). Set before m.Run,
	// so no test goroutine races the write. A grace that fires early is
	// harmless: it only kills the group of a process that has already exited,
	// and the readers still drain what is in the pipes.
	pipeReapGrace = 200 * time.Millisecond
	os.Exit(m.Run())
}
