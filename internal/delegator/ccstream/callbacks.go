package ccstream

import (
	"time"

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

// SetOnAuthFailure registers a hook fired when CC reports an authentication
// failure (a 401). Used to trigger automated re-login (#843). Must be set
// before Start.
func (b *Backend) SetOnAuthFailure(fn func(detail string)) { b.onAuthFailure = fn }

// SetOnRateLimited registers a hook fired with a formatted rate_limit_event
// notice when CC reports the API is past the "allowed" threshold. The agent
// delivers it to the user's chat; it does NOT gate periodic work (a warning is
// not a block). Must be set before Start (#1211/#1238).
func (b *Backend) SetOnRateLimited(fn func(detail string)) { b.onRateLimited = fn }

// SetRateLimitThrottle sets a shared rate-limit warning throttle so multiple
// Backends for the same agent (main + facet sessions) don't each fire their
// own first-seen warning for the same account-wide limit. Must be set before
// Start.
func (b *Backend) SetRateLimitThrottle(t *RateLimitThrottle) { b.rlThrottle = t }

// SetOnSessionLimit registers a hook fired when CC reports a session limit was
// hit — a synthetic "You've hit your session limit · resets <time>" message,
// which (unlike a direct-API 429) never reaches classifyAPIError. The argument
// is the parsed reset instant; the agent uses it to engage the rate-limit gate.
// Must be set before Start.
func (b *Backend) SetOnSessionLimit(fn func(until time.Time)) { b.onSessionLimit = fn }

// SetOnAutonomousOpen registers a hook fired when the backend detects CC has
// begun a run foci did not open (session_state:running with no foci turn). The
// agent wires this to openAutonomousTurn, which adopts the run as a first-class
// foci turn (streams, accounts, completes like any turn) (#1261). Must be set
// before Start.
func (b *Backend) SetOnAutonomousOpen(fn func()) { b.onAutonomousOpen = fn }
