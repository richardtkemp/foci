package main

import (
	"regexp"
	"testing"
)

// telegramCommandName is what Telegram's setMyCommands accepts: 1-32 of
// lowercase letters, digits and underscores. One bad name makes Telegram
// reject the WHOLE menu (BOT_COMMAND_INVALID), so every bot keeps a stale menu.
var telegramCommandName = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// TestRegisterAgentCommands_NamesValidForTelegram: /limit-reset (#3043) broke
// every bot's command menu from 11 Nov until it became /limit_reset.
func TestRegisterAgentCommands_NamesValidForTelegram(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"claude-code", "codex", "opencode", ""} {
		for _, cmd := range registerForBackend(backend, "a").All() {
			if cmd.Hidden {
				continue
			}
			if !telegramCommandName.MatchString(cmd.Name) {
				t.Errorf("backend %q: command %q is not a valid Telegram command name (want %s)", backend, cmd.Name, telegramCommandName)
			}
		}
	}
}
