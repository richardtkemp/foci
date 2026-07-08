// Package testtemp provides the canonical temp directory for foci test code.
// All test temp files live under /tmp/fgw/ (or $FOCI_TEST_TMPDIR when the
// Makefile sets it to a per-run TESTDIR for hermetic isolation). A daily cron
// sweeps entries older than 24h.
//
// Non-_test.go files in test-only packages (e.g. internal/testharness) are
// linted as production code by golangci-lint, so they must call Mkdir/Create
// here rather than os.MkdirTemp/os.CreateTemp (which forbidigo bans).
package testtemp

import (
	"os"
	"sync"
)

const (
	// Root is the default base temp directory for all foci test temp files.
	Root = "/tmp/fgw"

	// EnvOverride names the environment variable that, when set, overrides
	// the test temp root entirely. The Makefile test targets set it to the
	// per-run TESTDIR so test runs are hermetic and cleaned up by the
	// Makefile itself (instead of waiting for the daily cron sweep).
	EnvOverride = "FOCI_TEST_TMPDIR"
)

var (
	once    sync.Once
	current string
)

// Dir returns the effective test temp root, creating it if necessary.
// Resolution mirrors tempdir.Dir(): $FOCI_TEST_TMPDIR if set and writable,
// otherwise the shared /tmp/fgw root. An unusable override falls through to
// the default rather than failing.
func Dir() string {
	once.Do(func() {
		current = resolve(os.Getenv(EnvOverride))
	})
	return current
}

func resolve(override string) string {
	if override != "" {
		if err := os.MkdirAll(override, 0o1777); err == nil {
			return override
		}
	}
	_ = os.MkdirAll(Root, 0o1777)
	return Root
}

// Mkdir creates a temp directory under the test temp root.
func Mkdir(pattern string) (string, error) {
	return os.MkdirTemp(Dir(), pattern)
}

// Create creates a temp file under the test temp root.
func Create(pattern string) (*os.File, error) {
	return os.CreateTemp(Dir(), pattern)
}
