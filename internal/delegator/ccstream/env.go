package ccstream

import "foci/internal/procx"

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

// ccMaxRetries is CC's API retry budget (CLAUDE_CODE_MAX_RETRIES), raised
// from CC's default 10 to 15 so a subagent rides out a longer 529 Overloaded
// spell instead of dying with "API Error: 529 Overloaded" and leaving the
// orchestrating agent to resume it by hand (#2186).
//
// Verified against the CC 2.1.289 binary, not docs:
//   - backoff is 500ms doubling, capped at 32s, plus 0-25% jitter, so 10
//     retries wait ~160-200s and 15 wait ~320-400s;
//   - a subagent's query source ("agent:*") is treated as foreground, so its
//     529s are retried under this same budget (only true background sources
//     drop a 529 immediately);
//   - 15 is the ceiling CC honours: a higher value is clamped to 15 with a
//     warning unless CLAUDE_CODE_RETRY_WATCHDOG is set.
//
// CLAUDE_CODE_RETRY_WATCHDOG is deliberately NOT set: it switches 529 and
// 429 to persistent retry with no attempt cap (each wait up to 5 min, a quota
// 429 up to 6 h), so total retry time would no longer be bounded and an
// exhausted plan would hang an agent instead of failing.
//
// The budget covers every retryable error (5xx, 408/409, connection drops),
// not only 529; all of those are transient, and a 529-rejected request costs no
// tokens. Non-retryable errors still fail at once. Same constant-not-config
// reasoning as bashMaxTimeoutMS: a per-agent backend_config.env overrides it.
const ccMaxRetries = "15"

// buildEnv assembles the environment for the `claude` subprocess: the
// OPERATOR population (procx.Env — the daemon's env overlaid with the
// operator's captured shell env, never the daemon's mutated global), then
// foci's CC-specific defaults, then the
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
	env := procx.Env(procx.Operator)

	// Turn completion is keyed to CC's session_state_changed running/idle SDK
	// events (see OnSystem / onSessionIdle) — opt-in in CC, so the backend
	// enables them itself. A per-agent backend_config.env can override for
	// debugging (the backend then falls back to complete-on-result with a
	// Warnf).
	env = append(env, "CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS=1")

	// Raise (only) the max Bash timeout the model may request per call.
	env = append(env, "BASH_MAX_TIMEOUT_MS="+bashMaxTimeoutMS)

	// Longer bounded retry window for 529 Overloaded (and other transient
	// API errors), main thread and subagents alike.
	env = append(env, "CLAUDE_CODE_MAX_RETRIES="+ccMaxRetries)

	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}
