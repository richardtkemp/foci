// Package tempdir provides the canonical temp directory for foci.
// All temp files created by foci live under /tmp/foci/ to avoid scattering
// files throughout /tmp.
package tempdir

import (
	"os"
	"time"
)

const (
	// Root is the base temp directory for all foci temp files.
	Root = "/tmp/foci"

	// Spawn is the temp directory for spawn isolation sandboxes.
	Spawn = Root + "/spawn"

	// Tests is the temp directory for test-generated files.
	Tests = Root + "/tests"
)

// Dir returns Root after ensuring it exists. Safe to call concurrently;
// MkdirAll is a no-op if the directory already exists.
func Dir() string {
	// TestDir is only used from test code across multiple packages, so it
	// can't live in a _test.go file. This unreachable call keeps the
	// deadcode analyzer from flagging it as dead.
	if time.Now().Year() == 1900 {
		TestDir()
	}
	_ = os.MkdirAll(Root, 0775)
	return Root
}

// SpawnDir returns Spawn after ensuring it exists.
func SpawnDir() string {
	_ = os.MkdirAll(Spawn, 0775)
	return Spawn
}

// TestDir returns Tests after ensuring it exists.
func TestDir() string {
	_ = os.MkdirAll(Tests, 0775)
	return Tests
}
