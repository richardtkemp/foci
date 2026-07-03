package route

import (
	"errors"
	"fmt"

	"foci/internal/session"
)

// Rung identifies which step of the resolution ladder matched a target.
// It is returned to senders in receipts so a cron job can tell "landed in the
// named session" from "fell back to the default chat".
type Rung string

const (
	RungExact   Rung = "exact"   // Rest was a full session key with an index entry
	RungNamed   Rung = "named"   // Rest matched an existing named independent session
	RungAlias   Rung = "alias"   // Rest matched a chat alias
	RungCreated Rung = "created" // Rest names a new session, created lazily on first message
	RungDefault Rung = "default" // empty Rest: the agent's default / most-recently-active session
)

var (
	// ErrNoSession means the agent has no resolvable default session yet.
	ErrNoSession = errors.New("no active session for agent")
	// ErrUnknownTarget means Rest matched nothing on the ladder and creation
	// was not permitted.
	ErrUnknownTarget = errors.New("target does not resolve to a session")
)

// Resolution is the outcome of resolving a Target to a session. Policy is
// the target's effective delivery policy, carried through so delivery code
// doesn't re-derive it.
type Resolution struct {
	SessionKey string
	Rung       Rung
	Policy     Policy
}

// Resolver turns Targets into session keys. It is the ONE resolution ladder —
// every entry point (HTTP handlers, CLI, tools, webhooks) resolves through it
// so addressing behaves identically everywhere.
type Resolver struct {
	Index *session.SessionIndex // nil tolerated: only key derivation, no existence checks
	// PreferredPlatform resolves the configured default_platform for an
	// agent (per-agent override, else global). nil / "" = no preference —
	// the most-recently-active platform wins the default-session rung.
	PreferredPlatform func(agentID string) string
}

// Resolve maps a Target to a session key via the ladder:
//
//  1. Rest empty → the agent's default session (is_default chat, else the
//     most recently active root session).
//  2. Rest is a full session key with an index entry → exact.
//  3. Rest names an existing named independent session (agent/i<rest>).
//  4. Rest is a chat alias (case-insensitive; ambiguity is an error, never a
//     silent pick).
//  5. Target.Create and Rest is a valid session name → the named session key,
//     created lazily by the first message.
//
// Errors wrap ErrNoSession, ErrUnknownTarget, or session.ErrAliasAmbiguous so
// callers can map them to distinct user-facing failures.
func (r *Resolver) Resolve(t Target) (Resolution, error) {
	if t.Agent == "" {
		return Resolution{}, fmt.Errorf("resolve target: empty agent")
	}

	policy := t.Policy
	if policy == "" {
		policy = PolicyFallback
	}

	if t.Rest == "" {
		if r.Index == nil {
			return Resolution{}, fmt.Errorf("%w: %s (no session index)", ErrNoSession, t.Agent)
		}
		preferred := ""
		if r.PreferredPlatform != nil {
			preferred = r.PreferredPlatform(t.Agent)
		}
		key := r.Index.DefaultSessionKeyForAgentOn(t.Agent, preferred)
		if key == "" {
			return Resolution{}, fmt.Errorf("%w: %s", ErrNoSession, t.Agent)
		}
		return Resolution{SessionKey: key, Rung: RungDefault, Policy: policy}, nil
	}

	// Exact session key.
	full := t.Agent + "/" + t.Rest
	if _, err := session.ParseSessionKey(full); err == nil {
		if r.Index != nil && r.Index.SessionExists(full) {
			return Resolution{SessionKey: full, Rung: RungExact, Policy: policy}, nil
		}
	}

	// Existing named independent session.
	named, nameErr := session.NamedIndependentSessionKey(t.Agent, t.Rest)
	if nameErr == nil && r.Index != nil && r.Index.SessionExists(named) {
		return Resolution{SessionKey: named, Rung: RungNamed, Policy: policy}, nil
	}

	// Chat alias.
	if r.Index != nil {
		aliasKey, aliasErr := r.Index.ResolveChatAlias(t.Agent, t.Rest)
		switch {
		case aliasErr == nil:
			return Resolution{SessionKey: aliasKey, Rung: RungAlias, Policy: policy}, nil
		case errors.Is(aliasErr, session.ErrAliasAmbiguous):
			return Resolution{}, fmt.Errorf("resolve %q: %w", t.Rest, aliasErr)
		}
	}

	// Create a named session.
	if t.Create && nameErr == nil {
		return Resolution{SessionKey: named, Rung: RungCreated, Policy: policy}, nil
	}

	if nameErr != nil {
		return Resolution{}, fmt.Errorf("%w: %q is not a valid session key, name, or known alias: %v", ErrUnknownTarget, t.Rest, nameErr)
	}
	return Resolution{}, fmt.Errorf("%w: %q (create disabled)", ErrUnknownTarget, t.Rest)
}

// Receipt reports to a sender how its target resolved: the canonical target
// string as understood by the router, the session it resolved to, and which
// ladder rung matched. Delivery-level outcomes (delivered / queued / fell
// back) are layered on by the delivery path where known.
type Receipt struct {
	Target     string `json:"target,omitempty"`
	SessionKey string `json:"session"`
	Via        Rung   `json:"resolved_via"`
	Policy     string `json:"policy,omitempty"` // non-default delivery policy
}

// ReceiptFor builds the receipt for a resolution of the given target,
// including the target's canonical string form so a sender can see exactly
// how its addressing was interpreted.
func (res Resolution) ReceiptFor(t Target) Receipt {
	return Receipt{Target: t.String(), SessionKey: res.SessionKey, Via: res.Rung}
}
