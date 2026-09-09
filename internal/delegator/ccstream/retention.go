package ccstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// defaultCleanupPeriodDays is Claude Code's built-in transcript retention when
// ~/.claude/settings.json sets no cleanupPeriodDays.
const defaultCleanupPeriodDays = 30

// CleanupPeriod returns how long Claude Code keeps an idle transcript before
// its own startup cleanup deletes it: cleanupPeriodDays from
// ~/.claude/settings.json, else CC's default of 30 days. Foci never sweeps a
// root chat session's transcript itself (cleanup_ephemeral.go is forks only),
// so a session idle past this period will fail to --resume on its next turn.
func CleanupPeriod() time.Duration {
	days := defaultCleanupPeriodDays
	if home, err := os.UserHomeDir(); err == nil {
		if raw, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); err == nil {
			var s struct {
				CleanupPeriodDays *int `json:"cleanupPeriodDays"`
			}
			if json.Unmarshal(raw, &s) == nil && s.CleanupPeriodDays != nil && *s.CleanupPeriodDays > 0 {
				days = *s.CleanupPeriodDays
			}
		}
	}
	return time.Duration(days) * 24 * time.Hour
}
