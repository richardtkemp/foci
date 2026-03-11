package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/provision"
	"foci/internal/secrets"

	_ "foci/internal/telegram" // register telegram messaging provider
)

// setupFlags holds parsed flags for the setup command.
type setupFlags struct {
	configDir      string // directory for foci.toml and secrets.toml
	homeDir        string // foci user home (workspace parent)
	nonInteractive bool
	agentID        string
	displayName    string // display name for agent
	model          string // model alias or full ID
	setupToken     string // setup token from 'claude setup-token'
	apiKey         string // API key (auth credential)
	// Provider-contributed flags are stored here by name.
	providerFlags map[string]string
}

// setupState tracks wizard state for back navigation.
type setupState struct {
	authMethod  string // "setup-token", "apikey", "skip"
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
  --agent-id <id>        Agent identifier (default: main)
  --display-name <name>  Display name for agent (default: titlecased agent ID)
  --model <model>        Model alias or full ID: opus, sonnet, haiku (default: sonnet)
  --setup-token <token>  Setup token from 'claude setup-token'
  --api-key <key>        API key (direct Anthropic API key)
`)
	// Print provider-contributed flags
	for _, w := range platform.SetupProviders() {
		for _, f := range w.SetupFlags() {
			req := ""
			if f.Required {
				req = " (required for non-interactive)"
			}
			fmt.Fprintf(os.Stderr, "  --%-20s %s%s\n", f.Name+" <value>", f.Usage, req)
		}
	}
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
	f.providerFlags = make(map[string]string)

	// Collect all known provider flag names for dynamic parsing.
	providerFlagNames := map[string]bool{}
	for _, w := range platform.SetupProviders() {
		for _, pf := range w.SetupFlags() {
			providerFlagNames[pf.Name] = true
		}
	}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config-dir":
			if i+1 < len(args) {
				f.configDir = args[i+1]
				i++
			}
		case "--non-interactive":
			f.nonInteractive = true
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
		default:
			// Check if this is a provider-contributed flag (--bot-token, --user-id, etc.)
			if strings.HasPrefix(args[i], "--") && i+1 < len(args) {
				name := strings.TrimPrefix(args[i], "--")
				if providerFlagNames[name] {
					f.providerFlags[name] = args[i+1]
					i++
				}
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
	// Resolve model
	model := provision.ResolveModelAlias(f.model)

	// Resolve display name
	displayName := f.displayName
	if displayName == "" {
		displayName = provision.TitleCase(f.agentID)
	}

	secretsPath := filepath.Join(f.configDir, "secrets.toml")
	store, err := secrets.Load(secretsPath)
	if err != nil {
		return fmt.Errorf("load secrets: %w", err)
	}

	secretsOpts := config.SecretsOptions{}

	// Auth: setup-token takes precedence, then api-key, then skip.
	if f.setupToken != "" {
		store.Set("anthropic.setup_token", f.setupToken)
		secretsOpts.SetupToken = f.setupToken
	} else if f.apiKey != "" {
		store.Set("anthropic.api_key", f.apiKey)
		secretsOpts.SetupToken = f.apiKey
	}

	// Run provider setup (non-interactive)
	// Pass agent-id through so providers can use it for secret naming.
	provFlags := copyMap(f.providerFlags)
	provFlags["agent-id"] = f.agentID

	var providerConfigFragments []string
	providerSecrets := map[string]string{}
	for _, w := range platform.SetupProviders() {
		result, err := w.RunSetup(nil, provFlags, true)
		if err != nil {
			return err
		}
		if result.ConfigTOML != "" {
			providerConfigFragments = append(providerConfigFragments, result.ConfigTOML)
		}
		for k, v := range result.Secrets {
			providerSecrets[k] = v
		}
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
		AgentBlock: result.ConfigBlock,
		Model:      model,
	}

	return writeSetupFiles(f, configOpts, secretsOpts, store, result, providerConfigFragments, providerSecrets)
}

func runSetupInteractive(f setupFlags) error {
	reader := bufio.NewReader(os.Stdin)
	state := setupState{agentID: f.agentID}
	ui := &consoleUI{reader: reader}

	fmt.Println()
	fmt.Println("──────────────────────────────────────────")
	fmt.Println("  Foci First-Run Setup")
	fmt.Println("  (Enter 'back' at any prompt to return to the previous step)")
	fmt.Println("──────────────────────────────────────────")

	// Load secrets store early — needed by RunSetupTokenFlow in step 2.
	secretsPath := filepath.Join(f.configDir, "secrets.toml")
	store, err := secrets.Load(secretsPath)
	if err != nil {
		return fmt.Errorf("load secrets: %w", err)
	}

	secretsOpts := config.SecretsOptions{}

	// Generic steps
	totalSteps := 7
	step := 1
	for step <= totalSteps {
		switch step {
		case 1:
			method, back := stepAuth(reader, state.authMethod, totalSteps)
			if back {
				fmt.Println("  Already at the first step.")
				continue
			}
			state.authMethod = method
			step++

		case 2:
			back, err := stepCredential(reader, state.authMethod, store, &secretsOpts, totalSteps)
			if err != nil {
				return err
			}
			if back {
				step--
				continue
			}
			step++

		case 3:
			agentID, back := stepAgentID(reader, state.agentID, totalSteps)
			if back {
				step--
				continue
			}
			state.agentID = agentID
			step++

		case 4:
			displayName, back := stepDisplayName(reader, state.agentID, state.displayName, totalSteps)
			if back {
				step--
				continue
			}
			state.displayName = displayName
			step++

		case 5:
			model, back := stepModel(reader, state.model, store, totalSteps)
			if back {
				step--
				continue
			}
			state.model = model
			step++

		case 6:
			charMode, back := stepCharacterMode(reader, f, totalSteps)
			if back {
				step--
				continue
			}
			state.charMode = charMode
			step++

		case 7:
			// Platform provider steps (e.g. telegram bot token + user ID)
			provFlags := map[string]string{"agent-id": state.agentID}
			providerConfigFragments, providerSecrets, back, err := runProviderSetups(ui, provFlags, totalSteps)
			if err != nil {
				return err
			}
			if back {
				step--
				continue
			}

			// We have everything — finalize.
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
				AgentBlock: provResult.ConfigBlock,
				Model:      state.model,
			}

			fmt.Println()
			fmt.Println("Creating config...")

			if err := writeSetupFiles(f, configOpts, secretsOpts, store, provResult, providerConfigFragments, providerSecrets); err != nil {
				return err
			}

			fmt.Println("  Setup complete.")
			fmt.Println()
			fmt.Println("──────────────────────────────────────────")
			step++
		}
	}

	return nil
}

// writeSetupFiles writes foci.toml, secrets.toml, and ensures workspace directories exist.
func writeSetupFiles(f setupFlags, configOpts config.SetupOptions, secretsOpts config.SecretsOptions, store *secrets.Store, provResult *provision.Result, providerConfigFragments []string, providerSecrets map[string]string) error {
	// Ensure config directory exists
	if err := os.MkdirAll(f.configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	// Write foci.toml — generic config + provider fragments
	configPath := filepath.Join(f.configDir, "foci.toml")
	configContent := config.GenerateConfig(configOpts)
	for _, fragment := range providerConfigFragments {
		configContent += fragment
		if !strings.HasSuffix(fragment, "\n") {
			configContent += "\n"
		}
	}
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		return fmt.Errorf("write foci.toml: %w", err)
	}
	fmt.Printf("  → %s\n", configPath)

	// Write secrets via the store (so it handles formatting + existing values)
	if secretsOpts.SetupToken != "" {
		store.Set("anthropic.setup_token", secretsOpts.SetupToken)
	}
	// Store provider-contributed secrets
	// Sort keys for deterministic output.
	provSecretKeys := make([]string, 0, len(providerSecrets))
	for k := range providerSecrets {
		provSecretKeys = append(provSecretKeys, k)
	}
	sort.Strings(provSecretKeys)
	for _, k := range provSecretKeys {
		store.Set(k, providerSecrets[k])
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
			fmt.Printf("  Could not install crontab entries automatically.\n")
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
