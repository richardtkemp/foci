//go:build integration

package integration

import (
	"testing"

	"foci/internal/testharness"
)

// Slash commands are foci's client-side command surface. The Telegram
// bot's interceptor pipeline parses every inbound text message: if it
// begins with `/` (or `.` as an alias), the command registry dispatches
// it directly and the agent never sees it. The L2 suite below proves
// that mechanism — message text in via TelegramStub.PushUpdate, command
// outcome out via TelegramStub.PeekSent — without touching real claude
// or real Telegram. Each scenario asserts only on observable side
// effects: outbound sendMessage bodies, cc-stub recorder entries, or
// the absence thereof. Anything that needs a real model, real Telegram
// formatting/voice TTS, or wall-clock progression is L3 and is not
// listed here.

// TestL2_SlashCommands_HelpListsRegisteredCommands proves /help is
// dispatched client-side and returns a sendMessage whose body
// enumerates the registered command set. Confirms HelpCommand's
// registry walk is wired through the interceptor and that the reply
// path bypasses cc-stub entirely.
func TestL2_SlashCommands_HelpListsRegisteredCommands(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_HelpDoesNotInvokeCCStub proves /help is a
// pure client-side command — no cc-stub invocation entry appears
// after the command runs, so we are not paying mana for a model
// turn on a foci-internal command.
func TestL2_SlashCommands_HelpDoesNotInvokeCCStub(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_PingReturnsPong proves the simplest
// liveness check — /ping — round-trips through the interceptor and
// produces a "pong" sendMessage with a timestamp. Bare smoke test
// for the command dispatch path.
func TestL2_SlashCommands_PingReturnsPong(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_DotPrefixAlias proves `.ping` is treated as
// `/ping` — the dot-prefix shortcut documented in COMMANDS.md (easier
// to type on a phone keyboard). Asserts the same sendMessage shape as
// the slash form.
func TestL2_SlashCommands_DotPrefixAlias(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_DotPrefixNonCommandPassesThrough proves a
// `.something` where "something" is not a registered command falls
// through to the agent as normal text — i.e. cc-stub receives a
// user_message containing the literal ".something". This protects
// against the dot-prefix alias eating common phone-typed messages.
func TestL2_SlashCommands_DotPrefixNonCommandPassesThrough(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_UnknownCommandSuggestsAlternatives proves an
// unrecognised /foo emits a "Unknown command /foo. Did you mean ...?"
// reply built from the registry's Levenshtein-distance suggester. The
// reply MUST come via sendMessage; cc-stub MUST NOT be invoked.
func TestL2_SlashCommands_UnknownCommandSuggestsAlternatives(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_ResetClearsSession proves /reset dispatches the
// soft reset path: a confirmation sendMessage ("Session reset..." /
// "Session cleared.") arrives, and a follow-up user message lands in
// the recorder under a *different* session id than before the reset,
// proving the session key rotated.
func TestL2_SlashCommands_ResetClearsSession(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_ResetHardCancelsInflightTurn proves /reset hard
// runs inline in the polling goroutine (Subcommand.Immediate=true) and
// cancels a live cc-stub turn. Mechanism: configure cc-stub with
// CCSTUB_HANG so the first message blocks, then send /reset hard. The
// hung subprocess must be killed and the confirmation sendMessage
// ("Session reset (hard)...") must arrive without the original turn
// ever completing.
func TestL2_SlashCommands_ResetHardCancelsInflightTurn(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_ReloadReturnsSkillCount proves /reload reloads
// workspace files and skills and returns a sendMessage containing the
// skill count and a note that foci.toml changes need a service
// restart. Confirms the ReloadSystem path is reachable from the
// command registry without invoking cc-stub.
func TestL2_SlashCommands_ReloadReturnsSkillCount(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_ReloadPicksUpEditedWorkspaceFile proves an
// in-flight edit to a character/workspace file under the agent's
// workspace is reflected on the NEXT cc-stub invocation after /reload:
// the cc-stub init-args (system prompt segment surface) carries the
// new file contents. Reload must not pick up foci.toml — config
// changes still need a restart per the command's reply text.
func TestL2_SlashCommands_ReloadPicksUpEditedWorkspaceFile(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_ErrorsTailsEventLog proves /errors returns only
// ERROR/WARN level lines from the configured event log file. Seeds
// the log with a mix of INFO/WARN/ERROR lines (via a side-channel
// write to cc.EventLogPath) and asserts the reply contains the WARN
// and ERROR but not the INFO entries, wrapped in a fenced code block.
func TestL2_SlashCommands_ErrorsTailsEventLog(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_ErrorsMissingLogFile proves /errors does NOT
// 404 or panic when the event log file is absent — it should reply
// "Log file not found." via sendMessage. This is the regression net
// for the comment in the original placeholder: "should return recent
// ERROR/WARN log lines, not 404".
func TestL2_SlashCommands_ErrorsMissingLogFile(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_ErrorsRespectsLineCountArg proves /errors 5
// honours its line-count arg and caps the reply at 5 matching lines.
// Negative paths: non-numeric argument (e.g. /errors foo) falls back
// to the default of 10 rather than erroring.
func TestL2_SlashCommands_ErrorsRespectsLineCountArg(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_ManaReportsNoProviderSupport proves the dynamic
// /mana command is registered only when the provider's UsageClient is
// non-nil, but the test harness uses a provider without usage support,
// so a /mana sent to a default agent reply must surface as "Unknown
// command" via the suggester — NOT as a panic and NOT as a fake
// percentage.
func TestL2_SlashCommands_ManaReportsNoProviderSupport(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_CostTodayReadsAPILog proves /cost today aggregates
// rows from the configured api.db / api log and emits a sendMessage
// with a "Today: $X.XX eq." header and per-session table. Seeds the
// api log with two synthetic entries dated today; asserts both
// session names appear and the total matches the seeded sum.
func TestL2_SlashCommands_CostTodayReadsAPILog(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_CostUnknownPeriodShowsUsage proves /cost with
// an unrecognised period (e.g. /cost banana) replies with the usage
// string listing the supported subcommands rather than crashing or
// dispatching to cc-stub.
func TestL2_SlashCommands_CostUnknownPeriodShowsUsage(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_ModeBareShowsCurrent proves /mode (no args)
// returns the current permission mode label + options hint. For a
// freshly-started ccstream-backed agent the value is "normal
// (default)" — confirms the displayMode reverse-mapping from CC's
// "default" wire value to the user-friendly "normal" label.
func TestL2_SlashCommands_ModeBareShowsCurrent(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_ModeSwitchToAccept proves /mode accept (and its
// aliases "2", "edits", "acceptedits") routes through
// Agent.SetPermissionMode with the CC-native value "acceptEdits" and
// surfaces the success reply. After the switch, a subsequent /mode
// bare-query must report "accept" — proving the session metadata
// actually changed.
func TestL2_SlashCommands_ModeSwitchToAccept(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_ModeInvalidValueReturnsError proves /mode
// banana replies with an "Invalid permission mode" message and the
// options hint, without mutating session metadata. A follow-up bare
// /mode must still report the previous mode.
func TestL2_SlashCommands_ModeInvalidValueReturnsError(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_StaleCommandDropped proves a slash command
// whose Telegram message Date is older than dispatch.StaleCommandAge
// (30s) is silently dropped: no sendMessage, no cc-stub invocation,
// and a WARN line in foci-gw stderr. Protects against replay storms
// after a foci restart drains old getUpdates.
func TestL2_SlashCommands_StaleCommandDropped(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_NeverReachCCStub proves the general invariant:
// for every command in the foci-internal set (/help, /ping, /reset,
// /reload, /errors, /cost, /mode, /version, /status), the cc-stub
// recorder records ZERO user_message entries with text_prefix
// starting with "/" for that agent. The negative assertion catches
// regressions where a future change forwards command text into the
// agent pipeline.
func TestL2_SlashCommands_NeverReachCCStub(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_PassForwardsToBackend proves /pass <text>
// bypasses foci's command dispatch and forwards the raw remainder to
// the delegated backend — cc-stub records the forwarded text
// (without the leading "/pass ") as a user_message. This is the
// escape hatch documented for running CC's own slash commands like
// /pass /context.
func TestL2_SlashCommands_PassForwardsToBackend(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_RepeatResendsLastMessage proves the hidden //
// (repeat) command replays the user's most recent non-command
// message: cc-stub records two user_message entries with the same
// text. Negative path: // sent before any prior message must reply
// "no previous message to repeat".
func TestL2_SlashCommands_RepeatResendsLastMessage(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_VersionReportsBuildInfo proves /version emits a
// sendMessage containing the version, go version, commit, and build
// time strings populated in cmdRegParams.BuildInfo. Confirms the
// build-info plumbing reaches the command without going through
// cc-stub.
func TestL2_SlashCommands_VersionReportsBuildInfo(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SlashCommands_StopCancelsInflightTurn proves /stop runs
// Immediate=true and cancels an in-flight cc-stub turn (configured
// with CCSTUB_HANG). The "Stopped." sendMessage must arrive even
// though the original message would otherwise have hung the worker
// goroutine indefinitely.
func TestL2_SlashCommands_StopCancelsInflightTurn(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}
