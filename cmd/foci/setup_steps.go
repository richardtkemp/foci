package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/provision"
	"foci/internal/secrets"
)

// llmProviderInfo describes an LLM provider for the setup wizard.
type llmProviderInfo struct {
	Name         string // display name: "Anthropic (Claude)"
	Key          string // selection key: "anthropic", "gemini", etc.
	SecretKey    string // secrets.toml key: "anthropic.api_key"
	DefaultModel string // default model: "anthropic/claude-sonnet-4-6"
	Endpoint     string // endpoint override (empty = auto-detect from developer)
	HasAliases   bool   // opus/sonnet/haiku aliases work
	HasDiscovery bool   // Anthropic API model discovery works
	// Backend names a delegated foci backend (e.g. "claude-code"). Empty means
	// the standard API backend, which talks to a remote endpoint with an API
	// key. A non-empty Backend is a *local* backend: it shells out to a tool on
	// the host (the `claude` CLI for "claude-code") and needs no API key — auth
	// is the host's own Claude Code OAuth login, exercised via /login.
	Backend string
}

// IsLocalBackend reports whether the provider is a delegated local backend
// (no API key; auth via the host tool's own login) rather than a remote API.
func (p *llmProviderInfo) IsLocalBackend() bool { return p.Backend != "" }

var llmProviders = []llmProviderInfo{
	{Name: "Anthropic (Claude)", Key: "anthropic", SecretKey: "anthropic.api_key", DefaultModel: "anthropic/claude-sonnet-4-6", HasAliases: true, HasDiscovery: true},
	{Name: "Google Gemini", Key: "gemini", SecretKey: "gemini.api_key", DefaultModel: "google/gemini-2.5-flash"},
	{Name: "OpenAI", Key: "openai", SecretKey: "openai.api_key", DefaultModel: "openai/gpt-4o"},
	{Name: "OpenRouter (multi-provider)", Key: "openrouter", SecretKey: "openrouter.api_key", DefaultModel: "anthropic/claude-sonnet-4-6", Endpoint: "openrouter"},
	{Name: "Custom endpoint", Key: "custom"},
	{Name: "Claude Code (local, uses your Claude login — no API key)", Key: "claude-code", DefaultModel: "sonnet", HasAliases: true, Backend: "claude-code"},
}

// providerByKey returns the provider info for a key, or nil if not found.
func providerByKey(key string) *llmProviderInfo {
	for i := range llmProviders {
		if llmProviders[i].Key == key {
			return &llmProviders[i]
		}
	}
	return nil
}

// stepProvider prompts for LLM provider selection.
func stepProvider(reader *bufio.Reader, _ string, total int) (providerKey string, back bool) {
	fmt.Println()
	fmt.Printf("Step 1/%d: LLM Provider\n", total)
	fmt.Println("  Choose how foci reaches an LLM.")
	fmt.Println()
	fmt.Println("  API providers — talk to a remote endpoint with an API key:")
	for i, p := range llmProviders {
		if !p.IsLocalBackend() {
			fmt.Printf("    [%d] %s\n", i+1, p.Name)
		}
	}
	localShown := false
	for i, p := range llmProviders {
		if !p.IsLocalBackend() {
			continue
		}
		if !localShown {
			fmt.Println()
			fmt.Println("  Local backend — delegates to a tool on this host, no API key:")
			localShown = true
		}
		fmt.Printf("    [%d] %s\n", i+1, p.Name)
	}
	fmt.Println()

	for {
		fmt.Print("> ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		if input == "back" {
			return "", true
		}

		var idx int
		if _, err := fmt.Sscanf(input, "%d", &idx); err == nil && idx >= 1 && idx <= len(llmProviders) {
			p := llmProviders[idx-1]
			fmt.Printf("  Provider: %s\n", p.Name)
			return p.Key, false
		}
		fmt.Printf("  Enter a number between 1 and %d.\n", len(llmProviders))
	}
}

