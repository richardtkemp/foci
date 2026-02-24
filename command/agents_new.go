package command

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// AgentNewDeps holds dependencies for the /agents new wizard.
type AgentNewDeps struct {
	ConfigPath  string           // path to clod.toml
	DefaultsDir string           // path to shared/defaults/
	HomeDir     string           // base dir for workspaces (e.g. /home/clod)
	ListFn      func() []AgentInfo
	SecretNames func() []string // current secret names
}

// agentWizard implements WizardHandler for interactive agent creation.
type agentWizard struct {
	step int
	deps AgentNewDeps

	// Collected values:
	id, display, emoji, model string
	botName, tokenSecret      string
	charMode, copyFrom        string

	// Overridable for testing:
	createFn func(w *agentWizard) (string, error)
}

var slugRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// IsValidSlug checks if a string is a valid agent ID slug.
func IsValidSlug(s string) bool {
	return slugRe.MatchString(s)
}

func newAgentWizard(deps AgentNewDeps) *agentWizard {
	w := &agentWizard{deps: deps}
	w.createFn = createAgent
	return w
}

// Handle processes a wizard step and returns the response.
func (w *agentWizard) Handle(text string) (response string, done bool) {
	text = strings.TrimSpace(text)

	switch w.step {
	case 0: // Agent ID
		return w.handleID(text)
	case 1: // Display name
		return w.handleDisplay(text)
	case 2: // Emoji
		return w.handleEmoji(text)
	case 3: // Model
		return w.handleModel(text)
	case 4: // Bot token secret
		return w.handleToken(text)
	case 5: // Character mode
		return w.handleCharMode(text)
	default:
		return "Unexpected state.", true
	}
}

func (w *agentWizard) handleID(text string) (string, bool) {
	text = strings.ToLower(text)
	if !IsValidSlug(text) {
		return "Invalid ID — must match `[a-z][a-z0-9-]*` (e.g. `greek-tutor`). Try again:", false
	}

	// Check uniqueness
	for _, a := range w.deps.ListFn() {
		if a.ID == text {
			return fmt.Sprintf("Agent `%s` already exists. Choose a different ID:", text), false
		}
	}

	w.id = text
	w.step = 1
	return "Display name (e.g. `Greek Tutor`):", false
}

func (w *agentWizard) handleDisplay(text string) (string, bool) {
	if text == "" {
		return "Display name cannot be empty. Try again:", false
	}
	w.display = text
	w.step = 2
	return "Emoji (single emoji for this agent):", false
}

func (w *agentWizard) handleEmoji(text string) (string, bool) {
	if text == "" {
		return "Emoji cannot be empty. Try again:", false
	}
	w.emoji = text
	w.step = 3
	return "Model — `opus`, `sonnet`, `haiku`, or full model ID (default: `sonnet`):", false
}

func (w *agentWizard) handleModel(text string) (string, bool) {
	w.model = resolveModel(text)
	w.step = 4
	return "Bot token secret name (e.g. `telegram.greek`):", false
}

func (w *agentWizard) handleToken(text string) (string, bool) {
	if text == "" || !strings.Contains(text, ".") {
		return "Secret must be in `section.key` format (e.g. `telegram.greek`). Try again:", false
	}

	w.tokenSecret = text

	// Derive bot name from the key part after "telegram."
	parts := strings.SplitN(text, ".", 2)
	w.botName = parts[len(parts)-1]

	// Check if the secret exists
	var warning string
	found := false
	for _, name := range w.deps.SecretNames() {
		if name == text {
			found = true
			break
		}
	}
	if !found {
		warning = fmt.Sprintf("\n⚠️  Secret `%s` not found — you'll need to add it with `/secrets set %s <token>` before starting.", text, text)
	}

	w.step = 5
	return fmt.Sprintf("Character files — `defaults` (recommended), `copy <agent-id>`, or `blank` (default: `defaults`):%s", warning), false
}

func (w *agentWizard) handleCharMode(text string) (string, bool) {
	if text == "" {
		text = "defaults"
	}
	lower := strings.ToLower(text)

	if lower == "defaults" {
		w.charMode = "defaults"
	} else if lower == "blank" {
		w.charMode = "blank"
	} else if strings.HasPrefix(lower, "copy ") {
		source := strings.TrimSpace(lower[5:])
		if source == "" {
			return "Usage: `copy <agent-id>`. Try again:", false
		}
		// Verify source agent exists
		found := false
		for _, a := range w.deps.ListFn() {
			if a.ID == source {
				found = true
				break
			}
		}
		if !found {
			return fmt.Sprintf("Agent `%s` not found. Try again:", source), false
		}
		w.charMode = "copy"
		w.copyFrom = source
	} else {
		return "Must be `defaults`, `copy <agent-id>`, or `blank`. Try again:", false
	}

	// Execute creation
	result, err := w.createFn(w)
	if err != nil {
		return fmt.Sprintf("Creation failed: %s", err), true
	}
	return result, true
}

// resolveModel resolves shorthand model names to full Anthropic model IDs.
func resolveModel(input string) string {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "opus":
		return "claude-opus-4-6"
	case "sonnet", "":
		return "claude-sonnet-4-6"
	case "haiku":
		return "claude-haiku-4-5"
	default:
		return input
	}
}

