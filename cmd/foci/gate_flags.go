package main

import "strings"

// gateFlags holds the four activity-gate CLI flag values shared by
// `foci send`, `foci branch`, and `foci command`. The gate itself is
// evaluated server-side (checkActivityGate in cmd/foci-gw); these structs
// only carry the wire values from CLI flags / env vars into the POST body.
//
// Centralising the four flags here keeps the three subcommands DRY: each one
// parses with tryParseGateArg, fills defaults with applyEnvDefaults, and emits
// the JSON body with addToBody, instead of hand-rolling the same eight flag
// forms and four env lookups three times over.
type gateFlags struct {
	ifWarm         string // skip unless THIS session ran a turn within dur (or one is in flight) — cache-warmth
	ifCold         string // skip if THIS session ran a turn within dur (or one is in flight) — keepalive/reset shape
	ifUserActive   string // skip unless the user touched this agent within dur (or a turn is in flight)
	ifUserInactive string // skip if the user touched this agent within dur (or a turn is in flight)
}

// gateArgSpecs maps each gate flag name to its destination field. Declared once
// so parsing, defaults, and body-building all agree on the set of flags.
//
// The session gate is named for cache-WARMTH (it consults last_cache_touch —
// "did any turn run here recently", not "did a human interact"). The old
// --if-active / --if-inactive spellings (and FOCI_IF_ACTIVE / FOCI_IF_INACTIVE)
// are kept as hidden aliases so existing crontabs keep working; the JSON wire
// key stays if_active/if_inactive (internal contract, invisible to users).
func (g *gateFlags) specs() []struct {
	name    string
	aliases []string
	env     string
	envAlt  string
	body    string
	dst     *string
} {
	return []struct {
		name    string
		aliases []string
		env     string
		envAlt  string
		body    string
		dst     *string
	}{
		{"--if-warm", []string{"--if-active"}, "FOCI_IF_WARM", "FOCI_IF_ACTIVE", "if_active", &g.ifWarm},
		{"--if-cold", []string{"--if-inactive"}, "FOCI_IF_COLD", "FOCI_IF_INACTIVE", "if_inactive", &g.ifCold},
		{"--if-user-active", nil, "FOCI_IF_USER_ACTIVE", "", "if_user_active", &g.ifUserActive},
		{"--if-user-inactive", nil, "FOCI_IF_USER_INACTIVE", "", "if_user_inactive", &g.ifUserInactive},
	}
}

// tryParseGateArg attempts to consume one activity-gate flag at args[i], in
// either "--flag value" or "--flag=value" form. Returns consumed=true and the
// index to continue from (next) when it matched; consumed=false (and the arg
// is left for the caller to handle) otherwise.
//
// A bare "--flag" with no following value is deliberately NOT consumed — it
// falls through to the caller's trailing-args handling, matching the existing
// behaviour exercised by TestParseSendFlags ("--if-active without value at
// end" lands in rest).
func (g *gateFlags) tryParseGateArg(args []string, i int) (consumed bool, next int) {
	for _, s := range g.specs() {
		for _, name := range append([]string{s.name}, s.aliases...) {
			if args[i] == name {
				if i+1 < len(args) {
					*s.dst = args[i+1]
					return true, i + 1
				}
				return false, i // no value → leave for trailing-args handling
			}
			if strings.HasPrefix(args[i], name+"=") {
				*s.dst = args[i][len(name)+1:]
				return true, i
			}
		}
	}
	return false, i
}

// applyEnvDefaults fills any unset gate flag from its env var, precedence
// flag > canonical env > alias env > empty (mirroring -a/-s/-m for the primary
// env, with the legacy FOCI_IF_ACTIVE/INACTIVE honoured last).
func (g *gateFlags) applyEnvDefaults() {
	for _, s := range g.specs() {
		*s.dst = envDefault(*s.dst, s.env)
		if s.envAlt != "" {
			*s.dst = envDefault(*s.dst, s.envAlt)
		}
	}
}

// addToBody writes each non-empty gate value into the JSON request body under
// its wire key (if_active, if_inactive, …). The server reads these keys on the
// /send, /command, /wake, and /webhook endpoints.
func (g *gateFlags) addToBody(body map[string]interface{}) {
	for _, s := range g.specs() {
		if *s.dst != "" {
			body[s.body] = *s.dst
		}
	}
}