// stepAPIKey prompts for the API key and stores it in the secrets store.
func stepAPIKey(reader *bufio.Reader, providerKey string, store *secrets.Store, total int) (back bool) {
	fmt.Println()
	fmt.Printf("Step 2/%d: API Key\n", total)

	prov := providerByKey(providerKey)
	if prov != nil && prov.IsLocalBackend() {
		// Local backends (e.g. claude-code) authenticate via the host tool's
		// own login (run /login after setup), so there is no API key to enter.
		fmt.Printf("  %s needs no API key — it uses your local Claude Code login.\n", prov.Name)
		fmt.Println("  After setup, run /login in chat to authenticate.")
		return false
	}
	if prov == nil || providerKey == "custom" {
		// Custom provider handles credentials in stepCustomEndpoint
		fmt.Println("  (API key will be configured with the endpoint.)")
		return false
	}

	fmt.Printf("  Enter your %s API key:\n", prov.Name)
	fmt.Println()

	for {
		fmt.Print("  API key: ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)
		if input == "back" {
			return true
		}
		if input == "" {
			fmt.Println("  API key cannot be empty.")
			continue
		}
		store.Set(prov.SecretKey, input)
		fmt.Println("  API key saved.")
		return false
	}
}

// stepCustomEndpoint collects custom endpoint details.
func stepCustomEndpoint(reader *bufio.Reader, store *secrets.Store, total int) (ce *config.CustomEndpointSetup, back bool, err error) {
	fmt.Println()
	fmt.Printf("Step 2/%d: Custom Endpoint\n", total)
	fmt.Println("  Configure your custom LLM endpoint.")
	fmt.Println()

	// Endpoint name
	fmt.Print("  Endpoint name (e.g. local, vllm): ")
	name, _ := reader.ReadString('\n')
	name = strings.TrimSpace(name)
	if name == "back" {
		return nil, true, nil
	}
	if name == "" {
		name = "custom"
	}

	// Base URL
	fmt.Print("  Base URL (e.g. http://localhost:8000/v1): ")
	url, _ := reader.ReadString('\n')
	url = strings.TrimSpace(url)
	if url == "back" {
		return nil, true, nil
	}
	if url == "" {
		return nil, false, fmt.Errorf("base URL is required")
	}

	// Wire format
	fmt.Println("  Wire format:")
	fmt.Println("  [1] OpenAI (most common)")
	fmt.Println("  [2] Anthropic")
	fmt.Println("  [3] Gemini")
	fmt.Println()
	var format string
	for {
		fmt.Print("> ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)
		if input == "back" {
			return nil, true, nil
		}
		switch input {
		case "1", "":
			format = "openai"
		case "2":
			format = "anthropic"
		case "3":
			format = "gemini"
		default:
			fmt.Println("  Enter 1, 2, or 3.")
			continue
		}
		break
	}

	// API key (optional)
	secretKey := name + ".api_key"
	fmt.Print("  API key (blank to skip): ")
	apiKey, _ := reader.ReadString('\n')
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "back" {
		return nil, true, nil
	}
	if apiKey != "" {
		store.Set(secretKey, apiKey)
		fmt.Println("  API key saved.")
	} else {
		secretKey = "" // no secret needed
	}

	fmt.Printf("  Custom endpoint %q configured (%s format, %s)\n", name, format, url)
	return &config.CustomEndpointSetup{
		Name:      name,
		URL:       url,
		Format:    format,
		SecretKey: secretKey,
	}, false, nil
}

// stepAgentID prompts for an agent identifier.
func stepAgentID(reader *bufio.Reader, current string, total int) (agentID string, back bool) {
	fmt.Println()
	fmt.Printf("Step 3/%d: Agent ID\n", total)
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
			fmt.Printf("  Agent ID: %s\n", current)
			return current, false
		}
		if provision.IsValidAgentID(input) {
			fmt.Printf("  Agent ID: %s\n", input)
			return input, false
		}
		fmt.Println("  Invalid ID. Use lowercase letters, numbers, and hyphens only.")
	}
}

