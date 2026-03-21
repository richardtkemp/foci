package discord

import (
	"fmt"
	"regexp"

	"foci/internal/platform"

	"github.com/bwmarrin/discordgo"
)

var (
	// Discord bot tokens look like: NTk5MTIz...NjU3NjY0.Xh4...
	discordTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{24,}\.[A-Za-z0-9_-]{6}\.[A-Za-z0-9_-]{27,}$`)
	// Discord user IDs are snowflakes (17-20 digit numbers)
	discordUserIDRe = regexp.MustCompile(`^\d{17,20}$`)
)

// IsValidBotToken checks if a string looks like a Discord bot token.
func IsValidBotToken(token string) bool {
	return discordTokenRe.MatchString(token)
}

// IsValidUserID checks if a string is a Discord snowflake user ID.
func IsValidUserID(id string) bool {
	return discordUserIDRe.MatchString(id)
}

// SetupFlags returns CLI flag definitions for the discord provider.
func (p *discordProvider) SetupFlags() []platform.SetupFlag {
	return []platform.SetupFlag{
		{Name: "discord-bot-token", Usage: "Discord bot token", Required: true},
		{Name: "discord-user-id", Usage: "Discord user ID", Required: true},
	}
}

// RunSetup runs the discord setup flow (interactive or non-interactive).
func (p *discordProvider) RunSetup(ui platform.SetupUI, flags map[string]string, nonInteractive bool) (*platform.WizardResult, error) {
	if nonInteractive {
		return p.runSetupNonInteractive(flags)
	}
	return p.runSetupInteractive(ui, flags)
}

func (p *discordProvider) runSetupNonInteractive(flags map[string]string) (*platform.WizardResult, error) {
	botToken := flags["discord-bot-token"]
	userID := flags["discord-user-id"]

	if botToken == "" {
		return nil, fmt.Errorf("--discord-bot-token is required")
	}
	if userID == "" {
		return nil, fmt.Errorf("--discord-user-id is required")
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

func (p *discordProvider) runSetupInteractive(ui platform.SetupUI, flags map[string]string) (*platform.WizardResult, error) {
	// Step 1: Bot token
	botToken, back := promptBotToken(ui)
	if back {
		return nil, platform.ErrSetupBack
	}

	// Step 2: User ID
	userID, back := promptUserID(ui, botToken)
	if back {
		return nil, platform.ErrSetupBack
	}

	agentID := flags["agent-id"]
	return buildResult(agentID, botToken, userID), nil
}

func promptBotToken(ui platform.SetupUI) (token string, back bool) {
	ui.Print("Discord Bot")
	ui.Print("  Create a bot at https://discord.com/developers/applications")
	ui.Print("  Go to Bot > Token > Copy, and paste the token here.")
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
		ui.Print("  Invalid token format. Expected a Discord bot token (get it from the Developer Portal)")
	}
}

func promptUserID(ui platform.SetupUI, botToken string) (userID string, back bool) {
	ui.Print("Your Discord User ID")
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

// autoDetectUserID connects the bot and waits for a message to capture the sender ID.
func autoDetectUserID(ui platform.SetupUI, botToken string) (string, error) {
	dg, err := discordgo.New("Bot " + botToken)
	if err != nil {
		return "", fmt.Errorf("connect to Discord: %w", err)
	}

	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentsMessageContent

	// Channel to receive the first message
	userCh := make(chan *discordgo.User, 1)
	dg.AddHandler(func(_ *discordgo.Session, m *discordgo.MessageCreate) {
		if m.Author != nil && !m.Author.Bot {
			select {
			case userCh <- m.Author:
			default:
			}
		}
	})

	if err := dg.Open(); err != nil {
		return "", fmt.Errorf("open Discord gateway: %w", err)
	}
	defer func() { _ = dg.Close() }()

	botUser := "the bot"
	if dg.State != nil && dg.State.User != nil {
		botUser = "@" + dg.State.User.Username
	}

	ui.Print(fmt.Sprintf("  Bot connected as %s", botUser))
	ui.Print("  Send a DM to your bot on Discord, then press Enter here.")

	// Wait for Enter
	ui.Prompt("", "")

	// Wait for a message (with timeout via channel)
	select {
	case user := <-userCh:
		ui.Print(fmt.Sprintf("  User ID: %s (%s)", user.ID, user.Username))
		return user.ID, nil
	default:
		return "", fmt.Errorf("no messages received -- make sure you sent a DM to the bot")
	}
}

func manualUserID(ui platform.SetupUI) (string, bool) {
	ui.Print("  Enable Developer Mode in Discord settings, right-click your profile, and Copy User ID.")
	for {
		input, back := ui.Prompt("User ID", "")
		if back {
			return "", true
		}
		if IsValidUserID(input) {
			ui.Print(fmt.Sprintf("  User ID: %s", input))
			return input, false
		}
		ui.Print("  Invalid user ID. Expected a numeric snowflake ID (e.g. 123456789012345678).")
	}
}

func buildResult(agentID, botToken, userID string) *platform.WizardResult {
	configTOML := fmt.Sprintf("[[platforms]]\nid = \"discord\"\nallowed_users = [\"%s\"]\n", userID)

	secretsMap := map[string]string{}
	if agentID != "" {
		secretsMap["discord."+agentID] = botToken
	}

	return &platform.WizardResult{
		ConfigTOML: configTOML,
		Secrets:    secretsMap,
	}
}

// Compile-time check.
var _ platform.SetupWizard = (*discordProvider)(nil)
