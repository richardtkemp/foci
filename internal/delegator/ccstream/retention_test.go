package ccstream

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupPeriod(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := CleanupPeriod(); got != 30*24*time.Hour {
		t.Fatalf("no settings.json: got %v, want CC default 30d", got)
	}
	dir := filepath.Join(home, ".claude")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"cleanupPeriodDays": 7, "other": 1}`), 0o644)
	if got := CleanupPeriod(); got != 7*24*time.Hour {
		t.Fatalf("cleanupPeriodDays=7: got %v, want 7d", got)
	}
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`not json`), 0o644)
	if got := CleanupPeriod(); got != 30*24*time.Hour {
		t.Fatalf("unparseable settings.json: got %v, want default 30d", got)
	}
}
