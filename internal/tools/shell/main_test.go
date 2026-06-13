package shell

import (
	"os"
	"testing"
)

// TestMain isolates exec/shell tests from a live foci agent's bridge: a running
// agent's session exports FOCI_SOCK (the production exec-bridge socket) and
// BASH_ENV (the foci_* shell functions), which ExecTool subprocesses would
// otherwise inherit and hit the production bridge. Tests that need a bridge set
// FOCI_SOCK themselves. (Before 2.1's package split this lived in the unified
// tools TestMain.)
func TestMain(m *testing.M) {
	for _, k := range []string{"FOCI_SOCK", "BASH_ENV", "FOCI_GW_SOCK", "FOCI_ADDR"} {
		os.Unsetenv(k)
	}
	os.Exit(m.Run())
}
