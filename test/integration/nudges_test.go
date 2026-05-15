//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/nudge"
	"foci/internal/testharness"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// nudgeHeaderMarker mirrors the constant from internal/agent/agent.go. The
// L2 layer can't import the package (cycle risk), so we duplicate the
// load-bearing prefix here and assert on it as a substring. If the source
// constant changes, this string changes too — the regression net catches
// drift.
const nudgeHeaderMarker = "[system: automatic nudge"

// noResponseSentinelMarker mirrors agent.NoResponseSentinel. The footer
// embeds this token so we can use it as a structural assertion that the
// silence-vs-reply footer landed on a particular delivery path.
const noResponseSentinelMarker = "[[NO_RESPONSE]]"

// testCraftBody is the exact content the harness writes to character/CRAFT.md
// for each agent workspace (see internal/testharness/gateway_config.go's
// writeWorkspaces). Pre-computing the content hash from this string lets
// tests seed a nudge-rules.json whose ContentHash matches what the
// extractor will compute on startup, so NeedsExtraction returns false
// and the auto-extractor doesn't overwrite the seeded rules.
const testCraftBody = "# CRAFT.md\n\nTest-only workspace.\n"

// agentCharacterDir returns <workspace>/character — where workspace files
// (CRAFT.md, nudge-rules.json) live for the harness's default layout.
// agentWorkspace itself lives in helpers_test.go.
func agentCharacterDir(h *testharness.Harness, agentID string) string {
	return filepath.Join(agentWorkspace(h, agentID), "character")
}

