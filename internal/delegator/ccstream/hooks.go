package ccstream

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"foci/internal/delegator"
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
// turns SessionEvents.OnToolEnd with the tool_use_id / tool_name / output /
// is_error fields. Sub-agent tool calls are filtered out by checking agent_id
// (non-empty = subagent) before dispatch.
//
// Install mechanism: CC accepts a `--settings <file-or-json>` CLI flag
// (claude-code/src/main.tsx:1000) that loads an additional settings source
// called "flagSettings". flagSettings is always enabled regardless of any
// --setting-sources filter (constants.ts:159), and hooks from multiple
// sources merge rather than replace, so foci can pass its hook config as
// a JSON string on CC's command line and it coexists automatically with
// the user's own settings.json / settings.local.json hooks.
//
// This is significantly simpler than mutating the user's settings.local.json
// would be: no read-modify-write cycle, no mutex for concurrent backends, no
// multi-backend file race, no user-hook merge logic, no crash orphans, no
// uninstall step. Each CC process gets its own --settings argv, so two foci
// backends running in the same workdir have no shared state at all.
//
// Multi-backend safety: each backend generates a unique install ID, bakes
// it into the hook command string (`"<path>" --install <id>`), and filters
// incoming hook_response events by install_id. Even though foci backends
// share a workdir and might race-observe each other's events in the
// presence of user-installed hooks, each backend only acts on events it
// originated. This is also the path that keeps user-authored PostToolUse
// hooks out of foci's tracker: they fire, CC emits hook_response messages,
// but the install_id filter drops them cleanly.
// ---------------------------------------------------------------------------

// hookCommandName is the binary filename foci looks for alongside foci-gw or
// on $PATH.
const hookCommandName = "foci-cc-hook"

// installIDFlag must match cmd/foci-cc-hook/main.go installIDFlag — it's
// the flag the helper binary parses from its own argv to extract the ID
// foci passes in. Kept as a constant in both places so rename refactors
// surface as build errors in the tests.
const installIDFlag = "--install"

// hookTimeoutSeconds is the CC hook-script timeout foci configures. 10
// seconds is comfortable for the helper binary's ~10ms startup cost while
// still protecting against a pathological hang.
const hookTimeoutSeconds = 10

// Hook event names foci installs under.
const (
	eventPreToolUse         = "PreToolUse"
	eventPostToolUse        = "PostToolUse"
	eventPostToolUseFailure = "PostToolUseFailure"
)

// agentToolMatcher scopes the PreToolUse hook to the Agent (subagent-spawn) tool
// only, so ordinary tool calls don't each spawn an extra hook process — the Pre
// hook exists solely to surface a precise subagent start.
const agentToolMatcher = "Agent"

// newInstallID generates a short random identifier used to distinguish one
// backend's hook events from another's when multiple backends share a
// workdir or when the user has independently configured PostToolUse hooks.
// 8 bytes → 16 hex chars is plenty — collision risk is negligible compared
// to the probability of two backends installing in the same nanosecond, and
// even a collision only causes one backend to mis-filter (harmless with
// idempotent tracker updates).
func newInstallID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// buildHookCommand composes the shell command string foci writes into the
// hook settings JSON. The binary path is double-quoted so paths containing
// spaces survive bash parsing; the install ID is appended as an argv pair.
func buildHookCommand(hookPath, installID string) string {
	return fmt.Sprintf("%q %s %s", hookPath, installIDFlag, installID)
}

// ---------------------------------------------------------------------------
// Hook settings JSON build
// ---------------------------------------------------------------------------

// hooksConfig mirrors the "hooks" sub-tree of CC's settings schema. Event
// name → list of matcher entries; each matcher has one or more hook
// commands. Unknown fields in individual hook entries are preserved via
// Extras so foci round-trips any existing shape without data loss. (foci
// no longer parses the user's settings.local.json, so this is only used
// to emit foci's own entries.)
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

