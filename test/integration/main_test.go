//go:build integration

package integration

import (
	"os"
	"testing"

	"foci/internal/testharness"
)

// TestMain reclaims internal/testharness's process-lifetime shared-binary
// cache once this test binary's whole run is done — see
// testharness.CleanupSharedBinaries for why that cache (foci-gw + cc-stub,
// built once per test-binary process to save relink cost) has no other
// owner and, without this, becomes a permanent unowned entry directly under
// /tmp/fgw whenever this package is run outside `make integration` (foci_todo
// #1498). Capture m.Run()'s code first, THEN clean up, THEN os.Exit —
// os.Exit(m.Run()) would skip the cleanup entirely (same bug class fixed in
// cmd/foci/main_test.go).
func TestMain(m *testing.M) {
	code := m.Run()
	testharness.CleanupSharedBinaries()
	os.Exit(code)
}
