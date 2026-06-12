package discord

import (
	"strings"
	"testing"

	"foci/internal/platform"
)

// validTestToken matches the Discord bot token regex (24+.6.27+ base64url chars).
const validTestToken = "NTk5MTIzNDU2Nzg5MDEyMzQ1Njc4.Xh4abc.abcdefghijklmnopqrstuvwxyz1"

// scriptedUI implements platform.SetupUI with canned prompt/menu answers.
type scriptedUI struct {
	prompts []string // successive Prompt answers
	menus   []int    // successive Menu selections
	backAt  int      // 1-based prompt index that answers "back"; 0 = never
	printed []string

	promptCalls int
	menuCalls   int
}

func (u *scriptedUI) Prompt(string, string) (string, bool) {
	u.promptCalls++
	if u.backAt > 0 && u.promptCalls == u.backAt {
		return "", true
	}
	if len(u.prompts) == 0 {
		return "", false
	}
	answer := u.prompts[0]
	u.prompts = u.prompts[1:]
	return answer, false
}

func (u *scriptedUI) Menu(string, []string) (int, bool) {
	u.menuCalls++
	if len(u.menus) == 0 {
		return 0, true
	}
	idx := u.menus[0]
	u.menus = u.menus[1:]
	return idx, false
}

func (u *scriptedUI) MultiSelect(string, []string) ([]int, bool) { return nil, true }

func (u *scriptedUI) Print(text string) { u.printed = append(u.printed, text) }

// TestIsValidBotToken verifies the bot token format check accepts real-shaped
// tokens and rejects malformed ones.
func TestIsValidBotToken(t *testing.T) {
	tests := []struct {
		token string
		want  bool
	}{
		{validTestToken, true},
		{"", false},
		{"too.short.token", false},
		{"no-dots-at-all", false},
		{"has spaces.abcdef.ghijklmnopqrstuvwxyzabcdefg", false},
	}
	for _, tt := range tests {
		if got := IsValidBotToken(tt.token); got != tt.want {
			t.Errorf("IsValidBotToken(%q) = %v, want %v", tt.token, got, tt.want)
		}
	}
}

// TestIsValidUserID verifies snowflake user ID validation (17-20 digits).
func TestIsValidUserID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"123456789012345678", true},
		{"12345678901234567", true},
		{"1234", false},
		{"123456789012345678901", false},
		{"12345678901234567a", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsValidUserID(tt.id); got != tt.want {
			t.Errorf("IsValidUserID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

// TestSetupFlags verifies the provider declares its two required CLI flags.
func TestSetupFlags(t *testing.T) {
	p := &discordProvider{}
	flags := p.SetupFlags()
	if len(flags) != 2 {
		t.Fatalf("expected 2 flags, got %d", len(flags))
	}
	if flags[0].Name != "discord-bot-token" || !flags[0].Required {
		t.Errorf("unexpected flag %+v", flags[0])
	}
	if flags[1].Name != "discord-user-id" || !flags[1].Required {
		t.Errorf("unexpected flag %+v", flags[1])
	}
}

// TestRunSetupNonInteractive verifies flag validation: missing token, missing
// user ID, invalid formats, and the success path producing config + secret.
func TestRunSetupNonInteractive(t *testing.T) {
	p := &discordProvider{}
	uid := "123456789012345678"

	tests := []struct {
		name    string
		flags   map[string]string
		wantErr string
	}{
		{"missing token", map[string]string{"discord-user-id": uid}, "discord-bot-token"},
		{"missing user ID", map[string]string{"discord-bot-token": validTestToken}, "discord-user-id"},
		{"invalid token", map[string]string{"discord-bot-token": "bad", "discord-user-id": uid}, "invalid bot token"},
		{"invalid user ID", map[string]string{"discord-bot-token": validTestToken, "discord-user-id": "bad"}, "invalid user ID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := p.RunSetup(nil, tt.flags, true)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}

	res, err := p.RunSetup(nil, map[string]string{
		"discord-bot-token": validTestToken,
		"discord-user-id":   uid,
		"agent-id":          "myagent",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.ConfigTOML, `id = "discord"`) || !strings.Contains(res.ConfigTOML, uid) {
		t.Errorf("unexpected config TOML %q", res.ConfigTOML)
	}
	if res.Secrets["discord.myagent"] != validTestToken {
		t.Errorf("expected secret stored, got %v", res.Secrets)
	}
}

// TestRunSetupInteractive verifies the interactive flow: an invalid token is
// re-prompted, then manual user ID entry (menu option 1) validates and builds
// the result.
func TestRunSetupInteractive(t *testing.T) {
	p := &discordProvider{}
	ui := &scriptedUI{
		prompts: []string{"garbage-token", validTestToken, "notanumber", "123456789012345678"},
		menus:   []int{1}, // manual entry
	}

	res, err := p.RunSetup(ui, map[string]string{"agent-id": "a"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.ConfigTOML, "123456789012345678") {
		t.Errorf("expected user ID in config, got %q", res.ConfigTOML)
	}
	if res.Secrets["discord.a"] != validTestToken {
		t.Errorf("expected token secret, got %v", res.Secrets)
	}
	if ui.promptCalls != 4 {
		t.Errorf("expected 4 prompts (2 token, 2 user ID), got %d", ui.promptCalls)
	}
}

// TestRunSetupInteractiveBack verifies backing out of the token prompt returns
// ErrSetupBack.
func TestRunSetupInteractiveBack(t *testing.T) {
	p := &discordProvider{}
	ui := &scriptedUI{backAt: 1}
	if _, err := p.RunSetup(ui, nil, false); err != platform.ErrSetupBack {
		t.Errorf("expected ErrSetupBack, got %v", err)
	}
}

// TestBuildResult verifies result construction with and without an agent ID
// (no agent: no secrets recorded).
func TestBuildResult(t *testing.T) {
	res := buildResult("", "tok", "123")
	if len(res.Secrets) != 0 {
		t.Errorf("expected no secrets without agent ID, got %v", res.Secrets)
	}
	if !strings.Contains(res.ConfigTOML, `allowed_users = ["123"]`) {
		t.Errorf("unexpected TOML %q", res.ConfigTOML)
	}

	res = buildResult("agent1", "tok", "123")
	if res.Secrets["discord.agent1"] != "tok" {
		t.Errorf("expected secret for agent, got %v", res.Secrets)
	}
}
