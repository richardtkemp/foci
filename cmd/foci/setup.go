package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/provision"
	"foci/internal/secrets"
	"foci/shared"

	_ "foci/internal/telegram" // register telegram messaging provider
)

// setupFlags holds parsed flags for the setup command.
type setupFlags struct {
	configDir       string // directory for foci.toml and secrets.toml
	homeDir         string // foci user home (workspace parent)
	nonInteractive  bool
	agentID         string
	displayName     string // display name for agent
	provider        string // LLM provider: "anthropic", "gemini", "openai", "openrouter"
	apiKey          string // API key for the chosen provider
	model           string // model alias or full ID
	charMode        string // "defaults", "openclaw", "import", "blank"
	charImportDir   string // source directory when charMode=="import"
	memoryImportDir string // directory to import memory .md files from
	// Provider-contributed flags are stored here by name.
	providerFlags map[string]string
}

// setupState tracks wizard state for back navigation.
type setupState struct {
	provider        string // "anthropic", "gemini", "openai", "openrouter", "custom"
	agentID         string
	displayName     string
	model           string
	charMode        string // "defaults", "openclaw", "import"
	importDir       string // source directory when charMode=="import"
	memoryImportDir string // source directory for memory file import
	customEndpoint  *config.CustomEndpointSetup
}

func setupUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci first-run [flags]

Interactive setup wizard for first-run configuration.
Generates foci.toml, secrets.toml, and seeds character files.

Flags:
  -h, --help             Show this help
  --config-dir <path>    Directory for config files (default: ~/config)
  --non-interactive      Non-interactive mode (all required flags must be set)
  --agent-id <id>        Agent identifier (default: main)
  --display-name <name>  Display name for agent (default: titlecased agent ID)
  --provider <name>      LLM backend: anthropic, gemini, openai, openrouter (API providers),
                         or claude-code (local backend — uses your Claude login, no API key)
                         (default: interactive prompt)
  --api-key <key>        API key for the chosen provider (not needed for claude-code)
  --model <model>        Model alias or full ID: opus, sonnet, haiku, or developer/model_id (default: sonnet)
  --char-mode <mode>     Character mode: defaults, openclaw, import, blank (default: defaults)
  --char-import-dir <path>  Directory to import character .md files from (requires --char-mode import)
  --memory-import-dir <path>  Directory to import memory .md files from