// stepDisplayName prompts for a display name.
func stepDisplayName(reader *bufio.Reader, agentID, current string, total int) (displayName string, back bool) {
	fmt.Println()
	fmt.Printf("Step 4/%d: Display Name\n", total)
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
		fmt.Printf("  Display name: %s\n", defaultName)
		return defaultName, false
	}
	fmt.Printf("  Display name: %s\n", input)
	return input, false
}

// stepModel prompts for a model selection, aware of the chosen provider.
func stepModel(reader *bufio.Reader, current, providerKey string, store *secrets.Store, total int) (model string, back bool) {
	fmt.Println()
	fmt.Printf("Step 5/%d: Model\n", total)

	prov := providerByKey(providerKey)

	if prov != nil && prov.IsLocalBackend() {
		// Local backend: the alias is passed through verbatim to the host tool
		// (e.g. claude-code understands "opus"/"sonnet"/"haiku" natively), so no
		// alias resolution or API model discovery applies.
		fmt.Println("  Choose a model the backend understands: opus, sonnet, haiku.")
	} else if prov != nil && prov.HasAliases {
		// Anthropic: show aliases
		fmt.Println("  Choose a model: fable, opus, sonnet, haiku, or enter a full model ID.")
	} else if providerKey == "custom" {
		fmt.Println("  Enter a full model ID (developer/model_id, e.g. openai/my-model).")
	} else {
		fmt.Println("  Enter a full model ID (developer/model_id).")
		if prov != nil && prov.DefaultModel != "" {
			fmt.Printf("  Example: %s\n", prov.DefaultModel)
		}
	}
	fmt.Println()

	defaultModel := ""
	if current != "" {
		defaultModel = current
	} else if prov != nil {
		defaultModel = prov.DefaultModel
	}

	prompt := "Model"
	if defaultModel != "" {
		prompt = fmt.Sprintf("Model [%s]", defaultModel)
	}
	fmt.Printf("%s: ", prompt)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)

	if input == "back" {
		return "", true
	}
	if input == "" {
		input = defaultModel
	}
	if input == "" {
		fmt.Println("  Model is required.")
		return stepModel(reader, current, providerKey, store, total)
	}

	// Local backend: pass the alias through verbatim (the host tool resolves it).
	if prov != nil && prov.IsLocalBackend() {
		fmt.Printf("  Model: %s\n", input)
		return input, false
	}

	// For Anthropic with aliases, try API discovery
	if prov != nil && prov.HasDiscovery {
		resolved := discoverModelFamily(store, input)
		fmt.Printf("  Model: %s\n", resolved)
		return resolved, false
	}

	// For non-Anthropic, resolve alias or use as-is
	resolved := provision.ResolveModelAlias(input)
	fmt.Printf("  Model: %s\n", resolved)
	return resolved, false
}

// stepCharacterMode prompts for character file sourcing.
func stepCharacterMode(reader *bufio.Reader, _ setupFlags, total int) (charMode, importDir string, back bool) {
	fmt.Println()
	fmt.Printf("Step 6/%d: Character Files\n", total)
	fmt.Println("  How should we set up the character files?")
	fmt.Println("  [1] Defaults (recommended for new users)")
	fmt.Println("  [2] OpenClaw templates")
	fmt.Println("  [3] Import from a directory")
	fmt.Println()

	for {
		fmt.Print("> ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		if input == "back" {
			return "", "", true
		}

		switch input {
		case "1", "":
			fmt.Println("  Character files: defaults")
			return "defaults", "", false
		case "2":
			fmt.Println("  Character files: openclaw")
			return "openclaw", "", false
		case "3":
			fmt.Println("  Path to directory containing .md character files:")
			for {
				fmt.Print("> ")
				dir, _ := reader.ReadString('\n')
				dir = strings.TrimSpace(dir)
				if dir == "back" {
					break // re-show the mode menu
				}
				info, err := os.Stat(dir)
				if err != nil || !info.IsDir() {
					fmt.Printf("  Not a valid directory: %s\n  Try again (or 'back'):\n", dir)
					continue
				}
				fmt.Printf("  Import from: %s\n", dir)
				return "import", dir, false
			}
		default:
			fmt.Println("  Enter 1, 2, or 3.")
		}
	}
}

