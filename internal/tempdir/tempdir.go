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
// then the shared Root (created setgid + group-writable, matching how it's
// actually deployed — see rootMode), then a per-uid fallback if Root isn't
// writable (created private to that uid — see privateMode, nothing else
// legitimately needs to write there), then the OS temp dir as a last
// resort. An unusable override falls through to the ladder rather than
// failing — a bad FOCI_TMPDIR degrades to default behaviour instead of
// breaking every temp write.
func resolveRoot(override string) string {
	if override != "" {
		if dir := probeDir(override, privateMode); dir != "" {
			return dir
		}
	}
	if dir := probeDir(Root, rootMode); dir != "" {
		return dir
	}
	if dir := probeDir(fmt.Sprintf("/tmp/foci-%d", os.Getuid()), privateMode); dir != "" {
		return dir
	}
	return os.TempDir()
}

// rootMode is the mode requested for the shared temp root when this process
// creates it (MkdirAll only ever applies a mode on actual creation — an
// already-existing dir keeps whatever ops set up out-of-band, e.g. an ACL).
// It must match live deployment reality: on a real install the root is
// observed as setgid + group-writable, NOT world-writable
// (`drwxrwsr-x`, i.e. 02775 — see #1501). Requesting 1777 (world-writable)
// here was misleading to every reader and, on a fresh install where
// MkdirAll genuinely creates the dir, handed any local user a symlink-plant
// target against every predictable-path writer under the root. Group-write
// is kept because a shared multi-uid root (the daemon plus other members of
// its group) is the deployed reality; "other" only gets read+execute.
const rootMode = 0o2775

// privateMode is the mode for a temp root that is legitimately single-uid
// (the FOCI_TMPDIR override, and the per-uid /tmp/foci-<uid> fallback used
// only when the shared Root isn't writable). Nothing else needs access, so
// there's no reason to widen it beyond the owner.
const privateMode = 0o700

// probeDir tries to create dir (with mode, applied only if this call is the
// one that actually creates it — see os.MkdirAll) and write a temp file in
// it. Returns dir if writable, empty string otherwise.
func probeDir(dir string, mode os.FileMode) string {
	if err := os.MkdirAll(dir, mode); err != nil {
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
	// Same rootMode rationale as resolveRoot: MkdirAll only applies this on
	// actual creation, and the live spawn/ dir is likewise observed
	// group-writable, not world-writable (#1501).
	_ = os.MkdirAll(dir, rootMode)
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
