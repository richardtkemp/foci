package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"foci/internal/anthropic"
	"foci/internal/config"
	"foci/internal/provision"
	"foci/internal/secrets"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// setupFlags holds parsed flags for the setup command.
type setupFlags struct {
	configDir      string // directory for foci.toml and secrets.toml
	homeDir        string // foci user home (workspace parent)
	nonInteractive bool
	botToken       string
	userID         string
	agentID        string
	displayName    string // display name for agent
	model          string // model alias or full ID
	setupToken     string // setup token from 'claude setup-token'
	apiKey         string // API key (auth credential)
}

// setupState tracks wizard state for back navigation.
type setupState struct {
	botToken    string
	authMethod  string // "setup-token", "apikey", "skip"
	userID      string
	agentID     string
	displayName string
	model       string
	charMode    string // "defaults", "openclaw", "blank"
}

func setupUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci setup [flags]

Interactive setup wizard for first-run configuration.
Generates foci.toml, secrets.toml, and seeds character files.

Flags:
  -h, --help             Show this help
  --config-dir <path>    Directory for config files (default: ~/config)
  --non-interactive      Non-interactive mode (all required flags must be set)
  --bot-token <token>    Telegram bot token
  --user-id <id>         Telegram user ID
  --agent-id <id>        Agent identifier (default: main)
  --display-name <name>  Display name for agent (default: titlecased agent ID)
  --model <model>        Model alias or full ID: opus, sonnet, haiku (default: sonnet)
  --setup-token <token>  Setup token from 'claude setup-token'
  --api-key <key>        API key (direct Anthropic API key)