// stepMemoryImport prompts for memory file import.
// If the user imported character files in step 6, it auto-suggests likely memory directories.
func stepMemoryImport(reader *bufio.Reader, importDir string, total int) (memoryDir string, back bool) {
	fmt.Println()
	fmt.Printf("Step 7/%d: Memory Import\n", total)
	fmt.Println("  Import daily memory files from an existing agent?")
	fmt.Println("  [1] Skip (default)")
	fmt.Println("  [2] Import from a directory")
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
			fmt.Println("  Skipping memory import.")
			return "", false
		case "2":
			// Auto-suggest based on character import dir
			suggested := suggestMemoryDir(importDir)

			if suggested != "" {
				fmt.Printf("  Suggested: %s\n", suggested)
				fmt.Println("  Press Enter to accept, or type a different path:")
			} else {
				fmt.Println("  Path to directory containing .md memory files:")
			}

			for {
				fmt.Print("> ")
				dir, _ := reader.ReadString('\n')
				dir = strings.TrimSpace(dir)
				if dir == "back" {
					break // re-show the mode menu
				}
				if dir == "" && suggested != "" {
					dir = suggested
				}
				if dir == "" {
					fmt.Println("  Please enter a directory path (or 'back').")
					continue
				}
				info, err := os.Stat(dir)
				if err != nil || !info.IsDir() {
					fmt.Printf("  Not a valid directory: %s\n  Try again (or 'back'):\n", dir)
					continue
				}
				fmt.Printf("  Memory import from: %s\n", dir)
				return dir, false
			}
		default:
			fmt.Println("  Enter 1 or 2.")
		}
	}
}

// suggestMemoryDir tries to find a memory/ directory relative to the character import dir.
func suggestMemoryDir(importDir string) string {
	if importDir == "" {
		return ""
	}
	// If user pointed at workspace root: check importDir/memory/
	candidate := filepath.Join(importDir, "memory")
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	// If user pointed at character/ subdir: check importDir/../memory/
	candidate = filepath.Join(importDir, "..", "memory")
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return filepath.Clean(candidate)
	}
	return ""
}

// runProviderSetups lets the user choose which platforms to configure, then
// runs each selected provider's interactive setup and collects results.
func runProviderSetups(ui platform.SetupUI, flags map[string]string, total int) (configFragments []string, providerSecrets map[string]string, back bool, err error) {
	namedWizards := platform.SetupProviders()
	if len(namedWizards) == 0 {
		return nil, nil, false, nil
	}

	fmt.Println()
	fmt.Printf("Step %d/%d: Platform Configuration\n", total, total)

	// Determine which providers to run.
	var selected []platform.NamedSetupWizard
	if len(namedWizards) == 1 {
		selected = namedWizards
	} else {
		names := make([]string, len(namedWizards))
		for i, nw := range namedWizards {
			names[i] = nw.Name
		}
		indices, b := ui.MultiSelect("Which platforms do you want to configure?", names)
		if b {
			return nil, nil, true, nil
		}
		if len(indices) == 0 {
			// User chose none — skip platform step.
			return nil, nil, false, nil
		}
		for _, i := range indices {
			selected = append(selected, namedWizards[i])
		}
	}

	providerSecrets = map[string]string{}
	for _, nw := range selected {
		result, err := nw.Wizard.RunSetup(ui, flags, false)
		if err == platform.ErrSetupBack {
			return nil, nil, true, nil
		}
		if err != nil {
			return nil, nil, false, err
		}
		if result.ConfigTOML != "" {
			configFragments = append(configFragments, result.ConfigTOML)
		}
		for k, v := range result.Secrets {
			providerSecrets[k] = v
		}
	}
	return configFragments, providerSecrets, false, nil
}
