// Package tempdir provides the canonical temp directory for foci.
// All temp files created by foci live under /tmp/foci/ to avoid scattering
// files throughout /tmp.
package tempdir

import "os"

const (
	// Root is the base temp directory for all foci temp files.
	Root = "/tmp/foci"

	// Tests is the temp directory for test-generated files.
	Tests = Root + "/tests"
)

// Dir returns Root after ensuring it exists. Safe to call concurrently;
// MkdirAll is a no-op if the directory already exists.
func Dir() string {
	_ = os.MkdirAll(Root, 0755)
	return Root
}

// TestDir returns Tests after ensuring it exists.
func TestDir() string {
	_ = os.MkdirAll(Tests, 0755)
	return Tests
}
