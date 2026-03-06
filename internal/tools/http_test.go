package tools

import (
	"os"
	"path/filepath"
	"testing"

	"foci/internal/secrets"
)

func writeTestSecrets(t *testing.T, content string) *secrets.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.toml")
	os.WriteFile(path, []byte(content), 0600)
	s, err := secrets.Load(path)
	if err != nil {
		t.Fatalf("load test secrets: %v", err)
	}
	return s
}
