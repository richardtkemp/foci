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

	// EnvOverride names the environment variable that, when set, overrides
	// the temp root entirely. The Makefile test targets set it to the
	// per-run TESTDIR (alongside TMPDIR) so test runs are hermetic: on a
	// host where a live foci install owns /tmp/foci, the daemon's private
	// subdirs (app-blobs 0700, session-env, …) reject other users' writes —
	// and tests must never write into a live install's state regardless.
	// Also the supported way to run two foci instances on one host without
	// sharing a temp root.
	EnvOverride = "FOCI_TMPDIR"
)

var (
	resolveOnce sync.Once
	resolved    string // effective root, set by resolveOnce
)

// resolve determines the effective root directory, once per process.
func resolve() string {
	resolveOnce.Do(func() {
		resolved = resolveRoot(os.Getenv(EnvOverride))
	})
	return resolved
}

// resolveRoot walks the root ladder: the override (when set and usable),
// then the shared Root (created with sticky-bit world-writable perms like
// /tmp), then a per-uid fallback if Root isn't writable, then the OS temp
// dir as a last resort. An unusable override falls through to the ladder
// rather than failing — a bad FOCI_TMPDIR degrades to default behaviour
// instead of breaking every temp write.
func resolveRoot(override string) string {
	if override != "" {
		if dir := probeDir(override); dir != "" {
			return dir
		}
	}
	if dir := probeDir(Root); dir != "" {
		return dir
	}
	if dir := probeDir(fmt.Sprintf("/tmp/foci-%d", os.Getuid())); dir != "" {
		return dir
	}
	return os.TempDir()
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

// Mkdir creates a temp directory under the foci temp root.
func Mkdir(pattern string) (string, error) {
	return os.MkdirTemp(Dir(), pattern)
}

// Create creates a temp file under the foci temp root.
func Create(pattern string) (*os.File, error) {
	return os.CreateTemp(Dir(), pattern)
}

// SpawnMkdir creates a temp directory under the spawn subdirectory.
func SpawnMkdir(pattern string) (string, error) {
	return os.MkdirTemp(SpawnDir(), pattern)
}
