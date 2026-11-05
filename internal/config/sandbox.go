package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// validateWritableRoots requires explicit absolute paths: never interpret a typo
// or an empty entry as a grant relative to the backend's working directory.
func validateWritableRoots(key string, roots []string) error {
	for _, path := range roots {
		if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
			return fmt.Errorf("%s: writable root %q must be an absolute path without NUL bytes", key, path)
		}
	}
	return nil
}
