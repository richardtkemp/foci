package command

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"foci/internal/provision"
)

// AgentNewDeps holds dependencies for the /agents new wizard.
type AgentNewDeps struct {
	ConfigPath   string // path to foci.toml
	DefaultsDir  string // path to shared/
	HomeDir      string // base dir for workspaces (e.g. /home/foci)
	ListFn       func() []AgentInfo
	PreFlightFn  func(agentID string) []string // platform pre-flight warnings
	ResolveModel func(string) string
	Registry     *Registry // for setting wizard
}

// agentWizard implements WizardHandler for interactive agent creation.
type agentWizard struct {
	step int
	deps AgentNewDeps

	// Collected values:
	id, display, model string
	charMode, copyFrom string

	// Overridable for testing:
	createFn func(w *agentWizard) (string, error)
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
	case 0: // Name
		return w.handleName(text)
	case 1: // Model
		return w.handleModel(text)
	case 2: // Character mode
		return w.handleCharMode(text)
	default:
		return "Unexpected state.", true
	}
}

func (w *agentWizard) handleName(text string) (string, bool) {
	if text == "" {
		return "Name cannot be empty. Try again:", false
	}

	id := provision.ToSlug(text)
	if !provision.IsValidAgentID(id) {
		return fmt.Sprintf("Could not form a valid ID from %q (got %q) — try a simpler name:", text, id), false
	}

	// Check uniqueness
	for _, a := range w.deps.ListFn() {
		if a.ID == id {
			return fmt.Sprintf("Agent `%s` already exists. Choose a different name:", id), false
		}
	}

	w.display = text
	w.id = id
	w.step = 1
	return "Model — `opus`, `sonnet`, `haiku`, or full model ID (default: `sonnet`):", false
}

func (w *agentWizard) handleModel(text string) (string, bool) {
	resolve := w.deps.ResolveModel
	if resolve == nil {
		resolve = provision.ResolveModelAlias
	}
	w.model = resolve(text)

	// Run platform pre-flight checks
	var warning string
	if w.deps.PreFlightFn != nil {
		if warnings := w.deps.PreFlightFn(w.id); len(warnings) > 0 {
			warning = "\n⚠️  " + strings.Join(warnings, "\n⚠️  ")
		}
	}

	w.step = 2
	return fmt.Sprintf("Character files — `defaults` (recommended), `openclaw`, `copy <agent-id>`, or `blank` (default: `defaults`):%s", warning), false
}

func (w *agentWizard) handleCharMode(text string) (string, bool) {
	if text == "" {
		text = "defaults"
	}
	lower := strings.ToLower(text)

	if lower == "defaults" {
		w.charMode = "defaults"
	} else if lower == "openclaw" {
		w.charMode = "openclaw"
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
		return "Must be `defaults`, `openclaw`, `copy <agent-id>`, or `blank`. Try again:", false
	}

	// Execute creation
	result, err := w.createFn(w)
	if err != nil {
		return fmt.Sprintf("Creation failed: %s", err), true
	}
	return result, true
}

// createAgent is the default creation function that sets up workspace, config, and crontab.
func createAgent(w *agentWizard) (string, error) {
	spec := provision.AgentSpec{
		ID:          w.id,
		Model:       w.model,
		DisplayName: w.display,
		HomeDir:     w.deps.HomeDir,
		DefaultsDir: w.deps.DefaultsDir,
		CharMode:    w.charMode,
		CopyFrom:    w.copyFrom,
	}

	// Count existing agents for crontab staggering
	existingCount := len(w.deps.ListFn())

	result, err := provision.Provision(spec)
	if err != nil {
		return "", err
	}

	// Override crontab with stagger based on existing agents
	templatePath := filepath.Join(w.deps.DefaultsDir, "crontab.template")
	if existingCount > 0 {
		if lines, err := provision.GenerateCrontab(templatePath, spec, existingCount); err == nil {
			result.CrontabLines = lines
		}
	}

	var sb strings.Builder

	fmt.Fprintf(&sb, "✅ Workspace: %s\n", result.Workspace)
	switch w.charMode {
	case "defaults":
		sb.WriteString("✅ Character files: copied from defaults\n")
	case "openclaw":
		sb.WriteString("✅ Character files: copied from openclaw\n")
	case "copy":
		fmt.Fprintf(&sb, "✅ Character files: copied from %s\n", w.copyFrom)
	case "blank":
		sb.WriteString("✅ Character files: blank templates created\n")
	}

	// Append to foci.toml
	if err := appendToFile(w.deps.ConfigPath, result.ConfigBlock); err != nil {
		return "", fmt.Errorf("update config: %w", err)
	}
	fmt.Fprintf(&sb, "✅ Config: appended to %s\n", w.deps.ConfigPath)

	// Crontab entries
	if len(result.CrontabLines) > 0 {
		if err := provision.AppendCrontab(result.CrontabLines); err != nil {
			sb.WriteString("⚠️  Crontab: could not update automatically. Add these entries manually:\n")
			for _, line := range result.CrontabLines {
				fmt.Fprintf(&sb, "   %s\n", line)
			}
		} else {
			sb.WriteString("✅ Crontab: entries added\n")
		}
	}

	sb.WriteString(fmt.Sprintf("\n%s (%s) is ready.\n", w.display, w.id))
	sb.WriteString("Restart foci for the new agent to start: /restart")
	return sb.String(), nil
}

// appendToFile appends text to a file.
func appendToFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644) // #nosec G302 - appending to existing config file
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	_, err = f.WriteString(text)
	return err
}