// fociHookSpec builds the hookSpec foci installs for an event. hookCmd is
// the full command string (path + --install <id>) so every backend's spec
// is uniquely identifiable by its embedded install ID. Identical shape for
// both PostToolUse and PostToolUseFailure — the helper binary branches on
// hook_event_name internally.
func fociHookSpec(hookCmd string) hookSpec {
	return hookSpec{
		Type:    "command",
		Command: hookCmd,
		Timeout: hookTimeoutSeconds,
	}
}

// buildHookSettingsJSON returns a JSON string encoding a settings object
// containing PostToolUse and PostToolUseFailure hook entries pointing at
// the given hook command. CC accepts this string via `--settings <json>`
// and loads it as an additional merged-in settings source. No filesystem
// I/O happens here — the caller passes the returned JSON as an argv to
// the claude subprocess.
func buildHookSettingsJSON(hookCmd string) (string, error) {
	spec := fociHookSpec(hookCmd)
	allTools := []hookMatcher{{
		Matcher: "*",
		Hooks:   []hookSpec{spec},
	}}
	top := map[string]any{
		"hooks": hooksConfig{
			// PreToolUse only for the Agent tool — a precise subagent start.
			eventPreToolUse:         {{Matcher: agentToolMatcher, Hooks: []hookSpec{spec}}},
			eventPostToolUse:        allTools,
			eventPostToolUseFailure: allTools,
		},
	}
	body, err := json.Marshal(top)
	if err != nil {
		return "", fmt.Errorf("marshal hook settings: %w", err)
	}
	return string(body), nil
}

// ---------------------------------------------------------------------------
// Backend lifecycle
// ---------------------------------------------------------------------------

// prepareHooks resolves the helper binary, generates a unique install ID,
// and returns the JSON settings string to pass to CC via `--settings`.
// The second return value is false when hook install should be skipped
// entirely (binary not found on sibling or PATH, or JSON marshal failure);
// callers omit the `--settings` argv in that case and CC launches without
// foci's hooks. A Warn-level log explains the skip so operators running
// stripped builds can diagnose missing tool-result display in ccstream.
func (b *Backend) prepareHooks() (string, bool) {
	hookPath, err := resolveHookBinary()
	if err != nil {
		b.logger().Warnf("CC hook install skipped: %v (ccstream OnToolEnd events will not fire)", err)
		return "", false
	}
	installID := newInstallID()
	hookCmd := buildHookCommand(hookPath, installID)
	settingsJSON, err := buildHookSettingsJSON(hookCmd)
	if err != nil {
		b.logger().Warnf("CC hook install skipped: %v", err)
		return "", false
	}

	b.mu.Lock()
	b.hookCmd = hookCmd
	b.hookInstallID = installID
	b.mu.Unlock()
	b.logger().Infof("CC hooks installed via --settings (install_id=%s)", installID)
	return settingsJSON, true
}

// resolveHookBinary returns the absolute path to foci-cc-hook. Lookup
// strategy (first hit wins):
//
//  1. Sibling of the running foci-gw executable, via os.Executable().
//     This is the standard case — foci's Makefile builds both binaries
//     into the same bin/ directory so co-located installs resolve here.
//  2. $PATH, via exec.LookPath. Covers distro packaging where foci-gw
//     and foci-cc-hook might end up in different directories (e.g.
//     /usr/local/bin/foci-gw + /usr/local/libexec/foci-cc-hook if the
//     latter is also on PATH, or any user-installed sibling).
//
// Returns an error if neither lookup finds an executable foci-cc-hook;
// prepareHooks logs at Warn and skips in that case so dev builds that
// only built foci-gw keep working (just without OnToolEnd events in
// ccstream mode).
func resolveHookBinary() (string, error) {
	var siblingErr error
	if self, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(self), hookCommandName)
		if isExecutableFile(candidate) {
			return candidate, nil
		}
		siblingErr = fmt.Errorf("sibling %s not executable", candidate)
	} else {
		siblingErr = fmt.Errorf("os.Executable: %w", err)
	}

	if path, err := exec.LookPath(hookCommandName); err == nil {
		return path, nil
	}

	return "", fmt.Errorf("%s not found (%v; and not on $PATH)", hookCommandName, siblingErr)
}

