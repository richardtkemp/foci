package ccstream

import (
	"foci/internal/delegator"
)

// SetPermissionPromptFunc sets the function used to send permission prompts.
func (b *Backend) SetPermissionPromptFunc(fn delegator.PermissionPromptFunc) { b.permPromptFn = fn }

// SetOnPromptsCleared sets a callback fired when the last outstanding prompt
// (permission, question, or elicitation) is removed. Used by
// DelegatedManager.WaitForPermission to unblock once all pending prompts have
// been resolved or cancelled.
func (b *Backend) SetOnPromptsCleared(fn func()) { b.outstanding.SetOnEmpty(fn) }

// RegisterPromptCancelListener appends a callback fired when the prompt with
// requestID is cancelled by a non-user path (e.g. CC's control_cancel_request
// after a follow-up message aborted the in-flight tool execution). The
// listener does NOT fire on normal user responses — use it to clean up
// per-prompt UI state (e.g. disable the inline keyboard) so the user can't
// click an already-resolved button. Multiple listeners may be registered for
// the same requestID; they fire in registration order. If no prompt with
// requestID is registered, the call is a silent no-op.
func (b *Backend) RegisterPromptCancelListener(requestID string, fn func(reason string)) {
	b.outstanding.AddCancelListener(requestID, fn)
}

// SetOnSessionReady sets a callback fired once when the session ID is known.
func (b *Backend) SetOnSessionReady(fn func(string)) { b.onSessionReady = fn }

// SetTypingFunc sets a callback to control the platform's typing indicator.
func (b *Backend) SetTypingFunc(fn func(bool)) { b.typingFunc = fn }

// SetOnSubagentStatus sets a callback for subagent (Agent-tool) lifecycle
// events. The callback receives the running-subagent detail string (or "" when
// none are running) — see delegator.SubagentTracker.OnStatus.
func (b *Backend) SetOnSubagentStatus(fn func(detail string)) { b.agents.OnStatus = fn }

// SetOnSubagentRunning sets the callback for the structured running list (id,
// kind, model, start time) — see delegator.SubagentTracker.OnRunning (#2138).
func (b *Backend) SetOnSubagentRunning(fn func([]delegator.RunningSubagent)) { b.agents.OnRunning = fn }

// RunningSubagents returns the session's running subagents and background
// commands, for the statusline's [running] line (#2127).
func (b *Backend) RunningSubagents() []delegator.RunningSubagent { return b.agents.Running() }

// SetHostHooks implements delegator.HostHooksAcceptor: the gateway's
// callbacks, installed before Start.
//   - OnAuthFailure fires when CC reports an authentication failure (a 401),
//     to trigger the automated re-login (#843).
//   - OnRateLimitNotice gets a formatted rate_limit_event notice when CC
//     reports the API is past the "allowed" threshold, with this Backend's
//     foci session key so it reaches the session that triggered it (#1857).
//     It does NOT gate periodic work (#1211/#1238).
//   - OnModelFallbackNotice gets a formatted notice when CC's model refused
//     and CC fell back to another model (model_refusal_fallback), for the
//     session's chat (#2200).
//   - EngageRateLimit fires when CC reports a session limit (a synthetic
//     "You've hit your session limit · resets <time>" message, which unlike a
//     direct-API 429 never reaches classifyAPIError).
//   - PreToolRules (#2028) and StopRules (#2089) are read at every Start and
//     baked into that CC process's hook command line, so a source that reads
//     the live config makes an edit reach the next session (#2033).
func (b *Backend) SetHostHooks(h delegator.HostHooks) {
	b.onAuthFailure = h.OnAuthFailure
	b.onRateLimited = h.OnRateLimitNotice
	b.onModelFallback = h.OnModelFallbackNotice
	b.onSessionLimit = h.EngageRateLimit
	b.preToolRules = h.PreToolRules
	b.stopRules = h.StopRules
}

// SetOnAutonomousOpen registers a hook fired when the backend detects CC has
// begun a run foci did not open (session_state:running with no foci turn). The
// agent wires this to openAutonomousTurn, which adopts the run as a first-class
// foci turn (streams, accounts, completes like any turn) (#1261). Must be set
// before Start. fn receives the run's triggers (see Backend.onAutonomousOpen).
func (b *Backend) SetOnAutonomousOpen(fn func(triggers []string)) { b.onAutonomousOpen = fn }
