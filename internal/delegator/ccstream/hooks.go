package ccstream

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"foci/internal/log"
)

// ---------------------------------------------------------------------------
// CC hook integration
//
// Claude Code can't surface tool_result blocks on its stdout stream directly —
// tool execution happens internally and only the next assistant message (with
// the model's reaction) is emitted. To get per-tool completion signals foci
// installs PostToolUse and PostToolUseFailure hooks on each CC session that
// point at the bin/foci-cc-hook helper binary. CC invokes the hook after each
// tool execution, pipes a JSON envelope (tool_use_id, tool_name, response,
// error, agent_id, ...) into the binary's stdin, and captures the binary's
// stdout into a system/hook_response message on its stream-json output.
//
// Foci receives that hook_response message in OnSystem, parses the stdout
// field as the compact JSON our helper wrote, and dispatches to the current
// turn's EventHandler.OnToolEnd with the tool_use_id / tool_name / output /
// is_error fields. Sub-agent tool calls are filtered out by checking agent_id
// (non-empty = subagent) before dispatch.
//
// Hooks are installed by merging into {workDir}/.claude/settings.local.json —
// the local-scoped settings file CC treats as user-local overrides (typically
// gitignored). User-owned hooks in .claude/settings.json and ~/.claude/
// settings.json are never touched. If the user already has entries in
// settings.local.json, ours are added alongside rather than replacing.
//
// On Close foci removes its own hook entries (identified by the command path
// pointing at bin/foci-cc-hook). If foci crashes mid-session the entries are
// left behind but keep working correctly — the helper binary still exists
// and the hook continues to fire for the next CC invocation in that workdir.
// ---------------------------------------------------------------------------

// hookCommandName is the binary filename foci looks for alongside foci-gw.
const hookCommandName = "foci-cc-hook"

// hookTimeoutSeconds is the CC hook-script timeout foci configures. 10
// seconds is comfortable for the helper binary's ~10ms startup cost while
// still protecting against a pathological hang.
const hookTimeoutSeconds = 10

// Hook event names foci installs under.
const (
	eventPostToolUse        = "PostToolUse"
	eventPostToolUseFailure = "PostToolUseFailure"
)

// ---------------------------------------------------------------------------
// Settings.local.json merge support
// ---------------------------------------------------------------------------

// hooksConfig mirrors the "hooks" sub-tree of CC's settings.local.json.
// Event name → list of matcher entries; each matcher has one or more hook
// commands. Unknown fields in individual hook entries are preserved via
// Extras so foci round-trips the user's existing shape without data loss.
type hooksConfig map[string][]hookMatcher

type hookMatcher struct {
	Matcher string     `json:"matcher,omitempty"`
	Hooks   []hookSpec `json:"hooks"`
}

type hookSpec struct {
	Type    string `json:"type"`
	Command string `json:"command,omitempty"`
	Prompt  string `json:"prompt,omitempty"`
	Shell   string `json:"shell,omitempty"`
	Timeout int    `json:"timeout,omitempty"`
	Async   bool   `json:"async,omitempty"`
}

// fociHookSpec builds the hookSpec foci installs for an event. Identical for
// both PostToolUse and PostToolUseFailure — the helper binary branches on
// hook_event_name internally.
func fociHookSpec(hookCmd string) hookSpec {
	return hookSpec{
		Type:    "command",
		Command: hookCmd,
		Timeout: hookTimeoutSeconds,
	}
}

// ---------------------------------------------------------------------------
// Backend lifecycle
// ---------------------------------------------------------------------------

// installHooks merges foci's PostToolUse and PostToolUseFailure hooks into
// {workDir}/.claude/settings.local.json. It preserves any existing entries
// (user hooks, other tools' hooks) and is idempotent — running install
// twice leaves a single foci entry per event. Records paths on the Backend
// so uninstallHooks can locate and remove them on Close.
func (b *Backend) installHooks(workDir string) {
	hookCmd, err := resolveHookBinary()
	if err != nil {
		b.logger().Debugf("hook install skipped: %v", err)
		return
	}

	settingsPath := filepath.Join(workDir, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		b.logger().Warnf("hook install: create .claude dir: %v", err)
		return
	}

	top, err := loadSettings(settingsPath)
	if err != nil {
		b.logger().Warnf("hook install: load %s: %v", settingsPath, err)
		return
	}

	hooks := extractHooks(top)
	spec := fociHookSpec(hookCmd)
	for _, event := range []string{eventPostToolUse, eventPostToolUseFailure} {
		hooks[event] = ensureFociEntry(hooks[event], spec)
	}

	if err := writeHooks(settingsPath, top, hooks); err != nil {
		b.logger().Warnf("hook install: write %s: %v", settingsPath, err)
		return
	}

	b.mu.Lock()
	b.hookInstalled = true
	b.hookSettingsPath = settingsPath
	b.hookCmd = hookCmd
	b.mu.Unlock()
	b.logger().Infof("installed CC hooks at %s (command=%s)", settingsPath, hookCmd)
}

