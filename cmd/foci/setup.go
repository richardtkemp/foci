package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"foci/anthropic"
	"foci/config"
	"foci/secrets"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// knownCharacterFiles are filenames pre-selected during character file import.
var knownCharacterFiles = map[string]bool{
	"SOUL.md":      true,
	"CRAFT.md":     true,
	"COHERENCE.md": true,
	"USER.md":      true,
	"MEMORY.md":    true,
}

// defaultSystemFiles is the list written to system_files in generated config.
var defaultSystemFiles = []string{
	"character/SOUL.md",
	"character/CRAFT.md",
	"character/COHERENCE.md",
	"character/USER.md",
	"character/MEMORY.md",
}

// setupFlags holds parsed flags for the setup command.
type setupFlags struct {
	configDir      string // directory for foci.toml and secrets.toml
	homeDir        string // foci user home (workspace parent)
	defaultsDir    string // path to shared/defaults/character/
	nonInteractive bool
	botToken       string
	userID         string
	agentID        string
	authMethod     string // "oauth", "apikey", "skip"
	authToken      string // API key (for authMethod=apikey)
}

// setupState tracks wizard state for back navigation.
type setupState struct {
	botToken   string
	authMethod string // "setup-token", "apikey", "skip"
	userID     string
	agentID    string
}

func setupUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci setup [flags]

Interactive setup wizard for first-run configuration.
Generates foci.toml, secrets.toml, and seeds character files.

Flags:
  --config-dir <path>    Directory for config files (default: ./config)
  --home <path>          Foci home directory (default: current user home)
  --defaults-dir <path>  Path to default character templates
  --non-interactive      Non-interactive mode (all required flags must be set)
  --bot-token <token>    Telegram bot token
  --user-id <id>         Telegram user ID
  --agent-id <id>        Agent identifier (default: main)
  --auth-method <m>      Auth method: setup-token, apikey, skip
  --auth-token <token>   API key (for --auth-method=apikey)
