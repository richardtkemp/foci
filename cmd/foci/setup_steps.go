package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"foci/internal/anthropic"
	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/provision"
	"foci/internal/secrets"
)

// stepAuth prompts for authentication method.
func stepAuth(reader *bufio.Reader, current string, total int) (method string, back bool) { // nolint:unparam
	fmt.Println()
	fmt.Printf("Step 1/%d: Anthropic Authentication\n", total)
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
	fmt.Printf("Step 2/%d: Credential\n", total)

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
		fmt.Println("  Setup token saved.")
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
			fmt.Println("  API key saved.")
			break
		}

	case "skip":
		fmt.Println("  Skipping auth (will use Claude Code credentials if available).")
	}

	return false, nil
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

// stepModel prompts for a model selection.
func stepModel(reader *bufio.Reader, current string, store *secrets.Store, total int) (model string, back bool) {
	fmt.Println()
	fmt.Printf("Step 5/%d: Model\n", total)
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

// runProviderSetups runs each provider's interactive setup and collects results.
func runProviderSetups(ui platform.SetupUI, flags map[string]string, total int) (configFragments []string, providerSecrets map[string]string, back bool, err error) {
	wizards := platform.SetupProviders()
	if len(wizards) == 0 {
		return nil, nil, false, nil
	}

	fmt.Println()
	fmt.Printf("Step %d/%d: Platform Configuration\n", total, total)

	providerSecrets = map[string]string{}
	for _, w := range wizards {
		result, err := w.RunSetup(ui, flags, false)
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