`)
}

func cmdSetup(args []string) error {
	if wantsHelp(args) {
		setupUsage()
		return nil
	}

	flags := parseSetupFlags(args)

	// Seed shared/defaults/ from repo to disk if not already present
	repoDefaultsDir := findRepoDefaults()
	if repoDefaultsDir != "" {
		targetDefaultsDir := filepath.Join(flags.homeDir, "shared", "defaults")
		if err := provision.SeedDefaults(repoDefaultsDir, targetDefaultsDir); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not seed defaults: %v\n", err)
		}
	}

	if flags.nonInteractive {
		return runSetupNonInteractive(flags)
	}
	return runSetupInteractive(flags)
}

func parseSetupFlags(args []string) setupFlags {
	var f setupFlags
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config-dir":
			if i+1 < len(args) {
				f.configDir = args[i+1]
				i++
			}
		case "--non-interactive":
			f.nonInteractive = true
		case "--bot-token":
			if i+1 < len(args) {
				f.botToken = args[i+1]
				i++
			}
		case "--user-id":
			if i+1 < len(args) {
				f.userID = args[i+1]
				i++
			}
		case "--agent-id":
			if i+1 < len(args) {
				f.agentID = args[i+1]
				i++
			}
		case "--display-name":
			if i+1 < len(args) {
				f.displayName = args[i+1]
				i++
			}
		case "--model":
			if i+1 < len(args) {
				f.model = args[i+1]
				i++
			}
		case "--setup-token":
			if i+1 < len(args) {
				f.setupToken = args[i+1]
				i++
			}
		case "--api-key":
			if i+1 < len(args) {
				f.apiKey = args[i+1]
				i++
			}
		}
	}

	// Apply defaults
	home := ""
	if h, err := os.UserHomeDir(); err == nil {
		home = h
	}
	if f.configDir == "" {
		if home != "" {
			f.configDir = filepath.Join(home, "config")
		} else {
			f.configDir = "./config"
		}
	}
	if f.homeDir == "" {
		if home != "" {
			f.homeDir = home
		} else {
			f.homeDir = "."
		}
	}
	if f.agentID == "" {
		f.agentID = "main"
	}

	return f
}

// findRepoDefaults tries to locate the shared/defaults/ directory relative to
// the running binary or the current working directory.
func findRepoDefaults() string {
	// Try relative to the executable
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "shared", "defaults")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	// Try current working directory
	if cwd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(cwd, "shared", "defaults")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}

func runSetupNonInteractive(f setupFlags) error {
	var missing []string
	if f.botToken == "" {
		missing = append(missing, "--bot-token")
	}
	if f.userID == "" {
		missing = append(missing, "--user-id")
	}
	if len(missing) > 0 {
		return fmt.Errorf("non-interactive mode requires: %s", strings.Join(missing, ", "))
	}

	if !provision.IsValidBotToken(f.botToken) {
		return fmt.Errorf("invalid bot token format")
	}

	// Resolve model
	model := provision.ResolveModelAlias(f.model)

	// Resolve display name
	displayName := f.displayName
	if displayName == "" {
		displayName = provision.TitleCase(f.agentID)
	}

	// Build secrets
	secretsOpts := config.SecretsOptions{
		AgentID:  f.agentID,
		BotToken: f.botToken,
	}

	secretsPath := filepath.Join(f.configDir, "secrets.toml")
	store, err := secrets.Load(secretsPath)
	if err != nil {
		return fmt.Errorf("load secrets: %w", err)
	}

	// Auth: setup-token takes precedence, then api-key, then skip.
	if f.setupToken != "" {
		store.Set("anthropic.setup_token", f.setupToken)
		secretsOpts.SetupToken = f.setupToken
	} else if f.apiKey != "" {
		store.Set("anthropic.api_key", f.apiKey)
		secretsOpts.SetupToken = f.apiKey
	}

	// Provision the agent workspace
	defaultsDir := filepath.Join(f.homeDir, "shared", "defaults")
	spec := provision.AgentSpec{
		ID:          f.agentID,
		Model:       model,
		DisplayName: displayName,
		HomeDir:     f.homeDir,
		DefaultsDir: defaultsDir,
		CharMode:    "defaults",
	}

	result, err := provision.Provision(spec)
	if err != nil {
		return fmt.Errorf("provision agent: %w", err)
	}

	configOpts := config.SetupOptions{
		AgentBlock:   result.ConfigBlock,
		Model:        model,
		AllowedUsers: []string{f.userID},
	}

	return writeSetupFiles(f, configOpts, secretsOpts, store, result)
}

func runSetupInteractive(f setupFlags) error {
	reader := bufio.NewReader(os.Stdin)
	state := setupState{agentID: f.agentID}

	fmt.Println()
	fmt.Println("──────────────────────────────────────────")
	fmt.Println("  Foci First-Run Setup")
	fmt.Println("  (Enter 'back' at any prompt to return to the previous step)")
	fmt.Println("──────────────────────────────────────────")

	// Load secrets store early — needed by RunSetupTokenFlow in step 3.
	secretsPath := filepath.Join(f.configDir, "secrets.toml")
	store, err := secrets.Load(secretsPath)
	if err != nil {
		return fmt.Errorf("load secrets: %w", err)
	}

	secretsOpts := config.SecretsOptions{}

	totalSteps := 8
	step := 1
	for step <= totalSteps {
		switch step {
		case 1:
			result, back := stepBotToken(reader, state.botToken, totalSteps)
			if back {
				fmt.Println("  Already at the first step.")
				continue
			}
			state.botToken = result
			step++

		case 2:
			method, back := stepAuth(reader, state.authMethod, totalSteps)
			if back {
				step--
				continue
			}
			state.authMethod = method
			step++

		case 3:
			// Collect the actual credential for the chosen auth method.
			back, err := stepCredential(reader, state.authMethod, store, &secretsOpts, totalSteps)
			if err != nil {
				return err
			}
			if back {
				step--
				continue
			}
			step++

		case 4:
			userID, back := stepUserID(reader, state.botToken, state.userID, totalSteps)
			if back {
				step--
				continue
			}
			state.userID = userID
			step++

		case 5:
			agentID, back := stepAgentID(reader, state.agentID, totalSteps)
			if back {
				step--
				continue
			}
			state.agentID = agentID
			step++

		case 6:
			displayName, back := stepDisplayName(reader, state.agentID, state.displayName, totalSteps)
			if back {
				step--
				continue
			}
			state.displayName = displayName
			step++

		case 7:
			model, back := stepModel(reader, state.model, store, totalSteps)
			if back {
				step--
				continue
			}
			state.model = model
			step++

		case 8:
			charMode, back := stepCharacterMode(reader, f, totalSteps)
			if back {
				step--
				continue
			}
			state.charMode = charMode
			step++
		}
	}

	secretsOpts.AgentID = state.agentID
	secretsOpts.BotToken = state.botToken

	// Provision the agent workspace
	defaultsDir := filepath.Join(f.homeDir, "shared", "defaults")
	spec := provision.AgentSpec{
		ID:          state.agentID,
		Model:       state.model,
		DisplayName: state.displayName,
		HomeDir:     f.homeDir,
		DefaultsDir: defaultsDir,
		CharMode:    state.charMode,
	}

	provResult, err := provision.Provision(spec)
	if err != nil {
		return fmt.Errorf("provision agent: %w", err)
	}

	configOpts := config.SetupOptions{
		AgentBlock:   provResult.ConfigBlock,
		Model:        state.model,
		AllowedUsers: []string{state.userID},
	}

	fmt.Println()
	fmt.Println("Creating config...")

	if err := writeSetupFiles(f, configOpts, secretsOpts, store, provResult); err != nil {
		return err
	}

	fmt.Println("✓ Setup complete.")
	fmt.Println()
	fmt.Println("──────────────────────────────────────────")
	return nil
}

// stepBotToken prompts for a Telegram bot token.
func stepBotToken(reader *bufio.Reader, current string, total int) (token string, back bool) {
	fmt.Println()
	fmt.Printf("Step 1/%d: Telegram Bot\n", total)
	fmt.Println("  Create a bot via @BotFather on Telegram (https://t.me/BotFather)")
	fmt.Println("  Send /newbot, follow the prompts, and paste the token here.")
	fmt.Println()

	for {
		prompt := "Bot token: "
		if current != "" {
			prompt = fmt.Sprintf("Bot token [%s...]: ", current[:min(15, len(current))])
		}
		fmt.Print(prompt)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		if input == "back" {
			return "", true
		}
		if input == "" && current != "" {
			return current, false
		}
		if provision.IsValidBotToken(input) {
			fmt.Println("✓ Bot token validated.")
			return input, false
		}
		fmt.Println("  Invalid token format. Expected: 123456789:AAF-... (get it from @BotFather)")
	}
}

// stepAuth prompts for authentication method.
func stepAuth(reader *bufio.Reader, current string, total int) (method string, back bool) { // nolint:unparam
	fmt.Println()
	fmt.Printf("Step 2/%d: Anthropic Authentication\n", total)
	fmt.Println("  Foci needs access to Claude. Choose one:")
	fmt.Println("  [1] Setup token (recommended — uses Claude Code subscription)")
	fmt.Println("  [2] API key")
	fmt.Println("  [3] Skip (use Claude Code credentials if available)")
	fmt.Println()

	for {
		fmt.Print("> ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		if input == "back" {
			return "", true
		}

		switch input {
		case "1":
			return "setup-token", false
		case "2":
			return "apikey", false
		case "3":
			return "skip", false
		default:
			fmt.Println("  Enter 1, 2, or 3.")
		}
	}
}

// stepCredential collects the actual auth credential based on the chosen method.
func stepCredential(reader *bufio.Reader, authMethod string, store *secrets.Store, opts *config.SecretsOptions, total int) (back bool, err error) {
	fmt.Println()
	fmt.Printf("Step 3/%d: Credential\n", total)

	switch authMethod {
	case "setup-token":
		fmt.Println("  Run 'claude setup-token' in another terminal and follow the prompts.")
		fmt.Println("  Type 'back' to return to auth method selection.")
		fmt.Println()
		fmt.Print("  Press Enter when ready (or 'back'): ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)
		if input == "back" {
			return true, nil
		}
		if err := anthropic.RunSetupTokenFlow(store); err != nil {
			return false, fmt.Errorf("auth: %w", err)
		}
		fmt.Println("✓ Setup token saved.")
		if v, ok := store.Get("anthropic.setup_token"); ok {
			opts.SetupToken = v
		}

	case "apikey":
		for {
			fmt.Print("  API key: ")
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(input)
			if input == "back" {
				return true, nil
			}
			if input == "" {
				fmt.Println("  API key cannot be empty.")
				continue
			}
			opts.SetupToken = input
			fmt.Println("✓ API key saved.")
			break
		}

	case "skip":
		fmt.Println("✓ Skipping auth (will use Claude Code credentials if available).")
	}

	return false, nil
}

// stepUserID prompts for user ID via auto-detect or manual entry.
func stepUserID(reader *bufio.Reader, botToken string, current string, total int) (userID string, back bool) {
	fmt.Println()
	fmt.Printf("Step 4/%d: Your Telegram User ID\n", total)
	fmt.Println("  How would you like to identify yourself?")
	fmt.Println("  [1] Auto-detect (send a message to your bot)")
	fmt.Println("  [2] Enter manually")
	fmt.Println()

	for {
		fmt.Print("> ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		if input == "back" {
			return "", true
		}

		switch input {
		case "1":
			uid, err := autoDetectUserID(reader, botToken)
			if err != nil {
				fmt.Printf("  Auto-detect failed: %v\n", err)
				fmt.Println("  Falling back to manual entry.")
				uid, back = manualUserID(reader, current)
				if back {
					return "", true
				}
			}
			return uid, false
		case "2":
			uid, back := manualUserID(reader, current)
			if back {
				return "", true
			}
			return uid, false
		default:
			fmt.Println("  Enter 1 or 2.")
		}
	}
}

// autoDetectUserID starts the bot temporarily and captures sender info.
func autoDetectUserID(reader *bufio.Reader, botToken string) (string, error) {
	bot, err := gotgbot.NewBot(botToken, nil)
	if err != nil {
		return "", fmt.Errorf("connect to Telegram: %w", err)
	}

	fmt.Printf("  Bot connected as @%s\n", bot.Username)
	fmt.Println("  Send a message to your bot on Telegram, then press Enter here.")
	fmt.Print("  ")
	_, _ = reader.ReadString('\n')

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
		fmt.Println("  No messages yet. Waiting up to 30 seconds...")
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
		fmt.Printf("✓ User ID: %d (%s)\n", s.ID, s.Name)
		return fmt.Sprintf("%d", s.ID), nil
	}

	// Multiple senders — ask which one
	fmt.Printf("\n  Received messages from %d senders:\n", len(senders))
	for i, s := range senders {
		fmt.Printf("    %d. %s (ID: %d)\n", i+1, s.Name, s.ID)
	}
	fmt.Println()
	for {
		fmt.Print("  Which one is you? Enter number: ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)
		var idx int
		if _, err := fmt.Sscanf(input, "%d", &idx); err == nil && idx >= 1 && idx <= len(senders) {
			s := senders[idx-1]
			fmt.Printf("✓ User ID: %d (%s)\n", s.ID, s.Name)
			return fmt.Sprintf("%d", s.ID), nil
		}
		fmt.Printf("  Enter a number between 1 and %d.\n", len(senders))
	}
}

// manualUserID prompts the user to enter their Telegram user ID.
func manualUserID(reader *bufio.Reader, current string) (string, bool) {
	fmt.Println("  Message @userinfobot on Telegram to find your user ID.")
	for {
		prompt := "  User ID: "
		if current != "" {
			prompt = fmt.Sprintf("  User ID [%s]: ", current)
		}
		fmt.Print(prompt)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		if input == "back" {
			return "", true
		}
		if input == "" && current != "" {
			fmt.Printf("✓ User ID: %s\n", current)
			return current, false
		}
		if provision.IsValidUserID(input) {
			fmt.Printf("✓ User ID: %s\n", input)
			return input, false
		}
		fmt.Println("  Invalid user ID. Expected a numeric ID (e.g. 12345678).")
	}
}

// stepAgentID prompts for an agent identifier.
func stepAgentID(reader *bufio.Reader, current string, total int) (agentID string, back bool) {
	fmt.Println()
	fmt.Printf("Step 5/%d: Agent ID\n", total)
	fmt.Println("  Pick a short lowercase name for your agent (letters, numbers, hyphens).")
	fmt.Println("  This becomes the agent's workspace directory and session key prefix.")
	fmt.Println()

	for {
		prompt := fmt.Sprintf("Agent ID [%s]: ", current)
		fmt.Print(prompt)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		if input == "back" {
			return "", true
		}
		if input == "" {
			fmt.Printf("✓ Agent ID: %s\n", current)
			return current, false
		}
		if provision.IsValidAgentID(input) {
			fmt.Printf("✓ Agent ID: %s\n", input)
			return input, false
		}
		fmt.Println("  Invalid ID. Use lowercase letters, numbers, and hyphens only.")
	}
}

// stepDisplayName prompts for a display name.
func stepDisplayName(reader *bufio.Reader, agentID, current string, total int) (displayName string, back bool) {
	fmt.Println()
	fmt.Printf("Step 6/%d: Display Name\n", total)
	fmt.Println("  A human-readable name for your agent (used in SOUL.md).")
	fmt.Println()

	defaultName := provision.TitleCase(agentID)
	if current != "" {
		defaultName = current
	}

	prompt := fmt.Sprintf("Display name [%s]: ", defaultName)
	fmt.Print(prompt)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)

	if input == "back" {
		return "", true
	}
	if input == "" {
		fmt.Printf("✓ Display name: %s\n", defaultName)
		return defaultName, false
	}
	fmt.Printf("✓ Display name: %s\n", input)
	return input, false
}

// stepModel prompts for a model selection.
func stepModel(reader *bufio.Reader, current string, store *secrets.Store, total int) (model string, back bool) {
	fmt.Println()
	fmt.Printf("Step 7/%d: Model\n", total)
	fmt.Println("  Choose a model: opus, sonnet, haiku, or enter a full model ID.")
	fmt.Println()

	defaultModel := "sonnet"
	if current != "" {
		defaultModel = current
	}

	prompt := fmt.Sprintf("Model [%s]: ", defaultModel)
	fmt.Print(prompt)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)

	if input == "back" {
		return "", true
	}
	if input == "" {
		input = defaultModel
	}

	// Try to discover exact model from API
	resolved := discoverModelFamily(store, input)
	fmt.Printf("✓ Model: %s\n", resolved)
	return resolved, false
}

// stepCharacterMode prompts for character file sourcing.
func stepCharacterMode(reader *bufio.Reader, f setupFlags, total int) (charMode string, back bool) { // nolint:unparam
	fmt.Println()
	fmt.Printf("Step 8/%d: Character Files\n", total)
	fmt.Println("  How should we set up the character files?")
	fmt.Println("  [1] Defaults (recommended for new users)")
	fmt.Println("  [2] OpenClaw templates")
	fmt.Println("  [3] Blank (empty files)")
	fmt.Println()

	for {
		fmt.Print("> ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		if input == "back" {
			return "", true
		}

		switch input {
		case "1", "":
			fmt.Println("✓ Character files: defaults")
			return "defaults", false
		case "2":
			fmt.Println("✓ Character files: openclaw")
			return "openclaw", false
		case "3":
			fmt.Println("✓ Character files: blank")
			return "blank", false
		default:
			fmt.Println("  Enter 1, 2, or 3.")
		}
	}
}

// writeSetupFiles writes foci.toml, secrets.toml, and ensures workspace directories exist.
func writeSetupFiles(f setupFlags, configOpts config.SetupOptions, secretsOpts config.SecretsOptions, store *secrets.Store, provResult *provision.Result) error {
	// Ensure config directory exists
	if err := os.MkdirAll(f.configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	// Write foci.toml
	configPath := filepath.Join(f.configDir, "foci.toml")
	configContent := config.GenerateConfig(configOpts)
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		return fmt.Errorf("write foci.toml: %w", err)
	}
	fmt.Printf("  → %s\n", configPath)

	// Write secrets via the store (so it handles formatting + existing values)
	if secretsOpts.BotToken != "" {
		store.Set(fmt.Sprintf("telegram.%s", secretsOpts.AgentID), secretsOpts.BotToken)
	}
	if secretsOpts.SetupToken != "" {
		store.Set("anthropic.setup_token", secretsOpts.SetupToken)
	}
	if err := store.Save(); err != nil {
		return fmt.Errorf("write secrets.toml: %w", err)
	}
	fmt.Printf("  → %s\n", filepath.Join(f.configDir, "secrets.toml"))

	// Workspace directories were already created by Provision
	fmt.Printf("  → %s/\n", provResult.Workspace)

	// Install crontab entries
	if len(provResult.CrontabLines) > 0 {
		if err := provision.AppendCrontab(provResult.CrontabLines); err != nil {
			fmt.Printf("  ⚠️  Could not install crontab entries automatically.\n")
			fmt.Printf("     Add these entries manually with `crontab -e`:\n")
			for _, line := range provResult.CrontabLines {
				fmt.Printf("     %s\n", line)
			}
		} else {
			fmt.Println("  → Crontab entries installed")
		}
	}

	return nil
}

// discoverModelFamily queries the Anthropic API to find the latest model in a family.
// Falls back to provision.ResolveModelAlias on failure.
func discoverModelFamily(store *secrets.Store, alias string) string {
	fallback := provision.ResolveModelAlias(alias)

	// Determine which family to search for
	family := strings.ToLower(strings.TrimSpace(alias))
	switch family {
	case "opus", "sonnet", "haiku":
		// proceed with API query
	default:
		// Full model ID or unknown alias — just use static resolution
		return fallback
	}

	// Get a token for the API call
	token := ""
	if v, ok := store.Get("anthropic.setup_token"); ok {
		token = v
	} else if v, ok := store.Get("anthropic.api_key"); ok {
		token = v
	}
	if token == "" {
		return fallback
	}

	fmt.Printf("  Querying Anthropic API for latest %s model... ", family)

	client := anthropic.NewClientWithTimeout(token, 5*time.Second)
	models, err := client.ListModels()
	if err != nil {
		fmt.Printf("(using default: %s)\n", fallback)
		return fallback
	}

	// Find the latest model in the requested family
	var bestID string
	var bestTime time.Time
	for _, m := range models {
		if !strings.Contains(strings.ToLower(m.ID), family) {
			continue
		}
		if m.CreatedAt.After(bestTime) {
			bestTime = m.CreatedAt
			bestID = m.ID
		}
	}

	if bestID == "" {
		fmt.Printf("(not found, using default: %s)\n", fallback)
		return fallback
	}

	fmt.Printf("✓ %s\n", bestID)
	return bestID
}

// importCharacterFiles lists .md files from srcDir and lets the user select which to import.
// Kept for potential future use with advanced import flows.
func importCharacterFiles(reader *bufio.Reader, srcDir, destDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("read directory %s: %w", srcDir, err)
	}

	knownCharacterFiles := map[string]bool{
		"SOUL.md":      true,
		"CRAFT.md":     true,
		"COHERENCE.md": true,
		"USER.md":      true,
		"MEMORY.md":    true,
	}

	type fileEntry struct {
		name     string
		size     int64
		selected bool
	}

	var files []fileEntry
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, fileEntry{
			name:     entry.Name(),
			size:     info.Size(),
			selected: knownCharacterFiles[entry.Name()],
		})
	}

	if len(files) == 0 {
		return fmt.Errorf("no .md files found in %s", srcDir)
	}

	// Sort: known files first, then alphabetical
	sort.Slice(files, func(i, j int) bool {
		ki := knownCharacterFiles[files[i].name]
		kj := knownCharacterFiles[files[j].name]
		if ki != kj {
			return ki
		}
		return files[i].name < files[j].name
	})

	printFileList := func() {
		fmt.Printf("\n  Found %d .md files (top-level only):\n", len(files))
		for i, f := range files {
			check := "[ ]"
			if f.selected {
				check = "[x]"
			}
			fmt.Printf("    %2d. %s %s (%.1f KB)\n", i+1, check, f.name, float64(f.size)/1024)
		}
		fmt.Println()
		fmt.Println("  Known character files are pre-selected. Toggle with number, 'a' for all, Enter to confirm.")
	}

	printFileList()

	for {
		fmt.Print("> ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		if input == "back" {
			return fmt.Errorf("cancelled")
		}
		if input == "" {
			// Confirm selection
			break
		}
		if input == "a" {
			allSelected := true
			for _, f := range files {
				if !f.selected {
					allSelected = false
					break
				}
			}
			for i := range files {
				files[i].selected = !allSelected
			}
			printFileList()
			continue
		}

		var idx int
		if _, err := fmt.Sscanf(input, "%d", &idx); err == nil && idx >= 1 && idx <= len(files) {
			files[idx-1].selected = !files[idx-1].selected
			check := "[ ]"
			if files[idx-1].selected {
				check = "[x]"
			}
			fmt.Printf("    %2d. %s %s\n", idx, check, files[idx-1].name)
			continue
		}
		fmt.Printf("  Enter a number (1-%d), 'a' for all, or Enter to confirm.\n", len(files))
	}

	// Copy selected files
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	count := 0
	for _, f := range files {
		if !f.selected {
			continue
		}
		src := filepath.Join(srcDir, f.name)
		dst := filepath.Join(destDir, f.name)
		data, err := os.ReadFile(src)
		if err != nil {
			fmt.Printf("  Warning: could not read %s: %v\n", f.name, err)
			continue
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			fmt.Printf("  Warning: could not write %s: %v\n", f.name, err)
			continue
		}
		count++
	}
	fmt.Printf("✓ Imported %d files to %s/\n", count, destDir)
	return nil
}

// min returns the smaller of a and b. (Go 1.21+ has builtin min but we support earlier.)
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
