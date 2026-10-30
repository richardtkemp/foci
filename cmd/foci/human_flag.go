package main

// humanFlag holds the --human switch shared by `foci send`, `foci branch`
// and `foci command` (#1130): the caller declares a human — not a cron job
// or script — sent this request, so the gateway counts it as user attention
// (last_user_activity_at) once it dispatches. A bare switch, no value;
// absent or false keeps today's behaviour (automated, like a cron).
//
// Centralised here for the same reason as gateFlags/waitFlags: the three
// subcommands parse, env-default and emit it identically instead of
// hand-rolling the same switch and env lookup three times over.
type humanFlag struct {
	human bool
}

// isHumanWord reports whether tok is the --human spelling — the word half
// of tryParseHumanArg.
func (h *humanFlag) isHumanWord(tok string) bool { return tok == "--human" }

// tryParseHumanArg consumes the bare --human switch token tok. --human
// takes no value, so unlike the gate/wait/session parses there is no next
// index to report — the caller's scan always resumes at the next token.
func (h *humanFlag) tryParseHumanArg(tok string) bool {
	if !h.isHumanWord(tok) {
		return false
	}
	h.human = true
	return true
}

// applyEnvDefault fills an unset flag from FOCI_HUMAN (non-empty = true);
// an explicit --human wins, matching every other bool flag.
func (h *humanFlag) applyEnvDefault() {
	h.human = envBool(h.human, "FOCI_HUMAN")
}

// addToBody writes "human": true only when set — an absent flag leaves no
// human key in the body at all, exactly like wait_none.
func (h *humanFlag) addToBody(body map[string]interface{}) {
	if h.human {
		body["human"] = true
	}
}
