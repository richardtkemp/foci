package command

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"foci/internal/provision"
)

// AgentNewDeps holds dependencies for the /agents new wizard.
type AgentNewDeps struct {
	ConfigPath   string      // path to foci.toml
	DefaultsDir  string      // path to shared/
	HomeDir      string      // base dir for workspaces (e.g. /home/foci)
	FileMode     os.FileMode // permission bits for created files (0 → 0640)
	ListFn       func() []AgentInfo
	PreFlightFn  func(agentID string) []string // platform pre-flight warnings
	ResolveModel func(string) string
	// AvailableBackends is the live set of registered delegated backend names
	// (e.g. "claude-code", "claude-code-tmux"), injected from the delegator
	// registry. Empty → the wizard falls back to offering just "claude-code".
	AvailableBackends []string
	Registry          *Registry // for setting wizard
}

// Wizard step indices. The flow is name → model → backend → character mode.
const (
	stepName = iota
	stepModel
	stepBackend
	stepCharMode
)

// defaultBackend is the backend offered (and used on empty input) when it is
// available — most agents want Claude Code delegation.
const defaultBackend = "claude-code"

// apiBackend is the explicit value for an in-process (non-delegated) agent.
const apiBackend = "api"

// agentWizard implements WizardHandler for interactive agent creation.
type agentWizard struct {
	step int
	deps AgentNewDeps

	// Collected values:
	id, display, model string
	modelRaw           string // raw model input (alias) for backend_config.model
	backend            string // "api" or a registered backend name
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
	case stepName:
		return w.handleName(text)
	case stepModel:
		return w.handleModel(text)
	case stepBackend:
		return w.handleBackend(text)
	case stepCharMode:
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
	w.step = stepModel
	return "Model — `opus`, `sonnet`, `haiku`, or full model ID (default: `sonnet`):", false
}

func (w *agentWizard) handleModel(text string) (string, bool) {
	resolve := w.deps.ResolveModel
	if resolve == nil {
		resolve = provision.ResolveModelAlias
	}
	// Keep the raw alias (e.g. "opus") for backend_config.model — delegated
	// backends pass it straight to CC's --model. The resolved developer/model_id
	// form is kept for display and the (future) API path.
	w.modelRaw = text
	if strings.TrimSpace(text) == "" {
		w.modelRaw = "sonnet"
	}
	w.model = resolve(text)

	// Run platform pre-flight checks — surfaced on the next prompt.
	var warning string
	if w.deps.PreFlightFn != nil {
		if warnings := w.deps.PreFlightFn(w.id); len(warnings) > 0 {
			warning = "\n⚠️  " + strings.Join(warnings, "\n⚠️  ")
		}
	}

	w.step = stepBackend
	return w.backendPrompt() + warning, false
}

// availableBackends returns the registered backend names, falling back to just
// the default when none were injected (e.g. in unit tests, or if no backend
// package is linked).
func (w *agentWizard) availableBackends() []string {
	if len(w.deps.AvailableBackends) > 0 {
		return w.deps.AvailableBackends
	}
	return []string{defaultBackend}
}

// backendPrompt builds the execution-mode prompt listing the live backends plus
// the in-process `api` option.
func (w *agentWizard) backendPrompt() string {
	backends := w.availableBackends()
	opts := make([]string, 0, len(backends)+1)
	for _, b := range backends {
		if b == defaultBackend {
			opts = append(opts, "`"+b+"` (default)")
		} else {
			opts = append(opts, "`"+b+"`")
		}
	}
	opts = append(opts, "`api` (in-process, no delegation)")
	return "Backend — " + strings.Join(opts, ", ") + ":"
}

// handleBackend records the execution mode: a registered delegated backend, or
// "api" for the traditional in-process loop. Empty input picks the default
// backend (claude-code) — the common case, and the fix for agents previously
// created with no backend at all (silent API fallback).
func (w *agentWizard) handleBackend(text string) (string, bool) {
	choice := strings.ToLower(strings.TrimSpace(text))
	backends := w.availableBackends()

	if choice == "" {
		// Prefer the default backend if offered, else the first available.
		choice = defaultBackend
		if !slices.Contains(backends, defaultBackend) {
			choice = backends[0]
		}
	}

	switch {
	case choice == apiBackend:
		w.backend = apiBackend
	case slices.Contains(backends, choice):
		w.backend = choice
	default:
		return fmt.Sprintf("Must be one of: %s, or `api`. Try again:", strings.Join(backends, ", ")), false
	}

	w.step = stepCharMode
	return "Character files — `defaults` (recommended), `openclaw`, `copy <agent-id>`, or `blank` (default: `defaults`):", false
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
		DisplayName: w.display,
		HomeDir:     w.deps.HomeDir,
		DefaultsDir: w.deps.DefaultsDir,
		CharMode:    w.charMode,
		CopyFrom:    w.copyFrom,
		FileMode:    w.deps.FileMode,
		Backend:     w.backend,
		Model:       w.modelRaw,
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

	// Backend summary.
	if w.backend == apiBackend || w.backend == "" {
		sb.WriteString("✅ Backend: api (in-process)\n")
	} else {
		fmt.Fprintf(&sb, "✅ Backend: %s (model: %s)\n", w.backend, w.modelRaw)
	}

	// Append to foci.toml
	if err := appendToFile(w.deps.ConfigPath, result.ConfigBlock, w.deps.FileMode); err != nil {
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
func appendToFile(path, text string, mode os.FileMode) error {
	if mode == 0 {
		mode = 0640
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	_, err = f.WriteString(text)
	return err
}
