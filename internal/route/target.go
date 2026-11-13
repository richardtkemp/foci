// Package route is the single addressing authority for foci.
//
// It defines the canonical Target grammar that every entry point parses the
// same way (CLI flags, HTTP bodies, webhook queries, the send_to_session
// tool), the Resolver that turns a Target into a session key via one
// documented ladder, and the Receipt vocabulary describing how a target was
// resolved. There is exactly one resolution ladder — an alias that resolves
// on /send resolves identically on /branch, /webhook, and send_to_session.
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
//
// A chat alias may itself contain '?'. ParseTarget cannot know that; use
// Resolver.ParseTarget, which tries the whole rest as a literal alias before
// reading '?' as the start of params (#2158).
type Target struct {
	Agent  string
	Rest   string // "" = the agent's default session
	Create bool   // create a named session when nothing else matches
	Policy Policy
}

// ParsePolicy validates a policy name.
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
	i, err := optionsStart(raw)
	if err != nil {
		return Target{}, err
	}
	if i >= 0 {
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

// targetOptionKeys are the option names a target's "?key=value" tail may set.
var targetOptionKeys = map[string]bool{"create": true, "policy": true}

// optionsStart returns the index of the '?' that starts s's options, or -1
// when s has none. A session name or alias may contain '?' ("Tax Q&A? part
// 2"), so a '?' starts options only when it is the LAST '?' and everything
// after it is "key=value" pairs joined by '&' whose every key is a known
// option. Anything else after the '?' is part of the name and is kept
// exactly (#3098: "Tax Q&A? part 2" used to become "Tax Q&A", silently).
// A tail that mixes known options with other text is ambiguous and is an
// error, so neither reading is guessed.
func optionsStart(s string) (int, error) {
	i := strings.LastIndexByte(s, '?')
	if i < 0 {
		return -1, nil
	}
	known, other := 0, 0
	var unknown string
	for _, part := range strings.Split(s[i+1:], "&") {
		if part == "" {
			continue
		}
		key, _, hasValue := strings.Cut(part, "=")
		if hasValue && targetOptionKeys[key] {
			known++
			continue
		}
		other++
		if unknown == "" {
			unknown = part
		}
	}
	switch {
	case known == 0:
		return -1, nil
	case other > 0:
		return -1, fmt.Errorf("invalid target %q: options after '?' mixed with %q (options are create= and policy=; a session name may contain '?' only when no option follows it)", s, unknown)
	default:
		return i, nil
	}
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