`)
	// Print provider-contributed flags
	for _, nw := range platform.SetupProviders() {
		for _, f := range nw.Wizard.SetupFlags() {
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

	// Warn if config already exists — first-run is not designed for reconfiguration.
	configPath := filepath.Join(flags.configDir, "foci.toml")
	if _, err := os.Stat(configPath); err == nil {
		if flags.nonInteractive {
			fmt.Fprintf(os.Stderr, "Warning: %s already exists. It will be backed up before overwriting.\n", configPath)
		} else {
			fmt.Println()
			fmt.Printf("  Warning: %s already exists.\n", configPath)
			fmt.Println("  Running first-run will overwrite it (a backup will be created).")
			fmt.Println()
			fmt.Print("  Continue? [y/N] ")
			reader := bufio.NewReader(os.Stdin)
			line, _ := reader.ReadString('\n')
			line = strings.TrimSpace(strings.ToLower(line))
			if line != "y" && line != "yes" {
				fmt.Println("  Aborted.")
				return nil
			}
		}
	}

	// Seed shared/ from repo to disk if available, then fill gaps from embedded defaults.
	targetSharedDir := filepath.Join(flags.homeDir, "shared")
	if repoSharedDir := findRepoShared(); repoSharedDir != "" {
		if err := provision.SeedDefaults(os.DirFS(repoSharedDir), targetSharedDir, 0640); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not seed defaults from repo: %v\n", err)
		}
	}
	// Always seed from embedded defaults (no-ops for files already on disk)
	if err := provision.SeedDefaults(shared.DefaultsFS, targetSharedDir, 0640); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not seed embedded defaults: %v\n", err)
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
	for _, nw := range platform.SetupProviders() {
		for _, pf := range nw.Wizard.SetupFlags() {
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
		case "--provider":
			if i+1 < len(args) {
				f.provider = args[i+1]
				i++
			}
		case "--api-key":
			if i+1 < len(args) {
				f.apiKey = args[i+1]
				i++
			}
		case "--model":
			if i+1 < len(args) {
				f.model = args[i+1]
				i++
			}
		case "--char-mode":
			if i+1 < len(args) {
				f.charMode = args[i+1]
				i++
			}
		case "--char-import-dir":
			if i+1 < len(args) {
				f.charImportDir = args[i+1]
				i++
			}
		case "--memory-import-dir":
			if i+1 < len(args) {
				f.memoryImportDir = args[i+1]
				i++
			}
		default:
			// Check if this is a provider-contributed flag (--telegram-bot-token, etc.)
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
	if f.charMode == "" {
		if f.charImportDir != "" {
			f.charMode = "import"
		} else {
			f.charMode = "defaults"
		}
	}
	if f.agentID == "" {
		f.agentID = "main"
	}

	return f
}

// findRepoShared tries to locate the shared/ directory relative to
// the running binary or the current working directory.
func findRepoShared() string {
	// Try relative to the executable
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "shared")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	// Try current working directory
	if cwd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(cwd, "shared")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}

func runSetupNonInteractive(f setupFlags) error {
	// Determine provider (default to anthropic for backward compat with --model aliases)
	provider := f.provider
	if provider == "" {
		provider = "anthropic"
	}
	prov := providerByKey(provider)
	if prov == nil && provider != "custom" {
		return fmt.Errorf("unknown provider %q; use: anthropic, gemini, openai, openrouter, claude-code", provider)
	}

	// Resolve model
	model := f.model
	if model == "" && prov != nil {
		model = prov.DefaultModel
	}
	switch {
	case prov != nil && prov.IsLocalBackend():
		// Local backend (claude-code): the alias is passed through verbatim to
		// the host tool, which resolves it. No API alias resolution.
	case prov != nil && prov.HasAliases:
		model = provision.ResolveModelAlias(model)
	case model != "" && !strings.Contains(model, "/"):
		// Non-Anthropic: aliases don't apply, but still resolve if it happens to be one
		model = provision.ResolveModelAlias(model)
	}

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

	// Store API key under the provider's secret key
	if f.apiKey != "" && prov != nil {
		store.Set(prov.SecretKey, f.apiKey)
	}

	// Run provider setup (non-interactive)
	// Pass agent-id through so providers can use it for secret naming.
	provFlags := copyMap(f.providerFlags)
	provFlags["agent-id"] = f.agentID

	var providerConfigFragments []string
	providerSecrets := map[string]string{}
	for _, nw := range platform.SetupProviders() {
		// Skip providers whose flags aren't present.
		hasFlag := false
		for _, sf := range nw.Wizard.SetupFlags() {
			if _, ok := provFlags[sf.Name]; ok {
				hasFlag = true
				break
			}
		}
		if !hasFlag {
			continue
		}

		result, err := nw.Wizard.RunSetup(nil, provFlags, true)
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

	// Validate char-import-dir requirement
	if f.charMode == "import" && f.charImportDir == "" {
		return fmt.Errorf("--char-import-dir is required when --char-mode is import")
	}

	// Provision the agent workspace
	defaultsDir := filepath.Join(f.homeDir, "shared")
	spec := provision.AgentSpec{
		ID:          f.agentID,
		DisplayName: displayName,
		HomeDir:     f.homeDir,
		DefaultsDir: defaultsDir,
		CharMode:    f.charMode,
	}
	// Local backends carry their model in backend_config.model (set on the
	// agent block by provision), not in a shared [models.default] group.
	if prov != nil && prov.IsLocalBackend() {
		spec.Backend = prov.Backend
		spec.Model = model
	}

	result, err := provision.Provision(spec)
	if err != nil {
		return fmt.Errorf("provision agent: %w", err)
	}

	// Copy character files if --char-mode import
	if f.charMode == "import" {
		charDir := filepath.Join(result.Workspace, "character")
		if err := copyMDFiles(f.charImportDir, charDir); err != nil {
			return fmt.Errorf("import character files: %w", err)
		}
	}

	// Copy memory files if --memory-import-dir was specified
	if f.memoryImportDir != "" {
		memDir := filepath.Join(result.Workspace, "memory")
		if err := copyMDFiles(f.memoryImportDir, memDir); err != nil {
			return fmt.Errorf("import memory files: %w", err)
		}
	}

	endpoint := ""
	if prov != nil {
		endpoint = prov.Endpoint
	}
	configOpts := config.SetupOptions{
		AgentBlock: result.ConfigBlock,
		Model:      model,
		Endpoint:   endpoint,
	}
	// Local backends (claude-code, etc.) route EVERYTHING through the host tool —
	// agent turns AND foci's auxiliary calls (compaction → /compact, summaries →
	// CLISummariser, memory → backend turn). They never touch the model groups,
	// so we write NO [groups]/[models.default]/[endpoints.anthropic] at all.
	// Emitting an anthropic group here only caused a spurious startup "no
	// Anthropic credentials" error and a missing-secret warning on keyless
	// (login-only) deployments. The backend's model lives in backend_config.
	if prov != nil && prov.IsLocalBackend() {
		configOpts.Model = ""
		configOpts.Endpoint = ""
	}

	return writeSetupFiles(f, configOpts, store, result, providerConfigFragments, providerSecrets)
}

// copyMDFiles copies all .md files from srcDir to destDir (non-interactive).
func copyMDFiles(srcDir, destDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("read directory %s: %w", srcDir, err)
	}

	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		src := filepath.Join(srcDir, entry.Name())
		dst := filepath.Join(destDir, entry.Name())
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		if err := os.WriteFile(dst, data, 0640); err != nil {
			return fmt.Errorf("write %s: %w", entry.Name(), err)
		}
		count++
	}
	fmt.Printf("  Imported %d memory files to %s/\n", count, destDir)
	return nil
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

	// Load secrets store early — needed by API key storage and model discovery.
	secretsPath := filepath.Join(f.configDir, "secrets.toml")
	store, err := secrets.Load(secretsPath)
	if err != nil {
		return fmt.Errorf("load secrets: %w", err)
	}

	// Generic steps
	totalSteps := 8
	step := 1
	for step <= totalSteps {
		switch step {
		case 1:
			providerKey, back := stepProvider(reader, state.provider, totalSteps)
			if back {
				fmt.Println("  Already at the first step.")
				continue
			}
			state.provider = providerKey
			step++

		case 2:
			if state.provider == "custom" {
				ce, back, err := stepCustomEndpoint(reader, store, totalSteps)
				if err != nil {
					return err
				}
				if back {
					step--
					continue
				}
				state.customEndpoint = ce
			} else {
				back := stepAPIKey(reader, state.provider, store, totalSteps)
				if back {
					step--
					continue
				}
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
			model, back := stepModel(reader, state.model, state.provider, store, totalSteps)
			if back {
				step--
				continue
			}
			state.model = model
			step++

		case 6:
			charMode, importDir, back := stepCharacterMode(reader, f, totalSteps)
			if back {
				step--
				continue
			}
			state.charMode = charMode
			state.importDir = importDir
			step++

		case 7:
			memoryDir, back := stepMemoryImport(reader, state.importDir, totalSteps)
			if back {
				step--
				continue
			}
			state.memoryImportDir = memoryDir
			step++

		case 8:
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
			defaultsDir := filepath.Join(f.homeDir, "shared")
			spec := provision.AgentSpec{
				ID:          state.agentID,
				DisplayName: state.displayName,
				HomeDir:     f.homeDir,
				DefaultsDir: defaultsDir,
				CharMode:    state.charMode,
			}
			prov := providerByKey(state.provider)
			// Local backends carry their model in backend_config.model on the
			// agent block, not in a shared [models.default] group.
			if prov != nil && prov.IsLocalBackend() {
				spec.Backend = prov.Backend
				spec.Model = state.model
			}

			provResult, err := provision.Provision(spec)
			if err != nil {
				return fmt.Errorf("provision agent: %w", err)
			}

			if state.charMode == "import" {
				charDir := filepath.Join(provResult.Workspace, "character")
				if err := importCharacterFiles(reader, state.importDir, charDir); err != nil {
					return fmt.Errorf("import character files: %w", err)
				}
			}

			if state.memoryImportDir != "" {
				memDir := filepath.Join(provResult.Workspace, "memory")
				if err := importMemoryFiles(reader, state.memoryImportDir, memDir); err != nil {
					return fmt.Errorf("import memory files: %w", err)
				}
			}

			// Determine endpoint override from provider
			endpoint := ""
			if prov != nil {
				endpoint = prov.Endpoint
			}
			if state.customEndpoint != nil {
				endpoint = state.customEndpoint.Name
			}

			configOpts := config.SetupOptions{
				AgentBlock:     provResult.ConfigBlock,
				Model:          state.model,
				Endpoint:       endpoint,
				CustomEndpoint: state.customEndpoint,
			}
			// Local backends route everything through the host tool (turns +
			// foci's auxiliary calls) and never touch the model groups, so we
			// write NO [groups]/[models.default]/[endpoints.anthropic]. See the
			// non-interactive path above for the full rationale. The backend's
			// model lives in backend_config.
			if prov != nil && prov.IsLocalBackend() {
				configOpts.Model = ""
				configOpts.Endpoint = ""
			}

			fmt.Println()
			fmt.Println("Creating config...")

			if err := writeSetupFiles(f, configOpts, store, provResult, providerConfigFragments, providerSecrets); err != nil {
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

// backupIfExists renames path to path.old.<timestamp> if it exists.
// Returns the backup path (for display) or "" if no backup was needed.
func backupIfExists(path string) (string, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "", nil
	}
	ts := time.Now().Format("20060102-150405")
	backup := path + ".old." + ts
	if err := os.Rename(path, backup); err != nil {
		return "", fmt.Errorf("backup %s: %w", path, err)
	}
	return backup, nil
}

// writeSetupFiles writes foci.toml, secrets.toml, and ensures workspace directories exist.
// Existing files are backed up to *.old.<timestamp> before overwriting.
func writeSetupFiles(f setupFlags, configOpts config.SetupOptions, store *secrets.Store, provResult *provision.Result, providerConfigFragments []string, providerSecrets map[string]string) error {
	// Ensure config directory exists
	if err := os.MkdirAll(f.configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	// Write foci.toml — generic config + provider fragments
	configPath := filepath.Join(f.configDir, "foci.toml")

	if backup, err := backupIfExists(configPath); err != nil {
		return err
	} else if backup != "" {
		fmt.Printf("  → backed up %s\n", backup)
	}

	configContent := config.GenerateConfig(configOpts)
	for _, fragment := range providerConfigFragments {
		configContent += fragment
		if !strings.HasSuffix(fragment, "\n") {
			configContent += "\n"
		}
	}
	if err := os.WriteFile(configPath, []byte(configContent), 0640); err != nil {
		return fmt.Errorf("write foci.toml: %w", err)
	}
	fmt.Printf("  → %s\n", configPath)

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
