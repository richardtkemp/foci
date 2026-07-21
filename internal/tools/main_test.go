package tools

import (
	"os"
	"testing"
)

// TestMain isolates exec/shell tests from a live foci agent's bridge. When
// `go test` runs from inside a running agent's Bash session, the process
// inherits FOCI_SOCK (the production exec-bridge socket) and BASH_ENV (which
// defines the foci_* shell functions). ExecTool subprocesses inherit
// os.Environ(), so tests that exec `foci_http_request ... https://example.com`
// would hit the PRODUCTION bridge with the real secret store. Tests that
// genuinely need a bridge set FOCI_SOCK explicitly themselves.
//
// (Before 2.1's package split this lived in the tmux test TestMain; it stays
// here for the exec/shell tests that remain in package tools.)
func TestMain(m *testing.M) {
	for _, k := range []string{"FOCI_SOCK", "BASH_ENV", "FOCI_GW_SOCK", "FOCI_ADDR", "FOCI_SESSION_KEY"} {
		os.Unsetenv(k)
	}
	os.Exit(m.Run())
}