`)
}

func cmdSetup(args []string) error {
	if wantsHelp(args) {
		setupUsage()
		return nil
	}

	flags := parseSetupFlags(args)

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
		case "--home":
			if i+1 < len(args) {
				f.homeDir = args[i+1]
				i++
			}
		case "--defaults-dir":
			if i+1 < len(args) {
				f.defaultsDir = args[i+1]
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
		case "--auth-method":
			if i+1 < len(args) {
				f.authMethod = args[i+1]
				i++
			}
		case "--auth-token":
			if i+1 < len(args) {
				f.authToken = args[i+1]
				i++
			}
		}
	}

	// Apply defaults
	if f.configDir == "" {
		f.configDir = "./config"
	}
	if f.homeDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
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

func runSetupNonInteractive(f setupFlags) error {
	var missing []string
	if f.botToken == "" {
		missing = append(missing, "--bot-token")
	}
	if f.userID == "" {
		missing = append(missing, "--user-id")
	}
	if f.authMethod == "" {
		missing = append(missing, "--auth-method")
	}
	if f.authMethod == "apikey" && f.authToken == "" {
		missing = append(missing, "--auth-token (required for --auth-method=apikey)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("non-interactive mode requires: %s", strings.Join(missing, ", "))
	}

	if !isValidBotToken(f.botToken) {
		return fmt.Errorf("invalid bot token format")
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

	switch f.authMethod {
	case "setup-token":
		if err := anthropic.RunSetupTokenFlow(store); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
		// Token saved to store; read back for config gen
		if v, ok := store.Get("anthropic.setup_token"); ok {
			secretsOpts.SetupToken = v
		}
	case "apikey":
		secretsOpts.SetupToken = f.authToken
	case "skip":
		// No auth
	}

	model := "claude-sonnet-4-6"

	configOpts := config.SetupOptions{
		AgentID:      f.agentID,
		Model:        model,
		SystemFiles:  defaultSystemFiles,
		AllowedUsers: []string{f.userID},
	}

	return writeSetupFiles(f, configOpts, secretsOpts, store)
}

func runSetupInteractive(f setupFlags) error {
	reader := bufio.NewReader(os.Stdin)
	state := setupState{agentID: f.agentID}

	fmt.Println()
	fmt.Println("──────────────────────────────────────────")
	fmt.Println("  Foci First-Run Setup")
	fmt.Println("  (Enter 'back' at any prompt to return to the previous step)")
	fmt.Println("──────────────────────────────────────────")

	step := 1
	for step <= 5 {
		switch step {
		case 1:
			result, back := stepBotToken(reader, state.botToken)
			if back {
				fmt.Println("  Already at the first step.")
				continue
			}
			state.botToken = result
			step++

		case 2:
			method, back := stepAuth(reader, state.authMethod)
			if back {
				step--
				continue
			}
			state.authMethod = method
			step++

		case 3:
			userID, back := stepUserID(reader, state.botToken, state.userID)
			if back {
				step--
				continue
			}
			state.userID = userID
			step++

		case 4:
			agentID, back := stepAgentID(reader, state.agentID)
			if back {
				step--
				continue
			}
			state.agentID = agentID
			step++

		case 5:
			back := stepCharacterFiles(reader, f, state.agentID)
			if back {
				step--
				continue
			}
			step++
		}
	}

	// Run auth flow
	secretsPath := filepath.Join(f.configDir, "secrets.toml")
	store, err := secrets.Load(secretsPath)
	if err != nil {
		return fmt.Errorf("load secrets: %w", err)
	}

	secretsOpts := config.SecretsOptions{
		AgentID:  state.agentID,
		BotToken: state.botToken,
	}

	switch state.authMethod {
	case "setup-token":
		fmt.Println()
		if err := anthropic.RunSetupTokenFlow(store); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
		fmt.Println("✓ Setup token saved.")
		if v, ok := store.Get("anthropic.setup_token"); ok {
			secretsOpts.SetupToken = v
		}
	case "apikey":
		fmt.Print("\n  API key: ")
		key, _ := reader.ReadString('\n')
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("API key cannot be empty")
		}
		secretsOpts.SetupToken = key
		fmt.Println("✓ API key saved.")
	case "skip":
		fmt.Println("✓ Skipping auth (will use Claude Code credentials if available).")
	}

	// Discover model
	model := discoverModel(store)

	configOpts := config.SetupOptions{
		AgentID:      state.agentID,
		Model:        model,
		SystemFiles:  defaultSystemFiles,
		AllowedUsers: []string{state.userID},
	}

	fmt.Println()
	fmt.Println("Creating config...")
	if err := writeSetupFiles(f, configOpts, secretsOpts, store); err != nil {
		return err
	}

	fmt.Println("✓ Setup complete.")
	fmt.Println()
	fmt.Println("──────────────────────────────────────────")
	return nil
}

// stepBotToken prompts for a Telegram bot token.
func stepBotToken(reader *bufio.Reader, current string) (token string, back bool) {
	fmt.Println()
	fmt.Println("Step 1/5: Telegram Bot")
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
		if isValidBotToken(input) {
			fmt.Println("✓ Bot token validated.")
			return input, false
		}
		fmt.Println("  Invalid token format. Expected: 123456789:AAF-... (get it from @BotFather)")
	}
}

// stepAuth prompts for authentication method.
func stepAuth(reader *bufio.Reader, current string) (method string, back bool) {
	fmt.Println()
	fmt.Println("Step 2/5: Anthropic Authentication")
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

// stepUserID prompts for user ID via auto-detect or manual entry.
func stepUserID(reader *bufio.Reader, botToken string, current string) (userID string, back bool) {
	fmt.Println()
	fmt.Println("Step 3/5: Your Telegram User ID")
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
		if isValidUserID(input) {
			fmt.Printf("✓ User ID: %s\n", input)
			return input, false
		}
		fmt.Println("  Invalid user ID. Expected a numeric ID (e.g. 12345678).")
	}
}

// stepAgentID prompts for an agent identifier.
func stepAgentID(reader *bufio.Reader, current string) (agentID string, back bool) {
	fmt.Println()
	fmt.Println("Step 4/5: Agent ID")
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
		if isValidAgentID(input) {
			fmt.Printf("✓ Agent ID: %s\n", input)
			return input, false
		}
		fmt.Println("  Invalid ID. Use lowercase letters, numbers, and hyphens only.")
	}
}

// stepCharacterFiles handles character file seeding or import.
// Returns true if the user wants to go back.
func stepCharacterFiles(reader *bufio.Reader, f setupFlags, agentID string) bool {
	fmt.Println()
	fmt.Println("Step 5/5: Character Files")
	fmt.Println("  Do you have existing character files to import?")
	fmt.Println("  [1] No — use defaults (recommended for new users)")
	fmt.Println("  [2] Yes — import from a directory")
	fmt.Println()

	for {
		fmt.Print("> ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		if input == "back" {
			return true
		}

		switch input {
		case "1":
			destDir := filepath.Join(f.homeDir, agentID, "character")
			if err := seedDefaultCharacterFiles(f.defaultsDir, destDir); err != nil {
				fmt.Printf("  Warning: could not seed defaults: %v\n", err)
			} else {
				fmt.Printf("✓ Seeded default character files to %s/\n", destDir)
			}
			return false

		case "2":
			fmt.Print("  Directory path: ")
			dirInput, _ := reader.ReadString('\n')
			dirInput = strings.TrimSpace(dirInput)
			if dirInput == "back" {
				return true
			}

			destDir := filepath.Join(f.homeDir, agentID, "character")
			if err := importCharacterFiles(reader, dirInput, destDir); err != nil {
				fmt.Printf("  Error: %v\n", err)
				continue
			}
			return false

		default:
			fmt.Println("  Enter 1 or 2.")
		}
	}
}

// seedDefaultCharacterFiles copies templates from defaultsDir to destDir.
func seedDefaultCharacterFiles(defaultsDir, destDir string) error {
	if defaultsDir == "" {
		return fmt.Errorf("no defaults directory specified (use --defaults-dir)")
	}

	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	entries, err := os.ReadDir(defaultsDir)
	if err != nil {
		return fmt.Errorf("read defaults dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		src := filepath.Join(defaultsDir, entry.Name())
		dst := filepath.Join(destDir, entry.Name())

		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			return fmt.Errorf("write %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// importCharacterFiles lists .md files from srcDir and lets the user select which to import.
func importCharacterFiles(reader *bufio.Reader, srcDir, destDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("read directory %s: %w", srcDir, err)
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

// writeSetupFiles writes foci.toml, secrets.toml, and creates the workspace directory.
func writeSetupFiles(f setupFlags, configOpts config.SetupOptions, secretsOpts config.SecretsOptions, store *secrets.Store) error {
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
		store.Set(fmt.Sprintf("telegram.bots.%s.token", secretsOpts.AgentID), secretsOpts.BotToken)
	}
	if secretsOpts.SetupToken != "" {
		store.Set("anthropic.setup_token", secretsOpts.SetupToken)
	}
	// Setup token is already in store from RunSetupTokenFlow
	if err := store.Save(); err != nil {
		return fmt.Errorf("write secrets.toml: %w", err)
	}
	fmt.Printf("  → %s\n", filepath.Join(f.configDir, "secrets.toml"))

	// Create workspace directories
	workspaceDir := filepath.Join(f.homeDir, configOpts.AgentID)
	charDir := filepath.Join(workspaceDir, "character")
	memDir := filepath.Join(workspaceDir, "memory")
	for _, dir := range []string{charDir, memDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	fmt.Printf("  → %s/\n", charDir)

	return nil
}

// discoverModel queries the Anthropic /v1/models API to find the latest Sonnet.
// Falls back to "claude-sonnet-4-6" on failure or timeout.
func discoverModel(store *secrets.Store) string {
	fallback := "claude-sonnet-4-6"

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

	fmt.Print("  Querying Anthropic API for latest Sonnet model... ")

	client := anthropic.NewClientWithTimeout(token, 5*time.Second)
	models, err := client.ListModels()
	if err != nil {
		fmt.Printf("(using default: %s)\n", fallback)
		return fallback
	}

	// Find the latest sonnet model
	var bestID string
	var bestTime time.Time
	for _, m := range models {
		if !strings.Contains(strings.ToLower(m.ID), "sonnet") {
			continue
		}
		if m.CreatedAt.After(bestTime) {
			bestTime = m.CreatedAt
			bestID = m.ID
		}
	}

	if bestID == "" {
		fmt.Printf("(no sonnet found, using default: %s)\n", fallback)
		return fallback
	}

	fmt.Printf("✓ %s\n", bestID)
	return bestID
}

// isValidBotToken checks if a string looks like a Telegram bot token.
var botTokenRe = regexp.MustCompile(`^\d{5,}:[A-Za-z0-9_-]{20,}$`)

func isValidBotToken(token string) bool {
	return botTokenRe.MatchString(token)
}

// isValidUserID checks if a string is a numeric Telegram user ID.
var userIDRe = regexp.MustCompile(`^\d{3,}$`)

func isValidUserID(id string) bool {
	return userIDRe.MatchString(id)
}

// isValidAgentID checks if a string is a valid agent identifier.
var agentIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func isValidAgentID(id string) bool {
	return agentIDRe.MatchString(id)
}

// min returns the smaller of a and b. (Go 1.21+ has builtin min but we support earlier.)
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

