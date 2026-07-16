package codex

import (
	"foci/internal/delegator"
	"foci/internal/modelcaps"
)

// SetPermissionPromptFunc sets the function used to send permission prompts.
func (b *Backend) SetPermissionPromptFunc(fn delegator.PermissionPromptFunc) {
	b.permPromptFn = fn
}

// SetOnPromptsCleared sets a callback fired when outstanding prompts are
// resolved.
func (b *Backend) SetOnPromptsCleared(fn func()) {
	b.onPromptsCleared = fn
}

// RegisterPromptCancelListener registers a cancel listener. Currently a
// no-op — TODO: wire up when approval cancellation is implemented.
func (b *Backend) RegisterPromptCancelListener(requestID string, fn func(reason string)) {
	// TODO: implement when approval cancellation is needed.
}

// SetOnSessionReady sets a callback fired once when the thread ID is known.
func (b *Backend) SetOnSessionReady(fn func(sessionID string)) {
	b.onSessionReady = fn
}

// SetTypingFunc sets a callback to control the platform's typing indicator.
func (b *Backend) SetTypingFunc(fn func(typing bool)) {
	b.typingFunc = fn
}

// SetOnWarning registers a hook fired when the app-server emits a
// configWarning or runtime warning notification. Wired in
// agents_delegated.go to deliver the message to the user's chat as a
// system notification — same pattern as ccstream's SetOnRateLimited.
func (b *Backend) SetOnWarning(fn func(detail string)) { b.onWarning = fn }

// SetOnModelCaps registers a hook that receives the complete visible Codex
// catalogue after the app-server initialize handshake. Must be set before
// Start. The receiver publishes it into foci's backend-scoped live registry.
func (b *Backend) SetOnModelCaps(fn func(entries map[string]modelcaps.Caps)) {
	b.onModelCaps = fn
}

// fireWarning invokes the warning hook if one is registered. Safe to call
// whether or not a hook is set.
func (b *Backend) fireWarning(detail string) {
	if b.onWarning != nil {
		b.onWarning(detail)
	}
}
