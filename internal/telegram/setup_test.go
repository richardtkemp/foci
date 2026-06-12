package telegram

import (
	"strings"
	"testing"

	"foci/internal/platform"
)

const testValidToken = "123456789:AAF-abcdefghijklmnopqrstuvwxyz"

// fakeSetupUI implements platform.SetupUI with scripted responses.
// Prompt and Menu pop answers from their queues; Print output is recorded.
type fakeSetupUI struct {
	prompts []promptAnswer
	menus   []menuAnswer
	printed []string
}

type promptAnswer struct {
	input string
	back  bool
}

type menuAnswer struct {
	index int
	back  bool
}

func (u *fakeSetupUI) Prompt(_ string, _ string) (string, bool) {
	if len(u.prompts) == 0 {
		return "", true
	}
	a := u.prompts[0]
	u.prompts = u.prompts[1:]
	return a.input, a.back
}

func (u *fakeSetupUI) Menu(_ string, _ []string) (int, bool) {
	if len(u.menus) == 0 {
		return 0, true
	}
	a := u.menus[0]
	u.menus = u.menus[1:]
	return a.index, a.back
}

func (u *fakeSetupUI) MultiSelect(_ string, _ []string) ([]int, bool) {
	return nil, true
}

func (u *fakeSetupUI) Print(text string) {
	u.printed = append(u.printed, text)
}

func (u *fakeSetupUI) printedContains(sub string) bool {
	for _, p := range u.printed {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

func TestSetupFlags(t *testing.T) {
	// Proves the provider declares both required telegram setup flags.
	p := &telegramProvider{}
	flags := p.SetupFlags()
	if len(flags) != 2 {
		t.Fatalf("flags = %d, want 2", len(flags))
	}
	for _, f := range flags {
		if !f.Required {
			t.Errorf("flag %q should be required", f.Name)
		}
	}
	if flags[0].Name != "telegram-bot-token" || flags[1].Name != "telegram-user-id" {
		t.Errorf("unexpected flag names: %q, %q", flags[0].Name, flags[1].Name)
	}
}

func TestRunSetupNonInteractive(t *testing.T) {
	// Proves non-interactive setup validates both flags and builds a wizard
	// result with the allowed-user TOML and per-agent token secret.
	tests := []struct {
		name    string
		flags   map[string]string
		wantErr string
	}{
		{
			name:  "valid",
			flags: map[string]string{"telegram-bot-token": testValidToken, "telegram-user-id": "12345678", "agent-id": "scout"},
		},
		{
			name:    "missing token",
			flags:   map[string]string{"telegram-user-id": "12345678"},
			wantErr: "--telegram-bot-token is required",
		},
		{
			name:    "missing user id",
			flags:   map[string]string{"telegram-bot-token": testValidToken},
			wantErr: "--telegram-user-id is required",
		},
		{
			name:    "invalid token",
			flags:   map[string]string{"telegram-bot-token": "garbage", "telegram-user-id": "12345678"},
			wantErr: "invalid bot token format",
		},
		{
			name:    "invalid user id",
			flags:   map[string]string{"telegram-bot-token": testValidToken, "telegram-user-id": "xx"},
			wantErr: "invalid user ID format",
		},
	}
	p := &telegramProvider{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := p.RunSetup(nil, tt.flags, true)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(res.ConfigTOML, `allowed_users = ["12345678"]`) {
				t.Errorf("config TOML missing allowed user: %s", res.ConfigTOML)
			}
			if res.Secrets["telegram.scout"] != testValidToken {
				t.Errorf("secret telegram.scout = %q, want token", res.Secrets["telegram.scout"])
			}
		})
	}
}

func TestRunSetupInteractive_ManualUserID(t *testing.T) {
	// Proves the interactive flow accepts a valid token, retries an invalid
	// user ID until a valid one is entered, and builds the result.
	ui := &fakeSetupUI{
		prompts: []promptAnswer{
			{input: "bad-token"},    // rejected, re-prompted
			{input: testValidToken}, // accepted
			{input: "xx"},           // invalid user ID, re-prompted
			{input: "12345678"},     // accepted
		},
		menus: []menuAnswer{{index: 1}}, // choose manual entry
	}
	p := &telegramProvider{}
	res, err := p.RunSetup(ui, map[string]string{"agent-id": "scout"}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.ConfigTOML, `allowed_users = ["12345678"]`) {
		t.Errorf("config TOML missing user: %s", res.ConfigTOML)
	}
	if res.Secrets["telegram.scout"] != testValidToken {
		t.Errorf("secret = %q, want token", res.Secrets["telegram.scout"])
	}
	if !ui.printedContains("Invalid token format") {
		t.Error("expected invalid-token feedback to be printed")
	}
	if !ui.printedContains("Invalid user ID") {
		t.Error("expected invalid-user-id feedback to be printed")
	}
}

func TestRunSetupInteractive_BackFromToken(t *testing.T) {
	// Proves backing out of the token prompt aborts setup with ErrSetupBack.
	ui := &fakeSetupUI{prompts: []promptAnswer{{back: true}}}
	p := &telegramProvider{}
	_, err := p.RunSetup(ui, nil, false)
	if err != platform.ErrSetupBack {
		t.Fatalf("err = %v, want ErrSetupBack", err)
	}
}

func TestRunSetupInteractive_BackFromUserID(t *testing.T) {
	// Proves backing out of the user ID menu aborts setup with ErrSetupBack.
	ui := &fakeSetupUI{
		prompts: []promptAnswer{{input: testValidToken}},
		menus:   []menuAnswer{{back: true}},
	}
	p := &telegramProvider{}
	_, err := p.RunSetup(ui, nil, false)
	if err != platform.ErrSetupBack {
		t.Fatalf("err = %v, want ErrSetupBack", err)
	}
}

func TestManualUserID_Back(t *testing.T) {
	// Proves backing out of the manual user ID prompt reports back=true.
	ui := &fakeSetupUI{prompts: []promptAnswer{{back: true}}}
	_, back := manualUserID(ui)
	if !back {
		t.Error("expected back=true")
	}
}

func TestBuildResult_NoAgentID(t *testing.T) {
	// Proves buildResult omits the token secret when no agent ID is given
	// (nothing to namespace the secret under).
	res := buildResult("", testValidToken, "12345678")
	if len(res.Secrets) != 0 {
		t.Errorf("secrets = %v, want empty", res.Secrets)
	}
	if !strings.Contains(res.ConfigTOML, `id = "telegram"`) {
		t.Errorf("config TOML missing platform id: %s", res.ConfigTOML)
	}
}
