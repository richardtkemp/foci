package opencode

import (
	"os"
	"testing"
)

// TestMain isolates the opencode backend tests from a live foci agent's exec
// bridge. When `go test` runs from inside a running agent's Bash session, the
// process inherits FOCI_SOCK (the production exec-bridge socket) and BASH_ENV
// (which defines the foci_* shell functions). Server.buildCmdEnv starts from
// os.Environ(), so without this scrub TestServer_buildCmdEnv_NoExtraEnv sees
// those inherited vars and fails — a false failure invisible on a clean
// machine but tripped by any agent running the suite in-session. Tests that
// genuinely need a bridge set the vars explicitly via extraEnv.
//
// Mirrors internal/tools/main_test.go (the canonical pattern for this trap).
func TestMain(m *testing.M) {
	for _, k := range []string{"FOCI_SOCK", "BASH_ENV", "FOCI_GW_SOCK", "FOCI_ADDR"} {
		os.Unsetenv(k)
	}
	os.Exit(m.Run())
}