// uninstallHooks removes foci's hook entries from settings.local.json. If the
// file ends up empty it's deleted. No-op if installHooks never ran.
func (b *Backend) uninstallHooks() {
	b.mu.Lock()
	installed := b.hookInstalled
	settingsPath := b.hookSettingsPath
	hookCmd := b.hookCmd
	b.hookInstalled = false
	b.mu.Unlock()
	if !installed || settingsPath == "" || hookCmd == "" {
		return
	}

	top, err := loadSettings(settingsPath)
	if err != nil {
		b.logger().Warnf("hook uninstall: load %s: %v", settingsPath, err)
		return
	}

	hooks := extractHooks(top)
	for _, event := range []string{eventPostToolUse, eventPostToolUseFailure} {
		hooks[event] = removeFociEntries(hooks[event], hookCmd)
		if len(hooks[event]) == 0 {
			delete(hooks, event)
		}
	}

	if err := writeHooks(settingsPath, top, hooks); err != nil {
		b.logger().Warnf("hook uninstall: write %s: %v", settingsPath, err)
		return
	}
	b.logger().Debugf("uninstalled CC hooks from %s", settingsPath)
}

// resolveHookBinary returns the absolute path to bin/foci-cc-hook. It
// assumes the binary ships alongside the foci-gw executable — foci's
// standard Makefile builds both into the same bin/ directory. Returns an
// error if the binary can't be located or isn't executable; installHooks
// logs and skips in that case so dev builds that only built foci-gw keep
// working.
func resolveHookBinary() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("os.Executable: %w", err)
	}
	candidate := filepath.Join(filepath.Dir(self), hookCommandName)
	info, err := os.Stat(candidate)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", candidate, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("%s not executable", candidate)
	}
	return candidate, nil
}

// loadSettings reads settings.local.json and returns its top-level fields as
// a raw-message map. Missing file is treated as an empty config so fresh
// workdirs start clean.
func loadSettings(path string) (map[string]json.RawMessage, error) {
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if top == nil {
		top = map[string]json.RawMessage{}
	}
	return top, nil
}

// extractHooks pulls the "hooks" sub-tree out of a top-level settings map,
// returning a typed hooksConfig. Returns an empty map when the key is absent
// so callers can unconditionally merge into it.
func extractHooks(top map[string]json.RawMessage) hooksConfig {
	raw, ok := top["hooks"]
	if !ok || len(raw) == 0 {
		return hooksConfig{}
	}
	var h hooksConfig
	if err := json.Unmarshal(raw, &h); err != nil {
		return hooksConfig{}
	}
	if h == nil {
		h = hooksConfig{}
	}
	return h
}

// ensureFociEntry adds a matcher:"*" entry for foci's hook command to the
// event's matcher list if it isn't already present. Idempotent — running
// twice leaves exactly one foci entry.
func ensureFociEntry(matchers []hookMatcher, spec hookSpec) []hookMatcher {
	for _, m := range matchers {
		for _, h := range m.Hooks {
			if h.Type == "command" && h.Command == spec.Command {
				return matchers // already present
			}
		}
	}
	return append(matchers, hookMatcher{
		Matcher: "*",
		Hooks:   []hookSpec{spec},
	})
}

// removeFociEntries drops any hook whose command path matches our binary,
// and prunes matcher entries that have no hooks left. Preserves unrelated
// entries (user hooks, other tools' hooks) untouched.
func removeFociEntries(matchers []hookMatcher, hookCmd string) []hookMatcher {
	kept := matchers[:0]
	for _, m := range matchers {
		filtered := m.Hooks[:0]
		for _, h := range m.Hooks {
			if h.Type == "command" && h.Command == hookCmd {
				continue
			}
			filtered = append(filtered, h)
		}
		if len(filtered) == 0 {
			continue
		}
		m.Hooks = filtered
		kept = append(kept, m)
	}
	return kept
}

