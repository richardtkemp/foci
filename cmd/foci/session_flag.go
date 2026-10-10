package main

// sessionFlag holds the -s/--session selector shared by `foci send`,
// `foci branch` and `foci score` (#2284): the session the request targets —
// a full session key, a session name, or a chat alias — resolved server-side
// through the one route ladder. An empty selector targets the agent's default
// session.
//
// Centralised here for the same reason as gateFlags/waitFlags/humanFlag: the
// subcommands parse, env-default and emit it identically, from one
// declaration, instead of hand-rolling the same four flag forms and env
// lookup per command (branch used to have none of them, and its -s words
// silently became message text; score kept a private copy).
type sessionFlag struct {
	// session is the selector value. Embedded sessionFlags promote it to
	// flags.session, the same promoted-field shape as gateFlags.ifWarm.
	session string
}

// specs declares the flag once — long and short spellings, env var, and
// JSON wire key — so parsing, env-defaulting and body-building cannot drift
// apart (the gateFlags pattern over flagSpec).
func (sf *sessionFlag) specs() []flagSpec {
	return []flagSpec{
		{"--session", []string{"-s"}, "FOCI_SESSION", "", "session", &sf.session},
	}
}

// tryParseSessionArg consumes one -s/--session value-flag at args[i], in
// both the "--flag value" and "--flag=value" shapes. A bare valueless flag
// at the end of the args is left for trailing-args handling (consumed=false),
// exactly as send always treated it.
func (sf *sessionFlag) tryParseSessionArg(args []string, i int) (consumed bool, next int) {
	return tryParseFlagArg(sf.specs(), args, i)
}

// stripArgs removes every -s/--session flag (both shapes) from args and
// returns the rest, keeping the LAST value — the extract-style entry point
// for subcommands that pull the flag out up front (score) instead of
// filtering tokens in a parse loop (send, branch).
func (sf *sessionFlag) stripArgs(args []string) (rest []string) {
	for i := 0; i < len(args); i++ {
		if c, ni := sf.tryParseSessionArg(args, i); c {
			i = ni
			continue
		}
		rest = append(rest, args[i])
	}
	return rest
}

// applyEnvDefault fills an unset selector from FOCI_SESSION (flag > env >
// default: the agent's default session, i.e. no session key on the body).
func (sf *sessionFlag) applyEnvDefault() { applyFlagEnvDefaults(sf.specs()) }

// addToBody writes a non-empty selector into the JSON body under "session".
// An empty selector leaves no session key, so the gateway resolves the
// agent's default session — the historical default, unchanged.
func (sf *sessionFlag) addToBody(body map[string]interface{}) { addFlagsToBody(sf.specs(), body) }
