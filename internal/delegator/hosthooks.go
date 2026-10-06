package delegator

import (
	"foci/internal/delegator/pretool"
	"foci/internal/delegator/stoprule"
	"foci/internal/ratelimit"
)

// HostHooks are the gateway's callbacks into a delegated backend: what the
// backend reports up (auth failures, rate limits) and the host policy it
// enforces (pretool and stop rules). The gateway builds one set per agent,
// with no backend-specific branching, and hands it to every backend instance
// that declares CapHostHooks (#2154 Phase 3, Dick's Q6). Each backend uses the
// fields its capabilities say it honours and ignores the rest; a nil field
// means "not wired".
type HostHooks struct {
	// OnAuthFailure is fired when the backend's credentials are rejected. The
	// gateway runs the Claude Code re-login on a backend declaring CapRelogin,
	// and logs elsewhere.
	OnAuthFailure func(detail string)

	// OnRateLimitNotice delivers a human-facing utilisation notice (Claude
	// Code's rate_limit_event past the "allowed" threshold) to the session's
	// chat. Informational: it does not gate work.
	OnRateLimitNotice func(sessionKey, notice string)

	// OnModelFallbackNotice delivers a human-facing notice to the session's
	// chat when the backend's model refused and it fell back to another model
	// (Claude Code's model_refusal_fallback, #2200). Informational.
	OnModelFallbackNotice func(sessionKey, notice string)

	// EngageRateLimit engages the agent's rate-limit gate on a hard limit
	// (Claude Code's session limit, opencode's rejected usage limit).
	EngageRateLimit func(signal ratelimit.Signal)

	// PreToolRules is the live source of the PreToolUse deny rules, read at
	// every session start. Honoured by a backend declaring CapPreToolRules.
	PreToolRules func() []pretool.Rule

	// StopRules is the live source of the Stop-hook rules, read at every
	// session start. Honoured by a backend declaring CapStopRules.
	StopRules func() []stoprule.Rule
}

// HostHooksAcceptor is implemented by backends that take HostHooks
// (CapHostHooks). The gateway calls SetHostHooks once per instance, before
// Start.
type HostHooksAcceptor interface {
	SetHostHooks(h HostHooks)
}
