package command

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"foci/internal/session"
	"foci/internal/timeutil"
)

// --- Duration kinds ---

type costDurKind int

const (
	durNone      costDurKind = iota // no duration specified (all time)
	durToday                        // since midnight today
	durWindow                       // last Dur (24h, week, Go-duration, N-days)
)

// --- Parsed args ---

// costArgs holds the parsed arguments for /cost. Duration and scope are
// orthogonal: the time predicate filters by timestamp, scope predicates
// filter by session key. Multiple scopes intersect (entry must match ALL).
type costArgs struct {
	durKind   costDurKind
	durDur    time.Duration // actual window for durWindow
	durLabel  string        // display label ("today", "24h", "7 days", "4h")
	scopes    []string      // normalised scope keywords
	breakdown bool
}

// scopeAliases maps user-facing scope synonyms to their canonical form.
var scopeAliases = map[string]string{
	"session":     "session",
	"self":        "session",
	"strict-self": "strict-self",
	"descendant":  "descendants",
	"descendants": "descendants",
	"fork":        "descendants",
	"forks":       "descendants",
	"branch":      "descendants",
	"branches":    "descendants",
	"agent":       "agent",
}

// sessionTypeScopes is the set of session_type values accepted as scopes.
var sessionTypeScopes = map[string]bool{
	string(session.SessionTypeChat):          true,
	string(session.SessionTypeFacet):         true,
	string(session.SessionTypeIndependent):   true,
	string(session.SessionTypeSpawn):         true,
	string(session.SessionTypeReflection):    true,
	string(session.SessionTypeKeepalive):     true,
	string(session.SessionTypeBackgroundTask): true,
}

// parseCostArgs tokenises the args string and classifies each token as a
// duration, scope, or the breakdown modifier. At most one duration is
// accepted; an error is returned if a second duration token appears.
func parseCostArgs(args string) (costArgs, error) {
	fields := strings.Fields(args)
	var result costArgs
	for _, f := range fields {
		lower := strings.ToLower(f)
		if lower == "breakdown" {
			result.breakdown = true
			continue
		}
		if dk, dur, label, ok := tryParseCostDuration(lower); ok {
			if result.durKind != durNone {
				return costArgs{}, fmt.Errorf("multiple durations in /cost args")
			}
			result.durKind = dk
			result.durDur = dur
			result.durLabel = label
			continue
		}
		if canonical, ok := scopeAliases[lower]; ok {
			result.scopes = append(result.scopes, canonical)
			continue
		}
		if sessionTypeScopes[lower] {
			result.scopes = append(result.scopes, lower)
			continue
		}
		return costArgs{}, fmt.Errorf("unknown /cost argument %q", f)
	}
	return result, nil
}

// tryParseCostDuration attempts to classify a token as a duration.
// Recognised forms: "today", "24h", "week", Go duration strings ("4h",
// "30m"), and bare integers (days).
func tryParseCostDuration(s string) (kind costDurKind, dur time.Duration, label string, ok bool) {
	switch s {
	case "today":
		return durToday, 0, "today", true
	case "24h":
		return durWindow, 24 * time.Hour, "24h", true
	case "week":
		return durWindow, 7 * 24 * time.Hour, "7 days", true
	}
	if d, err := time.ParseDuration(s); err == nil {
		return durWindow, d, s, true
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return durWindow, time.Duration(n) * 24 * time.Hour, fmt.Sprintf("%d days", n), true
	}
	return durNone, 0, "", false
}

// --- Time predicate ---

// timePredicate returns a function that reports whether a timestamp falls
// within the window implied by the parsed duration.
func (a costArgs) timePredicate() func(time.Time) bool {
	switch a.durKind {
	case durToday:
		today := timeutil.Now().Format("2006-01-02")
		return func(t time.Time) bool {
			return t.Local().Format("2006-01-02") == today
		}
	case durWindow:
		cutoff := time.Now().Add(-a.durDur)
		// For multi-day windows, use start-of-day cutoff like the old
		// costWeek (days aligned to calendar boundaries).
		if a.durDur >= 24*time.Hour {
			now := timeutil.Now()
			startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
			days := int(a.durDur / (24 * time.Hour))
			cutoff = startOfToday.AddDate(0, 0, -(days - 1))
		}
		return func(t time.Time) bool {
			return !t.Before(cutoff)
		}
	default:
		return func(time.Time) bool { return true }
	}
}

// --- Scope resolution ---

// scopePredicate builds a session-key predicate from the scope list.
// When multiple scopes are present the predicate is the intersection:
// an entry's session must be in every scope's key set.
func scopePredicate(scopes []string, sessionKey string, idx *session.SessionIndex) (func(string) bool, string) {
	if len(scopes) == 0 {
		return func(string) bool { return true }, ""
	}
	if idx == nil {
		// Without an index, only strict-self resolves (to the bare key).
		// Other scopes match nothing.
		var labelParts []string
		pred := func(sk string) bool { return true }
		for _, scope := range scopes {
			var keys map[string]struct{}
			var label string
			switch scope {
			case "strict-self":
				keys = map[string]struct{}{sessionKey: {}}
				label = "this session only"
			case "session":
				keys = map[string]struct{}{sessionKey: {}}
				label = "this session"
			default:
				label = scope
			}
			labelParts = append(labelParts, label)
			prev := pred
			pred = func(sk string) bool {
				if keys == nil {
					return false
				}
				_, ok := keys[sk]
				return prev(sk) && ok
			}
		}
		return pred, strings.Join(labelParts, " ∩ ")
	}

	var labelParts []string
	pred := func(sk string) bool { return true }
	for _, scope := range scopes {
		keys, label := resolveScopeKeys(scope, sessionKey, idx)
		labelParts = append(labelParts, label)
		prev := pred
		pred = func(sk string) bool {
			_, ok := keys[sk]
			return prev(sk) && ok
		}
	}
	label := strings.Join(labelParts, " ∩ ")
	return pred, label
}

// resolveScopeKeys returns the set of session keys matching a single scope
// keyword, plus a human-readable label.
func resolveScopeKeys(scope, sessionKey string, idx *session.SessionIndex) (map[string]struct{}, string) {
	switch scope {
	case "session":
		family, _ := sessionFamily(idx, sessionKey)
		return family, "this session"
	case "strict-self":
		return map[string]struct{}{sessionKey: {}}, "this session only"
	case "descendants":
		family, _ := sessionFamily(idx, sessionKey)
		result := make(map[string]struct{}, len(family))
		for k := range family {
			if k != sessionKey {
				result[k] = struct{}{}
			}
		}
		return result, "descendants"
	case "agent":
		agentID := agentFromSession(sessionKey)
		entries, err := idx.Query(session.QueryOptions{AgentID: agentID})
		if err != nil {
			return map[string]struct{}{}, "agent " + agentID
		}
		set := make(map[string]struct{}, len(entries))
		for _, e := range entries {
			set[e.SessionKey] = struct{}{}
		}
		return set, "agent " + agentID
	default:
		// Session type scope
		entries, err := idx.Query(session.QueryOptions{SessionType: scope})
		if err != nil {
			return map[string]struct{}{}, "type:" + scope
		}
		set := make(map[string]struct{}, len(entries))
		for _, e := range entries {
			set[e.SessionKey] = struct{}{}
		}
		return set, "type:" + scope
	}
}

// hasSessionScope reports whether any scope narrows to the current session
// family (session, strict-self, or descendants).
func hasSessionScope(scopes []string) bool {
	for _, s := range scopes {
		switch s {
		case "session", "strict-self", "descendants":
			return true
		}
	}
	return false
}
