package ccstream

import (
	"encoding/json"
	"fmt"
)

// syncClaudeAISkillsKey is Claude Code's setting for the skills it syncs from
// the claude.ai account (listed as "anthropic-skills:<name>"). Verified live on
// CC 2.1.314 (#2953): given only through --settings, false removes all 13 of
// them from a fresh session's skill listing (32 -> 19 skills), also when the
// skills were synced earlier and also over a user settings.json saying true.
// A false in any source wins: true here does NOT list them over a user
// settings.json saying false. Plugin skills (e.g. Cloudflare's) are not
// affected.
const syncClaudeAISkillsKey = "syncClaudeAiSkills"

// launchSettings returns the --settings JSON every CC launch carries:
// settings (foci's hook settings, plus a read-only batch's permissions; ""
// when neither applies) with syncClaudeAiSkills set to claudeAISkills
// (#2953). The key is always written, so the default (false) hides the skills
// on every host, whatever ~/.claude/settings.json says.
func launchSettings(settings string, claudeAISkills bool) (string, error) {
	top := map[string]any{}
	if settings != "" {
		if err := json.Unmarshal([]byte(settings), &top); err != nil {
			return "", fmt.Errorf("launch settings: parse: %w", err)
		}
	}
	top[syncClaudeAISkillsKey] = claudeAISkills
	body, err := json.Marshal(top)
	if err != nil {
		return "", fmt.Errorf("launch settings: %w", err)
	}
	return string(body), nil
}
