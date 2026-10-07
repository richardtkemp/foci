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

// tryParseHumanArg consumes the bare --human switch at args[i].
func (h *humanFlag) tryParseHumanArg(args []string, i int) (consumed bool, next int) {
	if args[i] == "--human" {
		h.human = true
		return true, i
	}
	return false, i
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
