// Package execguardtest builds hermetic execguard views for tests in other
// packages, so a test's findings never depend on the host's real PATH or
// filesystem.
package execguardtest

import "foci/internal/execguard"

// Env builds a map-backed execguard.Env: PATH directories, writable paths and
// executable files. Every other path is neither writable nor executable.
func Env(pathDirs, writable, executables []string) execguard.Env {
	w := make(map[string]bool, len(writable))
	for _, p := range writable {
		w[p] = true
	}
	x := make(map[string]bool, len(executables))
	for _, p := range executables {
		x[p] = true
	}
	return execguard.Env{
		CanWrite:     func(p string) bool { return w[p] },
		PathDirs:     pathDirs,
		IsExecutable: func(p string) bool { return x[p] },
		HomeDir:      "/home/foci",
	}
}
