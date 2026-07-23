package testharness

import (
	"os"
	"testing"
)

// TestMain reclaims the process-lifetime shared-binary cache (sharedBinDir,
// built by sharedBinary in gateway.go) once this test binary's whole run is
// done — see CleanupSharedBinaries for why that cache has no other owner
// (foci_todo #1498). Note: capture m.Run()'s code first, THEN clean up, THEN
// os.Exit — os.Exit(m.Run()) would run the cleanup after the process has
// already exited, i.e. never (the same class of bug fixed in
// cmd/foci/main_test.go).
func TestMain(m *testing.M) {
	code := m.Run()
	CleanupSharedBinaries()
	os.Exit(code)
}
