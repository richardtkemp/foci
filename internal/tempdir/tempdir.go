// Package tempdir provides the canonical temp directory for foci.
// All temp files created by foci live under /tmp/foci/ to avoid scattering
// files throughout /tmp.
package tempdir

import (
	"fmt"
	"os"
	"sync"
)

const (
	// Root is the preferred base temp directory for all foci temp files.
	Root = "/tmp/foci"

	// Spawn is the temp directory for spawn isolation sandboxes.
	Spawn = Root + "/spawn"
)

var (
	resolveOnce sync.Once
	resolved    string // effective root, set by resolveOnce
)

// resolve determines the effective root directory. It tries Root first
// (creating with sticky-bit world-writable perms like /tmp), and falls
// back to /tmp/foci-<uid> if Root isn't writable.
func resolve() string {
	resolveOnce.Do(func() {
		resolved = probeDir(Root)
		if resolved == "" {
			resolved = probeDir(fmt.Sprintf("/tmp/foci-%d", os.Getuid()))
		}
		if resolved == "" {
			// Last resort: let the OS pick a temp dir.
			resolved = os.TempDir()
		}
	})
	return resolved
}

// probeDir tries to create dir (mode 1777) and write a temp file in it.
// Returns dir if writable, empty string otherwise.
func probeDir(dir string) string {
	if err := os.MkdirAll(dir, 0o1777); err != nil {
		return ""
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return ""
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return dir
}

// Dir returns the effective root after ensuring it exists and is writable.
func Dir() string {
	return resolve()
}

// SpawnDir returns the spawn subdirectory after ensuring it exists.
func SpawnDir() string {
	root := resolve()
	dir := root + "/spawn"
	_ = os.MkdirAll(dir, 0o1777)
	return dir
}
