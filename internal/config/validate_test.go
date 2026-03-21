package config

import (
	"testing"
)

// fakeSecrets is a simple in-memory SecretGetter for tests.
type fakeSecrets map[string]string

func (f fakeSecrets) Get(key string) (string, bool) {
	v, ok := f[key]
	return v, ok
}

// TestDetectBotTokenConflicts_NoConflicts verifies that agents with distinct
// tokens on both platforms produce no conflicts.
func TestDetectBotTokenConflicts_NoConflicts(t *testing.T) {
	agents := []AgentConfig{
		{ID: "alice", Platforms: []PlatformConfig{{ID: "telegram", Bot: "bot_a"}}},
		{ID: "bob", Platforms: []PlatformConfig{{ID: "telegram", Bot: "bot_b"}}},
	}
	secrets := fakeSecrets{
		"telegram.bot_a": "token_a",
		"telegram.bot_b": "token_b",
	}
	conflicts := DetectBotTokenConflicts(agents, secrets)
	if len(conflicts) != 0 {
		t.Fatalf("expected 0 conflicts, got %d: %+v", len(conflicts), conflicts)
	}
}

// TestDetectBotTokenConflicts_TelegramDuplicate verifies that two agents
// sharing the same resolved Telegram token are detected.
func TestDetectBotTokenConflicts_TelegramDuplicate(t *testing.T) {
	agents := []AgentConfig{
		{ID: "alice", Platforms: []PlatformConfig{{ID: "telegram", Bot: "mybot"}}},
		{ID: "bob", Platforms: []PlatformConfig{{ID: "telegram", Bot: "mybot"}}},
	}
	secrets := fakeSecrets{
		"telegram.mybot": "same_token",
	}
	conflicts := DetectBotTokenConflicts(agents, secrets)
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d: %+v", len(conflicts), conflicts)
	}
	c := conflicts[0]
	if c.Platform != "telegram" {
		t.Errorf("platform = %q, want telegram", c.Platform)
	}
	if c.BotName != "mybot" {
		t.Errorf("bot name = %q, want mybot", c.BotName)
	}
	if len(c.AgentIDs) != 2 || c.AgentIDs[0] != "alice" || c.AgentIDs[1] != "bob" {
		t.Errorf("agent IDs = %v, want [alice bob]", c.AgentIDs)
	}
}

// TestDetectBotTokenConflicts_DiscordDuplicate verifies that two agents
// sharing the same resolved Discord token are detected.
func TestDetectBotTokenConflicts_DiscordDuplicate(t *testing.T) {
	agents := []AgentConfig{
		{ID: "alpha", Platforms: []PlatformConfig{{ID: "discord", Bot: "dcbot"}}},
		{ID: "beta", Platforms: []PlatformConfig{{ID: "discord", Bot: "dcbot"}}},
	}
	secrets := fakeSecrets{
		"discord.dcbot": "dc_token",
	}
	conflicts := DetectBotTokenConflicts(agents, secrets)
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d: %+v", len(conflicts), conflicts)
	}
	c := conflicts[0]
	if c.Platform != "discord" {
		t.Errorf("platform = %q, want discord", c.Platform)
	}
	if c.AgentIDs[0] != "alpha" || c.AgentIDs[1] != "beta" {
		t.Errorf("agent IDs = %v, want [alpha beta]", c.AgentIDs)
	}
}

// TestDetectBotTokenConflicts_MixedConflicts verifies simultaneous Telegram
// and Discord conflicts are returned independently.
func TestDetectBotTokenConflicts_MixedConflicts(t *testing.T) {
	agents := []AgentConfig{
		{ID: "a1", Platforms: []PlatformConfig{{ID: "telegram", Bot: "tgbot"}, {ID: "discord", Bot: "dcbot"}}},
		{ID: "a2", Platforms: []PlatformConfig{{ID: "telegram", Bot: "tgbot"}, {ID: "discord", Bot: "dcbot"}}},
	}
	secrets := fakeSecrets{
		"telegram.tgbot": "tg_tok",
		"discord.dcbot":  "dc_tok",
	}
	conflicts := DetectBotTokenConflicts(agents, secrets)
	if len(conflicts) != 2 {
		t.Fatalf("expected 2 conflicts, got %d: %+v", len(conflicts), conflicts)
	}
	platforms := map[string]bool{}
	for _, c := range conflicts {
		platforms[c.Platform] = true
	}
	if !platforms["telegram"] || !platforms["discord"] {
		t.Errorf("expected both telegram and discord conflicts, got %v", platforms)
	}
}

// TestDetectBotTokenConflicts_DifferentBotSecretSameToken verifies that two
// different bot names that resolve to the same token via bot_secret overrides
// are correctly detected as a conflict.
func TestDetectBotTokenConflicts_DifferentBotSecretSameToken(t *testing.T) {
	agents := []AgentConfig{
		{ID: "x", Platforms: []PlatformConfig{{ID: "telegram", Bot: "bot_x", BotSecret: "shared_key"}}},
		{ID: "y", Platforms: []PlatformConfig{{ID: "telegram", Bot: "bot_y", BotSecret: "shared_key"}}},
	}
	secrets := fakeSecrets{
		"shared_key": "the_same_token",
	}
	conflicts := DetectBotTokenConflicts(agents, secrets)
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d: %+v", len(conflicts), conflicts)
	}
	if conflicts[0].AgentIDs[0] != "x" || conflicts[0].AgentIDs[1] != "y" {
		t.Errorf("agent IDs = %v, want [x y]", conflicts[0].AgentIDs)
	}
}

// TestDetectBotTokenConflicts_SameBotNameDifferentSecrets verifies that two
// agents using the same bot name but different bot_secret overrides that
// resolve to different tokens are NOT flagged as a conflict.
func TestDetectBotTokenConflicts_SameBotNameDifferentSecrets(t *testing.T) {
	agents := []AgentConfig{
		{ID: "p", Platforms: []PlatformConfig{{ID: "telegram", Bot: "shared_name", BotSecret: "key_p"}}},
		{ID: "q", Platforms: []PlatformConfig{{ID: "telegram", Bot: "shared_name", BotSecret: "key_q"}}},
	}
	secrets := fakeSecrets{
		"key_p": "token_p",
		"key_q": "token_q",
	}
	conflicts := DetectBotTokenConflicts(agents, secrets)
	if len(conflicts) != 0 {
		t.Fatalf("expected 0 conflicts, got %d: %+v", len(conflicts), conflicts)
	}
}

// TestDetectBotTokenConflicts_NoPlatform verifies that agents without any
// platform config are silently ignored.
func TestDetectBotTokenConflicts_NoPlatform(t *testing.T) {
	agents := []AgentConfig{
		{ID: "bare1"},
		{ID: "bare2"},
	}
	conflicts := DetectBotTokenConflicts(agents, fakeSecrets{})
	if len(conflicts) != 0 {
		t.Fatalf("expected 0 conflicts, got %d", len(conflicts))
	}
}
