package telegram

import (
	"fmt"
	"time"

	"foci/internal/platform"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// SetupFlags returns CLI flag definitions for the telegram provider.
func (p *telegramProvider) SetupFlags() []platform.SetupFlag {
	return []platform.SetupFlag{
		{Name: "telegram-bot-token", Usage: "Telegram bot token", Required: true},
		{Name: "telegram-user-id", Usage: "Telegram user ID", Required: true},
	}
}

// RunSetup runs the telegram setup flow (interactive or non-interactive).
func (p *telegramProvider) RunSetup(ui platform.SetupUI, flags map[string]string, nonInteractive bool) (*platform.WizardResult, error) {
	if nonInteractive {
		return p.runSetupNonInteractive(flags)
	}
	return p.runSetupInteractive(ui, flags)
}

func (p *telegramProvider) runSetupNonInteractive(flags map[string]string) (*platform.WizardResult, error) {
	botToken := flags["telegram-bot-token"]
	userID := flags["telegram-user-id"]

	if botToken == "" {
		return nil, fmt.Errorf("--telegram-bot-token is required")
	}
	if userID == "" {
		return nil, fmt.Errorf("--telegram-user-id is required")
	}
	if !IsValidBotToken(botToken) {
		return nil, fmt.Errorf("invalid bot token format")
	}
	if !IsValidUserID(userID) {
		return nil, fmt.Errorf("invalid user ID format")
	}

	agentID := flags["agent-id"]
	return buildResult(agentID, botToken, userID), nil
}

func (p *telegramProvider) runSetupInteractive(ui platform.SetupUI, flags map[string]string) (*platform.WizardResult, error) {
	// Step 1: Bot token
	botToken, back := promptBotToken(ui)
	if back {
		return nil, platform.ErrSetupBack
	}

	// Step 2: User ID (auto-detect or manual)
	userID, back := promptUserID(ui, botToken)
	if back {
		// Go back to bot token — but we only have one level of "back" to
		// report to the caller (the generic wizard handles step navigation).
		return nil, platform.ErrSetupBack
	}

	agentID := flags["agent-id"]
	return buildResult(agentID, botToken, userID), nil
}

func promptBotToken(ui platform.SetupUI) (token string, back bool) {
	ui.Print("Telegram Bot")
	ui.Print("  Create a bot via @BotFather on Telegram (https://t.me/BotFather)")
	ui.Print("  Send /newbot, follow the prompts, and paste the token here.")
	ui.Print("")

	for {
		input, b := ui.Prompt("Bot token", "")
		if b {
			return "", true
		}
		if IsValidBotToken(input) {
			ui.Print("  Bot token validated.")
			return input, false
		}
		ui.Print("  Invalid token format. Expected: 123456789:AAF-... (get it from @BotFather)")
	}
}

func promptUserID(ui platform.SetupUI, botToken string) (userID string, back bool) {
	ui.Print("Your Telegram User ID")
	ui.Print("  How would you like to identify yourself?")

	idx, b := ui.Menu("", []string{"Auto-detect (send a message to your bot)", "Enter manually"})
	if b {
		return "", true
	}

	switch idx {
	case 0:
		uid, err := autoDetectUserID(ui, botToken)
		if err != nil {
			ui.Print(fmt.Sprintf("  Auto-detect failed: %v", err))
			ui.Print("  Falling back to manual entry.")
			return manualUserID(ui)
		}
		return uid, false
	case 1:
		return manualUserID(ui)
	}
	return "", true
}

// autoDetectUserID starts the bot temporarily and captures sender info.
func autoDetectUserID(ui platform.SetupUI, botToken string) (string, error) {
	bot, err := gotgbot.NewBot(botToken, nil)
	if err != nil {
		return "", fmt.Errorf("connect to Telegram: %w", err)
	}

	ui.Print(fmt.Sprintf("  Bot connected as @%s", bot.Username))
	ui.Print("  Send a message to your bot on Telegram, then press Enter here.")

	// Wait for Enter via prompt
	ui.Prompt("", "")

	// Poll for messages with a short timeout
	updates, err := bot.GetUpdates(&gotgbot.GetUpdatesOpts{
		RequestOpts: &gotgbot.RequestOpts{
			Timeout: 5 * time.Second,
		},
		AllowedUpdates: []string{"message"},
	})
	if err != nil {
		return "", fmt.Errorf("poll for messages: %w", err)
	}

	if len(updates) == 0 {
		// Try again with a wait
		ui.Print("  No messages yet. Waiting up to 30 seconds...")
		updates, err = bot.GetUpdates(&gotgbot.GetUpdatesOpts{
			RequestOpts: &gotgbot.RequestOpts{
				Timeout: 35 * time.Second,
			},
			Timeout:        30,
			AllowedUpdates: []string{"message"},
		})
		if err != nil {
			return "", fmt.Errorf("poll for messages: %w", err)
		}
	}

	if len(updates) == 0 {
		return "", fmt.Errorf("no messages received within timeout")
	}

	// Collect unique senders
	type sender struct {
		ID   int64
		Name string
	}
	seen := map[int64]bool{}
	var senders []sender
	for _, u := range updates {
		if u.Message == nil || u.Message.From == nil {
			continue
		}
		from := u.Message.From
		if seen[from.Id] {
			continue
		}
		seen[from.Id] = true
		name := from.FirstName
		if from.LastName != "" {
			name += " " + from.LastName
		}
		senders = append(senders, sender{ID: from.Id, Name: name})
	}

	if len(senders) == 0 {
		return "", fmt.Errorf("no user messages found in updates")
	}

	if len(senders) == 1 {
		s := senders[0]
		ui.Print(fmt.Sprintf("  User ID: %d (%s)", s.ID, s.Name))
		return fmt.Sprintf("%d", s.ID), nil
	}

	// Multiple senders — ask which one
	options := make([]string, len(senders))
	for i, s := range senders {
		options[i] = fmt.Sprintf("%s (ID: %d)", s.Name, s.ID)
	}
	idx, _ := ui.Menu("Which one is you?", options)
	s := senders[idx]
	ui.Print(fmt.Sprintf("  User ID: %d (%s)", s.ID, s.Name))
	return fmt.Sprintf("%d", s.ID), nil
}

func manualUserID(ui platform.SetupUI) (string, bool) {
	ui.Print("  Message @userinfobot on Telegram to find your user ID.")
	for {
		input, back := ui.Prompt("User ID", "")
		if back {
			return "", true
		}
		if IsValidUserID(input) {
			ui.Print(fmt.Sprintf("  User ID: %s", input))
			return input, false
		}
		ui.Print("  Invalid user ID. Expected a numeric ID (e.g. 12345678).")
	}
}

func buildResult(agentID, botToken, userID string) *platform.WizardResult {
	configTOML := fmt.Sprintf("[telegram]\nallowed_users = [\"%s\"]\n", userID)

	secrets := map[string]string{}
	if agentID != "" {
		secrets["telegram."+agentID] = botToken
	}

	return &platform.WizardResult{
		ConfigTOML: configTOML,
		Secrets:    secrets,
	}
}

// Compile-time check.
var _ platform.SetupWizard = (*telegramProvider)(nil)
