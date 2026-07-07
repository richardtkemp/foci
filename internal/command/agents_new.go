package command

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"foci/internal/provision"
	"foci/internal/question"
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

// Wizard step indices. The flow is name → backend → model → character mode.
// Backend is chosen before model so the model question is asked in the context
// of the selected execution mode (delegated backend vs in-process api) rather
// than implicitly assuming Claude Code. The model step is SKIPPED entirely for
// the `api` backend: API agents have no per-agent model field — their model
// resolves globally via [groups]/[models], and provision silently discards
// backend_config.model for api backends. So asking would be dead UI.
const (
	stepName = iota
	stepBackend
	stepModel
	stepCharMode
)

// charModePrompt is the character-files question, shared by the two steps that
// can precede it (model for delegated backends, backend for api).
const charModePrompt = "Character files — `defaults` (recommended), `openclaw`, `copy <agent-id>`, or `blank` (default: `defaults`):"

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

	// preflight carries the platform pre-flight warnings raised at the name
	// step; the backend prompt surfaces them (both the chat text and the
	// structured PendingStep must include them, so they live here).
	preflight string

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
	case stepBackend:
		return w.handleBackend(text)
	case stepModel:
		return w.handleModel(text)
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

	// Run platform pre-flight checks (keyed on the agent id) — surfaced on the
	// backend prompt, the next step.
	w.preflight = ""
	if w.deps.PreFlightFn != nil {
		if warnings := w.deps.PreFlightFn(w.id); len(warnings) > 0 {
			w.preflight = "\n⚠️  " + strings.Join(warnings, "\n⚠️  ")
		}
	}

	w.step = stepBackend
	return w.backendPrompt() + w.preflight, false
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

	w.step = stepCharMode
	return charModePrompt, false
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

	// API agents resolve their model globally via [groups]/[models]; there is no
	// per-agent model field, and provision discards backend_config.model for api
	// backends. Skip the (dead) model question and go straight to character files.
	if w.backend == apiBackend {
		w.step = stepCharMode
		return charModePrompt, false
	}

	w.step = stepModel
	return "Model — `opus`, `sonnet`, `haiku`, or full model ID (default: `sonnet`):", false
}

// wizardKindAgentsNew is the persisted-snapshot kind tag for agentWizard.
const wizardKindAgentsNew = "agents-new"

// agentWizardSnapshot is agentWizard's persisted state: the step index plus
// every collected value. Deps and createFn are re-injected at restore.
type agentWizardSnapshot struct {
	Step      int    `json:"step"`
	ID        string `json:"id,omitempty"`
	Display   string `json:"display,omitempty"`
	Model     string `json:"model,omitempty"`
	ModelRaw  string `json:"modelRaw,omitempty"`
	Backend   string `json:"backend,omitempty"`
	CharMode  string `json:"charMode,omitempty"`
	CopyFrom  string `json:"copyFrom,omitempty"`
	Preflight string `json:"preflight,omitempty"`
}

func (w *agentWizard) WizardKind() string { return wizardKindAgentsNew }

func (w *agentWizard) SnapshotWizard() ([]byte, error) {
	return json.Marshal(agentWizardSnapshot{
		Step: w.step, ID: w.id, Display: w.display, Model: w.model, ModelRaw: w.modelRaw,
		Backend: w.backend, CharMode: w.charMode, CopyFrom: w.copyFrom, Preflight: w.preflight,
	})
}

func (w *agentWizard) RestoreWizard(data []byte) error {
	var s agentWizardSnapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	w.step, w.id, w.display, w.model, w.modelRaw = s.Step, s.ID, s.Display, s.Model, s.ModelRaw
	w.backend, w.charMode, w.copyFrom, w.preflight = s.Backend, s.CharMode, s.CopyFrom, s.Preflight
	return nil
}

// PendingStep implements WizardStepProvider: it describes the current step as
// structured data for out-of-band (app) rendering. Only the two choice steps
// are structured — option labels are fed back into Handle verbatim when
// picked, so they must be valid Handle inputs. The free-text steps (name,
// model) return nil: the transport falls back to the plain prompt text, which
// also carries validation re-asks.
func (w *agentWizard) PendingStep() *question.Question {
	switch w.step {
	case stepBackend:
		backends := w.availableBackends()
		opts := make([]question.Option, 0, len(backends)+1)
		for _, b := range backends {
			desc := "Delegated backend"
			if b == defaultBackend {
				desc = "Delegated backend (default)"
			}
			opts = append(opts, question.Option{Label: b, Description: desc})
		}
		opts = append(opts, question.Option{Label: apiBackend, Description: "In-process, no delegation"})
		return &question.Question{
			Header:   "Backend",
			Question: "How should this agent run?" + w.preflight,
			Options:  opts,
		}
	case stepCharMode:
		return &question.Question{
			Header:   "Character files",
			Question: "Which character files should the agent start with? Type `copy <agent-id>` to copy another agent's.",
			Options: []question.Option{
				{Label: "defaults", Description: "Copied from defaults (recommended)"},
				{Label: "openclaw", Description: "Copied from openclaw"},
				{Label: "blank", Description: "Blank templates"},
			},
		}
	default:
		return nil // free-text step (name, model) — plain prompt text
	}
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
		sb.WriteString("   ↳ Model resolves via global [groups]/[models] — edit those to change it.\n")
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
