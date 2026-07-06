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

// SetRateLimitState sets the shared rate limit state that OnRateLimit writes to.
// Must be called before Start. The state is shared across all backends for an agent.
func (b *Backend) SetRateLimitState(s *RateLimitState) { b.rateLimitState = s }

// SetOnAuthFailure registers a hook fired when CC reports an authentication
// failure (a 401). Used to trigger automated re-login (#843). Must be set
// before Start.
func (b *Backend) SetOnAuthFailure(fn func(detail string)) { b.onAuthFailure = fn }
