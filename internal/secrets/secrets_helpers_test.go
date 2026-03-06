package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSecrets creates a temporary secrets file with given content.
func writeSecrets(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.toml")
	os.WriteFile(path, []byte(content), 0600)
	return path
}