// writeHooks serialises the top-level map back to settings.local.json with
// the hooks sub-tree merged in. If the resulting hooks map is empty the
// "hooks" key is removed. If the top-level map is empty the file is
// deleted entirely. Atomic-write via tmp+rename.
func writeHooks(path string, top map[string]json.RawMessage, hooks hooksConfig) error {
	if len(hooks) == 0 {
		delete(top, "hooks")
	} else {
		raw, err := json.MarshalIndent(hooks, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal hooks: %w", err)
		}
		top["hooks"] = raw
	}

	if len(top) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		return nil
	}

	body, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	// Ensure the parent .claude/ exists — installHooks normally handles
	// this, but keep writeHooks self-sufficient so tests and future callers
	// can invoke it without pre-creating the directory.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s → %s: %w", tmp, path, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// hook_response stream dispatch
// ---------------------------------------------------------------------------

// hookResponseEnvelope is the system/hook_response message CC emits when a
// hook script completes. Stdout is the verbatim bytes the hook script wrote
// to its stdout — foci-cc-hook writes a compact hookScriptOutput JSON
// object there which we parse below. See claude-code src/cli/print.ts:655
// for the envelope's authoritative shape.
type hookResponseEnvelope struct {
	HookEvent string `json:"hook_event"`
	Stdout    string `json:"stdout"`
	ExitCode  int    `json:"exit_code"`
	Outcome   string `json:"outcome"`
}

// hookScriptOutput is the compact JSON foci-cc-hook writes to its stdout.
// Must stay in sync with cmd/foci-cc-hook/main.go's hookOutput type — they
// are the two sides of a stable contract.
type hookScriptOutput struct {
	HookEvent    string `json:"hook_event"`
	ToolUseID    string `json:"tool_use_id"`
	ToolName     string `json:"tool_name"`
	ToolResponse string `json:"tool_response,omitempty"`
	Error        string `json:"error,omitempty"`
	AgentID      string `json:"agent_id,omitempty"`
	IsError      bool   `json:"is_error"`
}

// handleHookResponse parses a system/hook_response envelope and dispatches
// to the current turn's EventHandler.OnToolEnd for PostToolUse and
// PostToolUseFailure events. Sub-agent tool calls (agent_id non-empty) are
// filtered out at parse time — their tool results belong to the sub-agent's
// own transcript rather than the parent turn.
//
// Unknown hook events (e.g. PreToolUse) and hook events we don't have a
// helper binary for (user-configured hooks with their own scripts) are
// silently ignored — we only act on output that matches our contract.
// Malformed stdout (parse failure) degrades gracefully: log at debug and
// drop the event, keeping the rest of the turn flowing.
func (b *Backend) handleHookResponse(raw json.RawMessage) {
	var env hookResponseEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return
	}
	if env.HookEvent != eventPostToolUse && env.HookEvent != eventPostToolUseFailure {
		return
	}
	if env.Stdout == "" {
		return
	}

	var parsed hookScriptOutput
	if err := json.Unmarshal([]byte(env.Stdout), &parsed); err != nil {
		b.logger().Debugf("hook_response: unparseable stdout: %v", err)
		return
	}
	if parsed.ToolUseID == "" {
		return
	}
	// Sidechain filter: sub-agent tool calls have a non-empty agent_id per
	// claude-code src/utils/hooks.ts:createBaseHookInput. Skip so they
	// don't fire OnToolEnd on the parent turn's tracker.
	if parsed.AgentID != "" {
		return
	}

	b.turnMu.Lock()
	handler := b.turnHandler
	b.turnMu.Unlock()
	if handler == nil || handler.OnToolEnd == nil {
		return
	}

	output := parsed.ToolResponse
	if parsed.IsError && parsed.Error != "" {
		output = parsed.Error
	}
	handler.OnToolEnd(parsed.ToolUseID, parsed.ToolName, output, parsed.IsError)
}

// logger returns a component-scoped logger for hook-related messages.
// Delegates to the backend's standard logger shape.
func (b *Backend) logger() *log.ComponentLogger {
	return log.NewComponentLogger(b.logComponent())
}
