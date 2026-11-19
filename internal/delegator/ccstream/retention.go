package ccstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"foci/internal/ccaccount"
)

// defaultCleanupPeriodDays is Claude Code's built-in transcript retention when
// the CC config folder's settings.json sets no cleanupPeriodDays.
const defaultCleanupPeriodDays = 30

// CleanupPeriod implements Spec.ResumeRetention: how long Claude Code keeps an
// idle transcript before its own startup cleanup deletes it, for a session
// whose launch env is launchEnv. It is cleanupPeriodDays from the
// settings.json of that session's account's CC config folder
// (ccaccount.ForLaunchEnv, #3104, #3186), else
// CC's default of 30 days. Foci never sweeps a root chat session's transcript
// itself (cleanup_ephemeral.go is forks only), so a session idle past this
// period will fail to --resume on its next turn.
func CleanupPeriod(launchEnv map[string]string) time.Duration {
	days := defaultCleanupPeriodDays
	if loc, err := ccaccount.ForLaunchEnv(launchEnv); err == nil {
		if raw, err := os.ReadFile(filepath.Join(loc.Dir, "settings.json")); err == nil {
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
