package main

// waitFlags holds the "wait-until" gate flags, exclusive to `foci send`: unlike
// the if-* gates (which skip when unmet), an unmet wait-* condition defers the
// send server-side until it holds (or --wait-timeout / --deadline elapses, then
// it sends anyway). --no-gate opts out of both the wait default and any gating.
type waitFlags struct {
	waitWarm         string
	waitCold         string
	waitUserActive   string
	waitUserInactive string
	waitTimeout      string
	noGate           bool
}

func (wf *waitFlags) specs() []flagSpec {
	return []flagSpec{
		{"--wait-warm", []string{"--wait-active"}, "FOCI_WAIT_WARM", "", "wait_warm", &wf.waitWarm},
		{"--wait-cold", []string{"--wait-inactive"}, "FOCI_WAIT_COLD", "", "wait_cold", &wf.waitCold},
		{"--wait-user-active", nil, "FOCI_WAIT_USER_ACTIVE", "", "wait_user_active", &wf.waitUserActive},
		{"--wait-user-inactive", nil, "FOCI_WAIT_USER_INACTIVE", "", "wait_user_inactive", &wf.waitUserInactive},
		{"--wait-timeout", []string{"--deadline"}, "FOCI_WAIT_TIMEOUT", "", "wait_timeout", &wf.waitTimeout},
	}
}

// tryParseWaitArg consumes one wait value-flag, or the bare --no-gate switch.
func (wf *waitFlags) tryParseWaitArg(args []string, i int) (consumed bool, next int) {
	if args[i] == "--no-gate" {
		wf.noGate = true
		return true, i
	}
	return tryParseFlagArg(wf.specs(), args, i)
}

func (wf *waitFlags) applyEnvDefaults() {
	applyFlagEnvDefaults(wf.specs())
	wf.noGate = envBool(wf.noGate, "FOCI_NO_GATE")
}

func (wf *waitFlags) addToBody(body map[string]interface{}) {
	addFlagsToBody(wf.specs(), body)
	if wf.noGate {
		body["wait_none"] = true
	}
}