// createAgent is the default creation function that sets up workspace, config, and crontab.
func createAgent(w *agentWizard) (string, error) {
	workspace := filepath.Join(w.deps.HomeDir, w.id)
	var sb strings.Builder

	// 1. Create workspace directories
	for _, dir := range []string{"character", "memory", "prompts"} {
		if err := os.MkdirAll(filepath.Join(workspace, dir), 0755); err != nil {
			return "", fmt.Errorf("create %s: %w", dir, err)
		}
	}
	fmt.Fprintf(&sb, "✅ Workspace: %s\n", workspace)

	// 2. Character files
	switch w.charMode {
	case "defaults":
		if err := copyCharacterFiles(w.deps.DefaultsDir, workspace); err != nil {
			return "", fmt.Errorf("copy defaults: %w", err)
		}
		sb.WriteString("✅ Character files: copied from defaults\n")
	case "copy":
		sourceWorkspace := filepath.Join(w.deps.HomeDir, w.copyFrom)
		if err := copyDir(filepath.Join(sourceWorkspace, "character"), filepath.Join(workspace, "character")); err != nil {
			return "", fmt.Errorf("copy from %s: %w", w.copyFrom, err)
		}
		fmt.Fprintf(&sb, "✅ Character files: copied from %s\n", w.copyFrom)
	case "blank":
		for _, name := range []string{"SOUL.md", "COHERENCE.md", "CRAFT.md", "USER.md", "MEMORY.md"} {
			path := filepath.Join(workspace, "character", name)
			if err := os.WriteFile(path, []byte(""), 0644); err != nil {
				return "", fmt.Errorf("create %s: %w", name, err)
			}
		}
		sb.WriteString("✅ Character files: blank templates created\n")
	}

	// 3. Append to clod.toml
	configEntry := generateConfigEntry(w, workspace)
	if err := appendToFile(w.deps.ConfigPath, configEntry); err != nil {
		return "", fmt.Errorf("update config: %w", err)
	}
	fmt.Fprintf(&sb, "✅ Config: appended to %s\n", w.deps.ConfigPath)

	// 4. Crontab entries
	crontabLines := generateCrontab(w, workspace)
	if err := appendCrontab(crontabLines); err != nil {
		sb.WriteString("⚠️  Crontab: could not update automatically. Add these entries manually:\n")
		for _, line := range crontabLines {
			fmt.Fprintf(&sb, "   %s\n", line)
		}
	} else {
		sb.WriteString("✅ Crontab: entries added\n")
	}

	sb.WriteString(fmt.Sprintf("\n%s %s (%s) is ready.\n", w.emoji, w.display, w.id))
	sb.WriteString("Restart clod for the new agent to start: /restart")
	return sb.String(), nil
}

// generateConfigEntry produces the TOML config block for the new agent.
func generateConfigEntry(w *agentWizard, workspace string) string {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString("[[agents]]\n")
	fmt.Fprintf(&sb, "id = %q\n", w.id)
	fmt.Fprintf(&sb, "model = %q\n", w.model)
	fmt.Fprintf(&sb, "telegram_bot = %q\n", w.botName)
	fmt.Fprintf(&sb, "workspace = %q\n", workspace)
	sb.WriteString("system_files = [\"character/SOUL.md\", \"character/COHERENCE.md\", \"character/CRAFT.md\", \"character/USER.md\", \"character/MEMORY.md\"]\n")
	sb.WriteString("\n")
	sb.WriteString("[[agents.memory.sources]]\n")
	fmt.Fprintf(&sb, "name = %q\n", w.id)
	fmt.Fprintf(&sb, "dir = %q\n", filepath.Join(workspace, "memory"))
	sb.WriteString("weight = 1.0\n")
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "[telegram.bots.%s]\n", w.botName)
	fmt.Fprintf(&sb, "token_secret = %q\n", w.tokenSecret)

	return sb.String()
}

// generateCrontab returns crontab entries for the new agent.
func generateCrontab(w *agentWizard, workspace string) []string {
	// Stagger based on number of existing agents
	agents := w.deps.ListFn()
	offset := len(agents) * 3

	// Prompt files in the workspace/prompts/ directory
	promptsDir := filepath.Join(workspace, "prompts")
	heartbeatMin := offset % 60

	return []string{
		fmt.Sprintf("# %s — %s", w.id, w.display),
		fmt.Sprintf("%d */4 * * * curl -s -X POST http://localhost:7080/wake -d '{\"agent\":\"%s\",\"prompt_file\":\"%s/HEARTBEAT.md\",\"no_compact\":true}' > /dev/null 2>&1",
			heartbeatMin, w.id, promptsDir),
	}
}

// copyCharacterFiles copies default character and prompt files to a new workspace.
func copyCharacterFiles(defaultsDir, workspace string) error {
	charSrc := filepath.Join(defaultsDir, "character")
	charDst := filepath.Join(workspace, "character")

	if err := copyDir(charSrc, charDst); err != nil {
		return err
	}

	// Copy heartbeat prompt if it exists
	heartbeatSrc := filepath.Join(defaultsDir, "prompts", "HEARTBEAT.md")
	heartbeatDst := filepath.Join(workspace, "prompts", "HEARTBEAT.md")
	if _, err := os.Stat(heartbeatSrc); err == nil {
		return copyFile(heartbeatSrc, heartbeatDst)
	}

	return nil
}

// copyDir copies all files from src to dst (non-recursive, files only).
func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := copyFile(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies a single file.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// appendToFile appends text to a file.
func appendToFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(text)
	return err
}

// appendCrontab appends entries to the user's crontab.
func appendCrontab(lines []string) error {
	newEntries := strings.Join(lines, "\n")
	cmd := fmt.Sprintf("(crontab -l 2>/dev/null; echo %q) | crontab -", "\n"+newEntries+"\n")
	return runCrontabCmd(cmd)
}

// runCrontabCmd is the function used to append crontab entries.
// Overridden in tests to avoid real exec.
var runCrontabCmd = func(shellCmd string) error {
	return exec.Command("sh", "-c", shellCmd).Run()
}
