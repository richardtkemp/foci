//go:build integration

package integration

import (
	"testing"
	"time"

	"foci/internal/testharness"
)

// TestL2_Nudges_RegexNudgePrependedToUserMessage proves the regex
// trigger path: when a user message matches a rule's regex pattern, the
// wrapped nudge text is prepended to the prompt foci hands to CC. The
// assertion reads cc-stub's user_message recorder entry for the agent's
// workdir and looks for the rule's reminder body inside the text_prefix,
// confirming foci injected the nudge before forwarding the user text.
func TestL2_Nudges_RegexNudgePrependedToUserMessage(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Nudges_TurnIntervalNudgeFiresOnSchedule proves the
// every_n_turns trigger path: with a turn-interval rule pre-seeded into
// the workspace, the first user-driven turn (turn counter rolls 0→1
// with n=1) injects the nudge ahead of the user text. Asserts the
// reminder body appears in the user_message text_prefix for that turn.
func TestL2_Nudges_TurnIntervalNudgeFiresOnSchedule(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Nudges_AfterToolsNudgeFollowsToolBatch proves the
// every_n_tools trigger path: a pre-seeded rule with n=1 plus a scripted
// Bash tool_use forces foci's PostToolNudgeFunc to fire after the tool
// batch. Asserts a SECOND user_message entry appears in the agent's
// workdir whose text_prefix carries the reminder body — i.e. the tool
// result and nudge were folded together and re-sent to CC.
func TestL2_Nudges_AfterToolsNudgeFollowsToolBatch(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Nudges_PreAnswerGateInjectsBeforeFinalReply proves the
// pre_answer trigger path: with NudgePreAnswerGate enabled, a
// pre_answer rule, and at least NudgePreAnswerMinTools tool calls
// recorded, foci re-prompts CC once before letting the answer land.
// Asserts a second user_message entry appears with the pre-answer
// reminder text, confirming the gate fired and was delivered as a
// follow-up prompt rather than dropped.
func TestL2_Nudges_PreAnswerGateInjectsBeforeFinalReply(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Nudges_FooterPresentOnAllDeliveryPaths is the regression net
// for the 2026-05-14 footer unification refactor (30f577c3). Pre-refactor
// only the pre_answer path appended the silence-vs-reply footer; the
// other three paths (turn-interval, regex, after-tools) shipped a bare
// nudge. Loads one rule per path, drives a single turn that triggers all
// three, and asserts the NoResponseSentinel marker from nudgeFooter
// appears in each corresponding user_message text_prefix.
func TestL2_Nudges_FooterPresentOnAllDeliveryPaths(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Nudges_HeaderPresentOnAllDeliveryPaths is the sibling
// regression net to the footer test: every nudge block must carry the
// "[system: automatic nudge —" header so CC treats it as background
// guidance, not user input. Asserts the header marker appears in the
// user_message text_prefix for regex, turn-interval, and after-tools
// fires.
func TestL2_Nudges_HeaderPresentOnAllDeliveryPaths(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Nudges_PerAgentRulesIsolated proves that per-agent rules files
// don't bleed into other agents. Two agents (alpha, bravo) get distinct
// nudge-rules.json files with distinct regex reminders that both match
// the same user phrase. Sends the matching phrase to each agent
// separately. Asserts alpha's user_message carries only alpha's reminder
// text, bravo's carries only bravo's — confirms the scheduler is built
// per-agent off the agent's own workspace rules file.
func TestL2_Nudges_PerAgentRulesIsolated(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Nudges_MaxPerBatchCapsAfterToolsReminders proves
// nudge_max_per_batch is honoured: two distinct every_n_tools rules
// both eligible to fire after a single tool batch, with max_per_batch=1
// in config, produce exactly one nudge in the resulting user_message
// text_prefix — not both. The negative half asserts the second rule's
// reminder body is absent.
func TestL2_Nudges_MaxPerBatchCapsAfterToolsReminders(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Nudges_CooldownSuppressesRepeatedAfterToolsNudge proves the
// cooldown gate: a single every_n_tools rule with n=1 and config
// nudge_cooldown=5 should fire on the first tool call, then be
// suppressed for the next four within the same turn. Scripts cc-stub
// to emit two Bash tool_uses back-to-back, then asserts the rule's
// reminder appears in exactly one user_message text_prefix, not both.
func TestL2_Nudges_CooldownSuppressesRepeatedAfterToolsNudge(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Nudges_AutoExtractInvocationRunsOnFirstActivity proves the
// extraction path is reached when no rules file exists and
// nudge_auto_extract is enabled. With CRAFT.md present but no
// nudge-rules.json, the first OnActivity fires `claude --print` via
// DelegatedManager.RunOnce for extraction. Asserts an EXTRA cc-stub
// invocation entry appears (beyond the long-lived agent CC) — the
// extractor's one-shot subprocess. Distinguished by either resume_id
// being empty AND a workdir that matches but a separate PID.
func TestL2_Nudges_AutoExtractInvocationRunsOnFirstActivity(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Nudges_AutoExtractSkippedWhenContentHashMatches proves the
// hash-gated skip in Extractor.NeedsExtraction: a pre-seeded
// nudge-rules.json whose content_hash matches the SHA-256 of the
// agent's character files causes the extractor to no-op. Asserts no
// extra `claude --print` invocation entry appears in the recorder
// beyond the normal agent CC spawn.
func TestL2_Nudges_AutoExtractSkippedWhenContentHashMatches(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Nudges_MalformedRulesFileToleratedAtStartup is the negative
// path for parser robustness: a nudge-rules.json containing invalid
// JSON must not crash foci-gw at startup or break the agent loop.
// Asserts the gateway reaches its "started N agent(s)" ready line AND
// a subsequent user message produces a user_message entry with no nudge
// text — foci degrades to "no rules" gracefully.
func TestL2_Nudges_MalformedRulesFileToleratedAtStartup(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Nudges_EmptyRulesArrayProducesNoNudge is the negative path
// for empty rule sets: a syntactically valid nudge-rules.json with an
// empty rules array means the scheduler has nothing to fire. Sends a
// message that WOULD match a regex if any rule existed, then asserts
// the resulting user_message text_prefix contains no nudgeHeader
// marker — i.e. nothing was injected.
func TestL2_Nudges_EmptyRulesArrayProducesNoNudge(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Nudges_InvalidRegexPatternIgnored proves the scheduler's
// graceful-degradation contract for malformed regex triggers: a rule
// with an uncompilable pattern (e.g. "[") must not crash the agent
// loop; the rule simply never fires. Asserts a normal user_message
// entry appears and contains no nudgeHeader marker.
func TestL2_Nudges_InvalidRegexPatternIgnored(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Nudges_CharacterDirRulesPathPreferred proves the path-precedence
// in nudge.RulesPath: when both {workspace}/character/nudge-rules.json
// and {workspace}/nudge-rules.json exist, the character-dir copy wins.
// Seeds the character-dir file with a regex reminder "INNER" and the
// workspace-dir file with "OUTER", sends a matching message, asserts
// "INNER" appears in the user_message text_prefix and "OUTER" does not.
func TestL2_Nudges_CharacterDirRulesPathPreferred(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}

// TestL2_Nudges_DisabledByConfigSuppressesAllInjection proves
// nudge_enable=false short-circuits the system entirely: with a regex
// rule pre-seeded AND nudge_enable=false in the agent's config block,
// a matching message produces a user_message entry whose text_prefix
// contains the user text but no nudgeHeader marker — the scheduler was
// never built, no injection happened.
func TestL2_Nudges_DisabledByConfigSuppressesAllInjection(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("not yet implemented")
}
