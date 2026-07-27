package ccstream

import "os"

// bashMaxTimeoutMS raises the ceiling the CC model may request for a single
// foreground Bash tool call, from CC's built-in 600000 ms (10 min) to 20
// minutes. It is a CEILING, not a default: BASH_DEFAULT_TIMEOUT_MS is
// deliberately left alone, so a Bash call that doesn't ask for a longer
// timeout still gets CC's 120000 ms default and nothing gets slower.
//
// Why we need the higher ceiling: a CC *subagent* is never woken by a
// background-task completion notification (open upstream bug,
// anthropics/claude-code #78782 / #77578 / #76594). A subagent therefore has
// to finish long work inside ONE foreground Bash call, and the 10-minute
// ceiling makes that impossible for slow builds/tests — forcing a parent
// agent to poll and poke the subagent instead of letting it run independently.
//
// Kept as a constant rather than a [cc_backend] config field on purpose:
//   - it costs nothing when unused (raising a ceiling changes no default), so
//     there is no trade-off for an operator to tune;
//   - it is a workaround pinned to a specific upstream bug — when that bug is
//     fixed the right move is to delete this, not to retune it;
//   - an escape hatch already exists without new config surface: buildEnv
//     applies StartOptions.Env last, so a per-agent
//     [agents.backend_config] env = { BASH_MAX_TIMEOUT_MS = "…" } overrides
//     it. That is exactly how CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS is
//     handled.
//
// See https://code.claude.com/docs/en/env-vars.
const bashMaxTimeoutMS = "1200000"

// buildEnv assembles the environment for the `claude` subprocess: the
// gateway's own environment, then foci's CC-specific defaults, then the
// per-session extras from StartOptions.Env (BASH_ENV / FOCI_SOCK from the
// exec bridge, FOCI_SESSION_KEY, and any per-agent backend_config.env).
//
// Order matters — later entries win in execve — so every foci default here is
// placed BEFORE extra, keeping per-agent backend_config.env authoritative.
//
// This lives in ccstream (not in DelegatedManager's shared per-session env)
// precisely so the CC-only vars reach ONLY the Claude Code subprocess: the
// opencode, codex and api backends build their environments in their own
// packages and never see these.
func buildEnv(extra map[string]string) []string {
	env := os.Environ()

	// Turn completion is keyed to CC's session_state_changed running/idle SDK
	// events (see OnSystem / onSessionIdle) — opt-in in CC, so the backend
	// enables them itself. A per-agent backend_config.env can override for
	// debugging (the backend then falls back to complete-on-result with a
	// Warnf).
	env = append(env, "CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS=1")

	// Raise (only) the max Bash timeout the model may request per call.
	env = append(env, "BASH_MAX_TIMEOUT_MS="+bashMaxTimeoutMS)

	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}