// isExecutableFile returns true when path is a regular file with at least
// one execute bit set. Used to validate resolveHookBinary candidates —
// directories, symlinks to non-executables, and mode-0 files all return
// false so the caller falls through to the next lookup strategy.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir() && info.Mode()&0o111 != 0
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
//
// InstallID is echoed back from the hook command's argv (see
// buildHookCommand) so handleHookResponse can filter events belonging to
// this backend from events belonging to user-installed hooks or other
// foci backends in the same process.
type hookScriptOutput struct {
	HookEvent    string `json:"hook_event"`
	InstallID    string `json:"install_id,omitempty"`
	ToolUseID    string `json:"tool_use_id"`
	ToolName     string `json:"tool_name"`
	ToolInput    string `json:"tool_input,omitempty"`
	ToolResponse string `json:"tool_response,omitempty"`
	Error        string `json:"error,omitempty"`
	AgentID      string `json:"agent_id,omitempty"`
	IsError      bool   `json:"is_error"`
}

// handleHookResponse parses a system/hook_response envelope and dispatches
// to the sessions SessionEvents.OnToolEnd for PostToolUse and
// PostToolUseFailure events.
//
// Three filter layers before dispatch:
//  1. Hook event must be PostToolUse or PostToolUseFailure — user-
//     configured PreToolUse or lifecycle hooks are silently ignored.
//  2. Install ID must match this backend's install ID. When user hooks
//     coexist with foci's via flagSettings + userSettings merging, each
//     fires its own hook_response; we only act on our own.
//  3. Sub-agent tool calls (agent_id non-empty) are filtered out — their
//     tool results belong to the sub-agent's own transcript rather than
//     the parent turn.
//
// Malformed stdout (parse failure) degrades gracefully: log at debug and
// drop the event, keeping the rest of the turn flowing.
func (b *Backend) handleHookResponse(raw json.RawMessage) {
	var env hookResponseEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return
	}
	if env.HookEvent != eventPreToolUse && env.HookEvent != eventPostToolUse && env.HookEvent != eventPostToolUseFailure {
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

	// Multi-source filter: only events emitted by a hook we installed
	// (carrying our install ID in their echoed stdout) belong to us.
	// Events from user-configured hooks — which coexist with foci's via
	// CC's source-merging — and hook events that arrive when foci never
	// installed a hook at all (prepareHooks skipped because the helper
	// binary wasn't found) are both dropped here. Requires exact match:
	// an empty ourInstallID means no match is ever possible, so every
	// hook event is correctly ignored when foci's install is inactive.
	b.mu.Lock()
	ourInstallID := b.hookInstallID
	b.mu.Unlock()
	if ourInstallID == "" || parsed.InstallID != ourInstallID {
		return
	}

	// Sidechain filter: sub-agent tool calls have a non-empty agent_id per
	// claude-code src/utils/hooks.ts:createBaseHookInput. Skip so they
	// don't fire OnToolEnd on the parent turn's tracker.
	if parsed.AgentID != "" {
		return
	}

	se := b.sessionEvents.Load()

	// PreToolUse (installed only for the Agent tool) is the PRIMARY subagent START
	// signal — its tool_use id is the run's group key. This was originally
	// designed symmetrically with END (commit 96ef6df0: "a missing/broken hook
	// yields NEITHER"), but END was later moved off PostToolUse onto the native
	// task_notification stream event (a background Agent tool_use's PostToolUse
	// fires at launch, not completion — see verify-cc-stream-hooks/SKILL.md), and
	// text was always native too. That left START as the ONLY hook-only signal,
	// so a dropped/racing hook (~7% of background subagents under concurrent/
	// bursty dispatch, #1423) now uniquely orphans text+end with no start. #1425
	// restores the safety net with a fallback start at the first task_started
	// (handlers.go) — markSubagentStarted dedups so exactly one start goes out
	// regardless of which signal (hook or fallback) wins the race.
	if env.HookEvent == eventPreToolUse {
		if se != nil && se.OnSubagentStart != nil && parsed.ToolName == "Agent" {
			if !b.markSubagentStarted(parsed.ToolUseID) {
				// The initial Agent spawn is always run 1; carry its prompt so the app
				// can show what was asked at the top of the run view (#1355).
				input := json.RawMessage(parsed.ToolInput)
				b.logger().Infof("subagent_start signal=agent_pre_tool_use group=%s", parsed.ToolUseID)
				se.OnSubagentStart(parsed.ToolUseID, delegator.ExtractAgentDescription(input), delegator.ExtractAgentPrompt(input), 1)
				// Foreground transcript tailing is armed earlier, at the Agent
				// tool_use detection in OnAssistant (race-free vs task_started).
			}
		}
		return
	}

	// Tool delivery (OnToolEnd) goes through SessionEvents — always-live so
	// late tool results from a stacked turn don't drop. Post-tool nudges
	// are bookkeeping (require knowledge of the active turn's nudge
	// scheduler), so they read TurnEvents which may legitimately be nil
	// between turns.
	if se != nil && se.OnToolEnd != nil {
		output := parsed.ToolResponse
		if parsed.IsError && parsed.Error != "" {
			output = parsed.Error
		}
		se.OnToolEnd(parsed.ToolUseID, parsed.ToolName, output, parsed.IsError)
	}

	// The top-level Agent tool completing is a subagent run finishing — its
	// tool_use id is the run's group key. Precise per-run end (agent_id is empty
	// here: the Agent tool runs at the parent level, so the sidechain filter above
	// already let it through).
	if parsed.ToolName == "Agent" {
		// Drain and stop the foreground transcript tail so every subagent text block
		// lands in the chit. No-op for background / untailed subagents.
		//
		// The END signal is NOT fired here: a BACKGROUND Agent tool_use resolves the
		// instant the task is launched, so this PostToolUse fires while the subagent
		// runs on — marking the chit complete prematurely. The real end (fg and bg
		// alike) is task_notification:completed (handlers.go). Logged for comparison.
		b.subagentTails().finalize(parsed.ToolUseID)
		b.logger().Infof("subagent_end signal=agent_post_tool_use tuid=%s", parsed.ToolUseID)
	}

	// Fire any post-tool nudges the caller wants to inject for this tool.
	// Sends as a plain user message at the default queue priority "next".
	// CC's mid-turn drain at the next tool boundary
	// (claude-code's query.ts:1570-1589) folds the message as an
	// attachment to the current turn's tool_results — the model responds
	// in the same ask(), so the nudge response reaches OnText through
	// SessionEvents. No separate OnResult cycle.
	b.turnMu.Lock()
	turn := b.turnEvents
	b.turnMu.Unlock()
	if turn != nil && turn.PostToolNudgeFunc != nil {
		for _, text := range turn.PostToolNudgeFunc(parsed.ToolName, parsed.ToolInput, parsed.IsError) {
			if text == "" {
				continue
			}
			preview := text
			if len(preview) > 80 {
				preview = preview[:80] + "..."
			}
			if err := b.writer.SendUser("[user] " + text); err != nil {
				b.logger().Warnf("post-tool nudge SendUser failed: tool=%q err=%v preview=%q",
					parsed.ToolName, err, preview)
			} else {
				b.logger().Debugf("post-tool nudge sent: tool=%q is_error=%v bytes=%d preview=%q",
					parsed.ToolName, parsed.IsError, len(text)+len("[user] "), preview)
			}
		}
	}
}

// logger returns a component-scoped logger for hook-related messages.
// Delegates to the backend's standard logger shape.
func (b *Backend) logger() *log.ComponentLogger {
	return log.NewComponentLogger(b.logComponent())
}
