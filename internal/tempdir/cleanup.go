package tempdir

import (
	"os"
	"path/filepath"
	"time"
)

// CleanOldFiles removes regular files matching the given glob pattern in dir
// whose modification time is older than maxAge. Returns the count of removed files.
func CleanOldFiles(dir, pattern string, maxAge time.Duration) (int, error) {
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, path := range matches {
		info, err := os.Lstat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if os.Remove(path) == nil {
				removed++
			}
		}
	}
	return removed, nil
}