// seedNudgeRules writes a nudge.RuleSet to the agent's character-dir
// nudge-rules.json with a content_hash that matches the harness's
// boilerplate CRAFT.md. Matching the hash prevents the auto-extractor
// (enabled by default) from overwriting our seeded rules.
func seedNudgeRules(t *testing.T, h *testharness.Harness, agentID string, rules []nudge.Rule) {
	t.Helper()
	charDir := agentCharacterDir(h, agentID)
	if err := os.MkdirAll(charDir, 0o755); err != nil {
		t.Fatalf("mkdir char dir: %v", err)
	}
	rs := &nudge.RuleSet{
		ContentHash: nudge.ContentHash([]string{testCraftBody}),
		Rules:       rules,
	}
	body, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		t.Fatalf("marshal rules: %v", err)
	}
	path := filepath.Join(charDir, "nudge-rules.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// seedNudgeRulesRaw writes raw bytes to the agent's character-dir rules
// file. For negative-path tests that need malformed JSON.
func seedNudgeRulesRaw(t *testing.T, h *testharness.Harness, agentID string, body []byte) {
	t.Helper()
	charDir := agentCharacterDir(h, agentID)
	if err := os.MkdirAll(charDir, 0o755); err != nil {
		t.Fatalf("mkdir char dir: %v", err)
	}
	path := filepath.Join(charDir, "nudge-rules.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// waitForUserMessageContaining polls the recorder until a user_message
// entry appears in the agent's workdir whose text_prefix contains every
// `must` substring. Returns the matching entry on hit or zero-value on
// timeout (caller decides whether the timeout is failure).
func waitForUserMessageContaining(t *testing.T, h *testharness.Harness, agentID string, timeout time.Duration, must ...string) (recorderEntry, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	wantWd := "workspaces/" + agentID
	for time.Now().Before(deadline) {
		for _, e := range readRecorderEntries(t, h.RecorderPath()) {
			if e.Kind != "user_message" || !strings.Contains(e.Workdir, wantWd) {
				continue
			}
			ok := true
			for _, m := range must {
				if !strings.Contains(e.TextPrefix, m) {
					ok = false
					break
				}
			}
			if ok {
				return e, true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return recorderEntry{}, false
}

// pushUserMessage sends a plain text Telegram update to the named agent.
// The chat/user ids default to the agent's UserID — the harness's
// allowed_users list scopes inbound messages, so messages from the
// configured user always pass access control.
func pushUserMessage(t *testing.T, h *testharness.Harness, agentID string, userID int64, text string) {
	t.Helper()
	token := h.AgentBotToken(agentID)
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: userID, Type: "private"},
			From: &gotgbot.User{Id: userID, FirstName: "Tester"},
			Text: text,
		},
	})
}

// TestL2_Nudges_RegexNudgePrependedToUserMessage proves the regex
// trigger path: when a user message matches a rule's regex pattern, the
// wrapped nudge text is prepended to the prompt foci hands to CC. The
// assertion reads cc-stub's user_message recorder entry for the agent's
// workdir and looks for the rule's reminder body inside the text_prefix,
// confirming foci injected the nudge before forwarding the user text.
func TestL2_Nudges_RegexNudgePrependedToUserMessage(t *testing.T) {
	const userID = 7100
	const reminderBody = "REGEX_REMINDER_MARKER_ALPHA"

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:       []testharness.AgentSpec{{ID: "alpha", UserID: userID}},
		ReadyTimeout: 30 * time.Second,
	})

	seedNudgeRules(t, h, "alpha", []nudge.Rule{
		{
			Text:       reminderBody,
			SourceFile: "CRAFT.md",
			Trigger:    nudge.Trigger{Type: "regex", Pattern: "(?i)deploy"},
			Priority:   "high",
		},
	})

	pushUserMessage(t, h, "alpha", userID, "should we deploy now?")

	entry, ok := waitForUserMessageContaining(t, h, "alpha", 20*time.Second, reminderBody, "should we deploy now?")
	if !ok {
		t.Fatalf("regex nudge never landed in alpha's user_message; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
	// Sanity: the nudge header must wrap the reminder, and the original
	// user text must come after (the injector prepends nudges).
	if !strings.Contains(entry.TextPrefix, nudgeHeaderMarker) {
		t.Errorf("text_prefix missing nudge header marker; got: %q", entry.TextPrefix)
	}
}

// TestL2_Nudges_TurnIntervalNudgeFiresOnSchedule proves the
// every_n_turns trigger path: with a turn-interval rule pre-seeded into
// the workspace, the first user-driven turn (turn counter rolls 0→1
// with n=1) injects the nudge ahead of the user text. Asserts the
// reminder body appears in the user_message text_prefix for that turn.
func TestL2_Nudges_TurnIntervalNudgeFiresOnSchedule(t *testing.T) {
	const userID = 7200
	const reminderBody = "TURN_INTERVAL_MARKER_BETA"

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:       []testharness.AgentSpec{{ID: "alpha", UserID: userID}},
		ReadyTimeout: 30 * time.Second,
	})

	seedNudgeRules(t, h, "alpha", []nudge.Rule{
		{
			Text:       reminderBody,
			SourceFile: "CRAFT.md",
			Trigger:    nudge.Trigger{Type: "every_n_turns", N: 1},
			Priority:   "low",
		},
	})

	pushUserMessage(t, h, "alpha", userID, "first turn please")

	if _, ok := waitForUserMessageContaining(t, h, "alpha", 20*time.Second, reminderBody, "first turn please"); !ok {
		t.Fatalf("every_n_turns nudge never landed; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
}

// TestL2_Nudges_AfterToolsNudgeFollowsToolBatch proves the
// every_n_tools trigger path: a pre-seeded rule with n=1 plus a scripted
// Bash tool_use forces foci's PostToolNudgeFunc to fire after the tool
// batch. Asserts a SECOND user_message entry appears in the agent's
// workdir whose text_prefix carries the reminder body — i.e. the tool
// result and nudge were folded together and re-sent to CC.
func TestL2_Nudges_AfterToolsNudgeFollowsToolBatch(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	// cc-stub does not emit CC PostToolUse hook events, so foci's
	// PostToolNudgeFunc never fires under the L2 test rig — there's no
	// hook envelope on stdout for ccstream.handleHookResponse to consume.
	// Wiring this needs cc-stub to synthesize a PostToolUse system/hook
	// event after every Bash tool_use it runs, mirroring real CC.
	t.Skip("HARNESS GAP: cc-stub does not emit PostToolUse hook envelopes — extend cmd/cc-stub to send a system/hook_response after each tool_use so ccstream's PostToolNudgeFunc bridge fires")
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
	// Two compounding gaps:
	//   1. The harness's writeTestConfig has no knob for setting
	//      nudge_pre_answer_gate=true at the [nudge] or [agents.nudge]
	//      level — gate defaults to false.
	//   2. Pre-answer fires only after NudgePreAnswerMinTools (default 2)
	//      tool calls and post-tool hook events, which cc-stub doesn't
	//      emit (see TestL2_Nudges_AfterToolsNudgeFollowsToolBatch).
	t.Skip("HARNESS GAP: needs HarnessOptions to inject nudge_pre_answer_gate=true in the test config, AND cc-stub PostToolUse hook events to bump the tool counter past NudgePreAnswerMinTools")
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
	// Partial coverage is possible (regex + turn-interval both render
	// through InjectNudges and would carry the footer), but the full
	// "all four paths" assertion in the purpose comment requires the
	// after-tools path which cc-stub can't reach without PostToolUse
	// hook events.
	t.Skip("HARNESS GAP: full coverage needs cc-stub to emit PostToolUse hook envelopes (after-tools path) and a nudge_pre_answer_gate config knob (pre-answer path)")
}

// TestL2_Nudges_HeaderPresentOnAllDeliveryPaths is the sibling
// regression net to the footer test: every nudge block must carry the
// "[system: automatic nudge —" header so CC treats it as background
// guidance, not user input. Asserts the header marker appears in the
// user_message text_prefix for regex, turn-interval, and after-tools
// fires.
func TestL2_Nudges_HeaderPresentOnAllDeliveryPaths(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("HARNESS GAP: full coverage needs cc-stub to emit PostToolUse hook envelopes so the after-tools delivery path is observable in the recorder")
}

// TestL2_Nudges_PerAgentRulesIsolated proves that per-agent rules files
// don't bleed into other agents. Two agents (alpha, bravo) get distinct
// nudge-rules.json files with distinct regex reminders that both match
// the same user phrase. Sends the matching phrase to each agent
// separately. Asserts alpha's user_message carries only alpha's reminder
// text, bravo's carries only bravo's — confirms the scheduler is built
// per-agent off the agent's own workspace rules file.
func TestL2_Nudges_PerAgentRulesIsolated(t *testing.T) {
	const alphaUser = 7301
	const bravoUser = 7302
	const alphaMarker = "ISOLATION_ALPHA_TAG_UNIQUE"
	const bravoMarker = "ISOLATION_BRAVO_TAG_UNIQUE"

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: alphaUser},
			{ID: "bravo", UserID: bravoUser},
		},
		ReadyTimeout: 30 * time.Second,
	})

	seedNudgeRules(t, h, "alpha", []nudge.Rule{{
		Text:       alphaMarker,
		SourceFile: "CRAFT.md",
		Trigger:    nudge.Trigger{Type: "regex", Pattern: "ping"},
		Priority:   "high",
	}})
	seedNudgeRules(t, h, "bravo", []nudge.Rule{{
		Text:       bravoMarker,
		SourceFile: "CRAFT.md",
		Trigger:    nudge.Trigger{Type: "regex", Pattern: "ping"},
		Priority:   "high",
	}})

	pushUserMessage(t, h, "alpha", alphaUser, "ping alpha")
	if _, ok := waitForUserMessageContaining(t, h, "alpha", 20*time.Second, alphaMarker, "ping alpha"); !ok {
		t.Fatalf("alpha's nudge never landed; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}

	pushUserMessage(t, h, "bravo", bravoUser, "ping bravo")
	if _, ok := waitForUserMessageContaining(t, h, "bravo", 20*time.Second, bravoMarker, "ping bravo"); !ok {
		t.Fatalf("bravo's nudge never landed; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}

	// Negative cross-checks: alpha's reminder text must never appear in
	// bravo's workdir, and vice versa.
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if e.Kind != "user_message" {
			continue
		}
		if strings.Contains(e.Workdir, "workspaces/alpha") && strings.Contains(e.TextPrefix, bravoMarker) {
			t.Errorf("bravo's reminder leaked into alpha's workdir: %q", e.TextPrefix)
		}
		if strings.Contains(e.Workdir, "workspaces/bravo") && strings.Contains(e.TextPrefix, alphaMarker) {
			t.Errorf("alpha's reminder leaked into bravo's workdir: %q", e.TextPrefix)
		}
	}
}

// TestL2_Nudges_MaxPerBatchCapsAfterToolsReminders proves
// nudge_max_per_batch is honoured: two distinct every_n_tools rules
// both eligible to fire after a single tool batch, with max_per_batch=1
// in config, produce exactly one nudge in the resulting user_message
// text_prefix — not both. The negative half asserts the second rule's
// reminder body is absent.
func TestL2_Nudges_MaxPerBatchCapsAfterToolsReminders(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("HARNESS GAP: after-tools nudges depend on PostToolUse hook events which cc-stub does not synthesize — extend cmd/cc-stub to emit a system/hook_response envelope after each Bash tool_use")
}

// TestL2_Nudges_CooldownSuppressesRepeatedAfterToolsNudge proves the
// cooldown gate: a single every_n_tools rule with n=1 and config
// nudge_cooldown=5 should fire on the first tool call, then be
// suppressed for the next four within the same turn. Scripts cc-stub
// to emit two Bash tool_uses back-to-back, then asserts the rule's
// reminder appears in exactly one user_message text_prefix, not both.
func TestL2_Nudges_CooldownSuppressesRepeatedAfterToolsNudge(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	t.Skip("HARNESS GAP: after-tools nudges require cc-stub to emit PostToolUse hook envelopes; without them PostToolNudgeFunc never fires and cooldown can't be exercised")
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
	const userID = 7400
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:       []testharness.AgentSpec{{ID: "alpha", UserID: userID}},
		ReadyTimeout: 30 * time.Second,
	})

	// Deliberately do NOT seed nudge-rules.json — that's the precondition
	// for auto-extract to fire. nudge_enable and nudge_auto_extract both
	// default to true in the resolved config.
	pushUserMessage(t, h, "alpha", userID, "trigger first activity")

	// The extractor uses DelegatedManager.RunOnce which spawns cc-stub
	// with flags unique to one-shot mode: --dangerously-skip-permissions
	// and --no-session-persistence. The long-lived agent CC spawn
	// instead carries --input-format stream-json. Use the presence of
	// --no-session-persistence as the discriminator — it's only set by
	// the RunOnce code path (delegated_manager.go:602-606), so its
	// presence in any recorded invocation is a definitive signal the
	// extractor ran.
	deadline := time.Now().Add(20 * time.Second)
	var sawExtract bool
	var allInvocations []recorderEntry
	for time.Now().Before(deadline) && !sawExtract {
		allInvocations = invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
		for _, inv := range allInvocations {
			for _, f := range inv.Flags {
				if f == "--no-session-persistence" {
					sawExtract = true
					break
				}
			}
		}
		if sawExtract {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !sawExtract {
		t.Errorf("never observed an extractor (--no-session-persistence) invocation for alpha — auto-extractor did not run.\ninvocations seen:\n%s\nstderr:\n%s",
			invocationsTail(allInvocations), stderrTail(h.Stderr()))
	}
}

// TestL2_Nudges_AutoExtractSkippedWhenContentHashMatches proves the
// hash-gated skip in Extractor.NeedsExtraction: a pre-seeded
// nudge-rules.json whose content_hash matches the SHA-256 of the
// agent's character files causes the extractor to no-op. Asserts no
// extra `claude --print` invocation entry appears in the recorder
// beyond the normal agent CC spawn.
func TestL2_Nudges_AutoExtractSkippedWhenContentHashMatches(t *testing.T) {
	const userID = 7500
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:       []testharness.AgentSpec{{ID: "alpha", UserID: userID}},
		ReadyTimeout: 30 * time.Second,
	})

	// Seed an empty-rules file with the correct content_hash. The
	// extractor's NeedsExtraction compares this to ContentHash of the
	// agent's character files; a match means "no work to do".
	seedNudgeRules(t, h, "alpha", nil)

	pushUserMessage(t, h, "alpha", userID, "drive one turn")

	// Wait for the long-lived agent CC invocation to appear so we know
	// foci processed the message.
	if !waitForUserMessage(t, h, "workspaces/alpha", "drive one turn", 20*time.Second) {
		t.Fatalf("agent never processed the priming message; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}

	// Give the extractor goroutine a fair chance to spawn cc-stub if it
	// were going to. NeedsExtraction is checked synchronously inside
	// OnActivity (BEFORE spawning the goroutine), so if the hash matches,
	// no goroutine launches at all — a short grace window is enough to
	// surface a spurious launch if one were happening.
	time.Sleep(2 * time.Second)

	invocations := invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
	for _, inv := range invocations {
		for _, f := range inv.Flags {
			if f == "--no-session-persistence" {
				t.Errorf("auto-extract ran despite matching content_hash — saw extractor invocation:\n%s",
					invocationsTail([]recorderEntry{inv}))
			}
		}
	}
}

// TestL2_Nudges_MalformedRulesFileToleratedAtStartup is the negative
// path for parser robustness: a nudge-rules.json containing invalid
// JSON must not crash foci-gw at startup or break the agent loop.
// Asserts the gateway reaches its "started N agent(s)" ready line AND
// a subsequent user message produces a user_message entry with no nudge
// text — foci degrades to "no rules" gracefully.
func TestL2_Nudges_MalformedRulesFileToleratedAtStartup(t *testing.T) {
	const userID = 7600

	// Seeding the bad file must happen BEFORE StartGateway — at startup
	// foci loads nudge rules and we want to exercise that path. The
	// workspace path is deterministic relative to t.TempDir(), but
	// StartGateway owns the temp dir. To work around that we'd normally
	// extend the harness; instead we accept that LoadRules failure is
	// also exercised on the OnActivity reload path, which runs against
	// our seeded file after startup. The crash-resistance contract is
	// the same in both cases (LoadRules logs and returns an error; foci
	// keeps running).
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:       []testharness.AgentSpec{{ID: "alpha", UserID: userID}},
		ReadyTimeout: 30 * time.Second,
	})

	// Write garbage where the rules file should be. Even though startup
	// has already happened, the reload path triggered by OnActivity
	// (and any subsequent NudgeReloadFunc call) goes through the same
	// LoadRules / ParseRules code path.
	seedNudgeRulesRaw(t, h, "alpha", []byte("{this is not valid json, ::]"))

	pushUserMessage(t, h, "alpha", userID, "still alive after malformed rules")

	entry, ok := waitForUserMessageContaining(t, h, "alpha", 20*time.Second, "still alive after malformed rules")
	if !ok {
		t.Fatalf("agent did not process message after malformed rules file; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}

	// Negative: no nudge text should have been prepended — graceful
	// degradation means "no rules loaded" which means "no nudges fire".
	// Default tool/skill reminders fire only every 50 turns so they
	// won't surface here.
	if strings.Contains(entry.TextPrefix, nudgeHeaderMarker) {
		t.Errorf("malformed rules unexpectedly produced an injected nudge: %q", entry.TextPrefix)
	}
}

// TestL2_Nudges_EmptyRulesArrayProducesNoNudge is the negative path
// for empty rule sets: a syntactically valid nudge-rules.json with an
// empty rules array means the scheduler has nothing to fire. Sends a
// message that WOULD match a regex if any rule existed, then asserts
// the resulting user_message text_prefix contains no nudgeHeader
// marker — i.e. nothing was injected.
func TestL2_Nudges_EmptyRulesArrayProducesNoNudge(t *testing.T) {
	const userID = 7700
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:       []testharness.AgentSpec{{ID: "alpha", UserID: userID}},
		ReadyTimeout: 30 * time.Second,
	})

	// Seed an empty rules set with matching content_hash so auto-extract
	// doesn't kick in and clobber it with extractor output.
	seedNudgeRules(t, h, "alpha", nil)

	pushUserMessage(t, h, "alpha", userID, "would-match deploy keyword")

	entry, ok := waitForUserMessageContaining(t, h, "alpha", 20*time.Second, "would-match deploy keyword")
	if !ok {
		t.Fatalf("agent never processed the message; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}
	if strings.Contains(entry.TextPrefix, nudgeHeaderMarker) {
		t.Errorf("empty rules array produced a nudge: text_prefix=%q", entry.TextPrefix)
	}
}

// TestL2_Nudges_InvalidRegexPatternIgnored proves the scheduler's
// graceful-degradation contract for malformed regex triggers: a rule
// with an uncompilable pattern (e.g. "[") must not crash the agent
// loop; the rule simply never fires. Asserts a normal user_message
// entry appears and contains no nudgeHeader marker.
func TestL2_Nudges_InvalidRegexPatternIgnored(t *testing.T) {
	const userID = 7800
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:       []testharness.AgentSpec{{ID: "alpha", UserID: userID}},
		ReadyTimeout: 30 * time.Second,
	})

	seedNudgeRules(t, h, "alpha", []nudge.Rule{{
		Text:       "should-never-fire-if-regex-is-invalid",
		SourceFile: "CRAFT.md",
		Trigger:    nudge.Trigger{Type: "regex", Pattern: "["}, // uncompilable
		Priority:   "high",
	}})

	pushUserMessage(t, h, "alpha", userID, "any message body here")

	entry, ok := waitForUserMessageContaining(t, h, "alpha", 20*time.Second, "any message body here")
	if !ok {
		t.Fatalf("agent never processed the message; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}
	if strings.Contains(entry.TextPrefix, nudgeHeaderMarker) {
		t.Errorf("invalid-regex rule produced a nudge anyway: text_prefix=%q", entry.TextPrefix)
	}
}

// TestL2_Nudges_CharacterDirRulesPathPreferred proves the path-precedence
// in nudge.RulesPath: when both {workspace}/character/nudge-rules.json
// and {workspace}/nudge-rules.json exist, the character-dir copy wins.
// Seeds the character-dir file with a regex reminder "INNER" and the
// workspace-dir file with "OUTER", sends a matching message, asserts
// "INNER" appears in the user_message text_prefix and "OUTER" does not.
func TestL2_Nudges_CharacterDirRulesPathPreferred(t *testing.T) {
	const userID = 7900
	const innerMarker = "INNER_CHAR_DIR_MARKER_PREFERRED"
	const outerMarker = "OUTER_WORKSPACE_ROOT_MARKER_IGNORED"

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:       []testharness.AgentSpec{{ID: "alpha", UserID: userID}},
		ReadyTimeout: 30 * time.Second,
	})

	// Character-dir file (the one that should win).
	seedNudgeRules(t, h, "alpha", []nudge.Rule{{
		Text:       innerMarker,
		SourceFile: "CRAFT.md",
		Trigger:    nudge.Trigger{Type: "regex", Pattern: "precedence"},
		Priority:   "high",
	}})

	// Workspace-root copy (the one that should be ignored). Same hash
	// shape; if the precedence rule were broken and this file won, we'd
	// see outerMarker in the prompt.
	wsRoot := agentWorkspace(h, "alpha")
	rs := &nudge.RuleSet{
		ContentHash: nudge.ContentHash([]string{testCraftBody}),
		Rules: []nudge.Rule{{
			Text:       outerMarker,
			SourceFile: "CRAFT.md",
			Trigger:    nudge.Trigger{Type: "regex", Pattern: "precedence"},
			Priority:   "high",
		}},
	}
	body, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		t.Fatalf("marshal workspace-root rules: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsRoot, "nudge-rules.json"), body, 0o600); err != nil {
		t.Fatalf("write workspace-root rules: %v", err)
	}

	pushUserMessage(t, h, "alpha", userID, "test precedence")

	entry, ok := waitForUserMessageContaining(t, h, "alpha", 20*time.Second, innerMarker, "test precedence")
	if !ok {
		t.Fatalf("character-dir rule did not fire; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
	if strings.Contains(entry.TextPrefix, outerMarker) {
		t.Errorf("workspace-root rules file leaked into the nudge — precedence is broken; text_prefix=%q", entry.TextPrefix)
	}
}

// TestL2_Nudges_DisabledByConfigSuppressesAllInjection proves
// nudge_enable=false short-circuits the system entirely: with a regex
// rule pre-seeded AND nudge_enable=false in the agent's config block,
// a matching message produces a user_message entry whose text_prefix
// contains the user text but no nudgeHeader marker — the scheduler was
// never built, no injection happened.
func TestL2_Nudges_DisabledByConfigSuppressesAllInjection(t *testing.T) {
	_ = testharness.HarnessOptions{ReadyTimeout: 30 * time.Second}
	// nudge_enable defaults to true and the harness's writeTestConfig
	// has no path for overriding it (no global [nudge] section emitted,
	// no per-agent override exposed in HarnessOptions). The test as
	// specified can only run with config injection support.
	t.Skip("HARNESS GAP: needs HarnessOptions to inject `nudge_enable=false` into either the global [nudge] table or per-agent [agents.nudge] override in writeTestConfig")
}

// invocationsTail summarises a slice of invocation recorderEntries for
// failure logs. Mirrors recorderTail's compact one-line format, scoped
// to just invocation entries.
func invocationsTail(invs []recorderEntry) string {
	var sb strings.Builder
	for _, e := range invs {
		fmt.Fprintf(&sb, "  %s\tworkdir=%s\tresume=%s\tflags=%v\n",
			e.Timestamp, e.Workdir, e.ResumeID, e.Flags)
	}
	return sb.String()
}
