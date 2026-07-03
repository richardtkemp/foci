// Package route is the single addressing authority for foci.
//
// It defines the canonical Target grammar that every entry point parses the
// same way (CLI flags, HTTP bodies, webhook queries, the send_to_session
// tool), the Resolver that turns a Target into a session key via one
// documented ladder, and the Receipt vocabulary describing how a target was
// resolved. There is exactly one resolution ladder — an alias that resolves
// on /send resolves identically on /wake, /webhook, and send_to_session.
package route

import (
	"fmt"
	"net/url"
	"strings"
)

// Policy selects the fallback behaviour when a target's session has no live
// delivery path. Resolution of the session itself is policy-independent;
// policy is consumed by the delivery layer.
type Policy string

const (
	// PolicyFallback (default): deliver to the resolved session's connection,
	// falling back to the owning platform's primary connection when the
	// session has no live one.
	PolicyFallback Policy = "fallback"
	// PolicyStrict: deliver to the resolved session's connection or not at
	// all — never land the message somewhere the sender didn't name.
	PolicyStrict Policy = "strict"
	// PolicyBroadcast: deliver to every live connection for the agent.
	PolicyBroadcast Policy = "broadcast"
)

// Target is a parsed addressing intent: which agent, and optionally which
// session within it.
//
// Canonical string form:
//
//	agent[/rest][?create=bool&policy=strict|fallback|broadcast]
//
//	clutch                       the agent's default session
//	clutch/c123                  exact session key
//	clutch/research              named session or chat alias
//	clutch/research?create=false named session, do not create if missing
//	clutch?policy=strict         default session, no delivery fallback
//
// Rest is resolved by Resolver.Resolve through one ladder: exact key →
// existing named session → chat alias → create-named (when Create).
type Target struct {
	Agent  string
	Rest   string // "" = the agent's default session
	Create bool   // create a named session when nothing else matches
	Policy Policy
}

// ParsePolicy validates a policy name. PolicyRootFallback is internal-only
// (senders choose it in code, not on the wire).
func ParsePolicy(s string) (Policy, error) {
	switch Policy(s) {
	case PolicyFallback, PolicyStrict, PolicyBroadcast:
		return Policy(s), nil
	default:
		return "", fmt.Errorf("invalid target policy %q (want strict, fallback, or broadcast)", s)
	}
}

// ParseTarget parses the canonical target string form. Create defaults to
// true (a cron job addressing clutch/nightly-report just works on first run);
// Policy defaults to PolicyFallback.
func ParseTarget(s string) (Target, error) {
	t := Target{Create: true, Policy: PolicyFallback}

	raw := s
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		q, err := url.ParseQuery(raw[i+1:])
		if err != nil {
			return Target{}, fmt.Errorf("invalid target params %q: %w", raw[i+1:], err)
		}
		raw = raw[:i]
		if v := q.Get("create"); v != "" {
			t.Create = v == "true" || v == "1"
		}
		if v := q.Get("policy"); v != "" {
			policy, err := ParsePolicy(v)
			if err != nil {
				return Target{}, err
			}
			t.Policy = policy
		}
	}

	agent, rest, _ := strings.Cut(raw, "/")
	if agent == "" {
		return Target{}, fmt.Errorf("invalid target %q: empty agent", s)
	}
	t.Agent = agent
	t.Rest = rest
	return t, nil
}

// String renders the canonical form. Round-trips through ParseTarget.
// Non-default params are included explicitly.
func (t Target) String() string {
	var sb strings.Builder
	sb.WriteString(t.Agent)
	if t.Rest != "" {
		sb.WriteByte('/')
		sb.WriteString(t.Rest)
	}
	params := url.Values{}
	if !t.Create {
		params.Set("create", "false")
	}
	if t.Policy != "" && t.Policy != PolicyFallback {
		params.Set("policy", string(t.Policy))
	}
	if len(params) > 0 {
		sb.WriteByte('?')
		sb.WriteString(params.Encode())
	}
	return sb.String()
}
