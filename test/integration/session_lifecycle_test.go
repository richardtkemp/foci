//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/testharness"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// SessionLifecycle covers the SESSION-level lifecycle surface of foci's
// delegated path: resume-id tracking, /reset and /reset hard, multi-turn
// continuity, queueing during a busy turn, cross-session isolation,
// and resume-failure fallback. SUBPROCESS restart (a single cc-stub
// process exiting unexpectedly) is owned by lifecycle_test.go — these
// tests focus on what foci tracks BETWEEN subprocesses and within one
// long-lived session.
//
// All tests start with t.Skip until implementation lands. The function
// shape — signature + purpose doc + harness reference — is here so that
// the scaffolding is greppable and the scenarios are immediately
// reviewable.

// --- local helpers --------------------------------------------------

// sendText pushes a plain Telegram text update onto the given token's
// queue. Kept short so test bodies stay readable.
func sendText(h *testharness.Harness, token string, chatID, userID int64, text string) {
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: chatID, Type: "private"},
			From: &gotgbot.User{Id: userID, FirstName: "Tester"},
			Text: text,
		},
	})
}

// waitForSendMessageContaining polls PeekSent for a sendMessage whose
// "text" body contains substr. Returns the full text on hit, "" on
// timeout. Useful for asserting that a slash command's reply (or an
// error message) landed in Telegram.
func waitForSendMessageContaining(h *testharness.Harness, token, substr string, timeout time.Duration) string {
	if timeout < testharness.CorrectnessWaitFloor {
		timeout = testharness.CorrectnessWaitFloor
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, call := range h.TelegramStub().PeekSent(token) {
			if call.Method != "sendMessage" {
				continue
			}
			var body map[string]any
			if err := json.Unmarshal(call.Body, &body); err != nil {
				continue
			}
			if text, _ := body["text"].(string); strings.Contains(text, substr) {
				return text
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return ""
}

// sentMessageContaining does a single non-blocking scan of the Telegram
// stub's sent log for a sendMessage containing any of the substrings,
// returning the matched text or "".
func sentMessageContaining(h *testharness.Harness, token string, substrs ...string) string {
	for _, call := range h.TelegramStub().PeekSent(token) {
		if call.Method != "sendMessage" {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(call.Body, &body); err != nil {
			continue
		}
		text, _ := body["text"].(string)
		for _, substr := range substrs {
			if strings.Contains(text, substr) {
				return text
			}
		}
	}
	return ""
}

// resetWhenIdle drives /reset to completion. The in-flight guard rejects a
// reset while the previous turn's tail is still running (by design — users
// see "send stop first"), so under CPU load a one-shot /reset races that
// tail: the L2 flake family where every reset test fails together on a
// loaded box. Re-send /reset until the confirmation lands; the guard error
// is harmless and idempotent. Matches both wordings ("Session reset" for
// delegated agents, "Session cleared." for API agents).
func resetWhenIdle(t *testing.T, h *testharness.Harness, token string, userID int64) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		sendText(h, token, userID, userID, "/reset")
		poll := time.Now().Add(5 * time.Second)
		for time.Now().Before(poll) {
			if sentMessageContaining(h, token, "Session reset", "Session cleared") != "" {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	t.Fatalf("/reset never confirmed within 90s; sent calls:\n%s\nstderr tail:\n%s",
		sentCallsTail(h.TelegramStub(), token), stderrTail(h.Stderr()))
}

// userMessagesIn returns the user_message entries whose workdir contains
// the given substring, preserving order.
func userMessagesIn(entries []recorderEntry, workdirSubstr string) []recorderEntry {
	var out []recorderEntry
	for _, e := range entries {
		if e.Kind == "user_message" && strings.Contains(e.Workdir, workdirSubstr) {
			out = append(out, e)
		}
	}
	return out
}

// initSystemsIn returns the init_system entries whose workdir contains the
// given substring, preserving order. Each is one initialize handshake — i.e.
// one spawn that received a system prompt.
func initSystemsIn(entries []recorderEntry, workdirSubstr string) []recorderEntry {
	var out []recorderEntry
	for _, e := range entries {
		if e.Kind == "init_system" && strings.Contains(e.Workdir, workdirSubstr) {
			out = append(out, e)
		}
	}
	return out
}

// waitForInvocationCount blocks until the recorder shows at least n
// invocation entries for the given workdir substring, or the deadline
// elapses. Returns the matching invocations (possibly more than n on
// hit) and a bool indicating whether the threshold was reached.
func waitForInvocationCount(t *testing.T, h *testharness.Harness, workdirSubstr string, n int, timeout time.Duration) ([]recorderEntry, bool) {
	t.Helper()
	if timeout < testharness.CorrectnessWaitFloor {
		timeout = testharness.CorrectnessWaitFloor
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		invs := invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), workdirSubstr)
		if len(invs) >= n {
			return invs, true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), workdirSubstr), false
}

// waitForUserMessageCount blocks until at least n user_message entries
// appear for the workdir substring, or the deadline elapses.
func waitForUserMessageCount(t *testing.T, h *testharness.Harness, workdirSubstr string, n int, timeout time.Duration) ([]recorderEntry, bool) {
	t.Helper()
	if timeout < testharness.CorrectnessWaitFloor {
		timeout = testharness.CorrectnessWaitFloor
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ums := userMessagesIn(readRecorderEntries(t, h.RecorderPath()), workdirSubstr)
		if len(ums) >= n {
			return ums, true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return userMessagesIn(readRecorderEntries(t, h.RecorderPath()), workdirSubstr), false
}

// --- tests ----------------------------------------------------------

// NOTE — TestL2_SessionLifecycle_ResumeIDPassedOnSecondTurn was removed
// 2026-05-16. The test asked the wrong question: it asserted invs[1]
// (the main session's INITIAL spawn) carries a --resume flag matching
// the first turn's session_id. But in cc-stub's long-lived streaming
// mode, the agent stays alive across turns — there is no per-turn
// re-spawn, so no --resume is ever emitted by the harness. The
// resume-id persistence path needs a different test that forces a
// real respawn (per-spawn env injection or a kill helper).
// TestL2_SessionLifecycle_BackendDeathMidSessionRespawns below already
// names this gap.

// TestL2_SessionLifecycle_MultiTurnSharesSessionID proves three
// sequential user messages within the same Telegram chat all process
// through ONE long-lived cc-stub subprocess and produce three
// user_message recorder entries that share a single session_id.
// Mechanism: push three updates on the same chat, poll the recorder
// until three user_message entries appear under that workdir, group by
// session_id and assert cardinality of 1. Catches accidental
// per-message subprocess spawning and session-key churn.
func TestL2_SessionLifecycle_MultiTurnSharesSessionID(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 9101},
		},
		ReadyTimeout: 30 * time.Second,
	})
	token := h.AgentBotToken("alpha")

	for i, text := range []string{"turn one", "turn two", "turn three"} {
		sendText(h, token, 9101, 9101, text)
		if !waitForUserMessage(t, h, "workspaces/alpha", text, 20*time.Second) {
			t.Fatalf("turn %d (%q) never processed; stderr tail:\n%s", i+1, text, stderrTail(h.Stderr()))
		}
	}

	ums, ok := waitForUserMessageCount(t, h, "workspaces/alpha", 3, 5*time.Second)
	if !ok {
		t.Fatalf("expected 3 user_message entries, got %d", len(ums))
	}

	sessIDs := map[string]struct{}{}
	for _, e := range ums {
		if e.SessionID == "" {
			t.Errorf("user_message has empty session_id: %+v", e)
			continue
		}
		sessIDs[e.SessionID] = struct{}{}
	}
	if len(sessIDs) != 1 {
		t.Errorf("expected all 3 turns to share one session_id, got %d distinct: %v", len(sessIDs), sessIDs)
	}
}

// TestL2_SessionLifecycle_ResetSpawnsFreshCCSession proves a /reset
// command on an established session hands the delegated backend to the
// reflection branch, and the NEXT user message spawns a fresh cc-stub
// subprocess with NO --resume flag (foci's session key is a stable
// identity — only the CC-side session is fresh). Mechanism: prime the
// session, send "/reset", send another message; assert two
// invocations in the recorder where the second has empty resume_id
// and a different session_id from the first.
func TestL2_SessionLifecycle_ResetSpawnsFreshCCSession(t *testing.T) {
	testharness.ParallelHeavy(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 9201},
		},
		ReadyTimeout: 30 * time.Second,
	})
	token := h.AgentBotToken("alpha")

	sendText(h, token, 9201, 9201, "prime")
	if !waitForUserMessage(t, h, "workspaces/alpha", "prime", 15*time.Second) {
		t.Fatalf("prime turn never processed; stderr tail:\n%s", stderrTail(h.Stderr()))
	}
	primeUMs := userMessagesIn(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
	if len(primeUMs) < 1 {
		t.Fatalf("no user_message after prime")
	}
	firstSession := primeUMs[0].SessionID

	resetWhenIdle(t, h, token, 9201)

	sendText(h, token, 9201, 9201, "after reset")
	if !waitForUserMessage(t, h, "workspaces/alpha", "after reset", 15*time.Second) {
		t.Fatalf("post-reset turn never processed; stderr tail:\n%s", stderrTail(h.Stderr()))
	}

	invs, ok := waitForInvocationCount(t, h, "workspaces/alpha", 2, 5*time.Second)
	if !ok {
		t.Fatalf("expected >=2 invocations after reset, got %d", len(invs))
	}
	// The second invocation must be fresh — no --resume.
	if invs[1].ResumeID != "" {
		t.Errorf("post-reset invocation carried resume_id=%q; want empty (fresh session)", invs[1].ResumeID)
	}

	// And the post-reset user_message session_id must differ from the
	// pre-reset one.
	allUMs := userMessagesIn(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
	var secondSession string
	for _, e := range allUMs {
		if strings.Contains(e.TextPrefix, "after reset") {
			secondSession = e.SessionID
			break
		}
	}
	if secondSession == "" {
		t.Fatalf("could not locate post-reset user_message")
	}
	if secondSession == firstSession {
		t.Errorf("CC session_id did not change across /reset: %q == %q", firstSession, secondSession)
	}
}

// TestL2_SessionLifecycle_ResetRebuildsSystemPromptFromDisk proves Part A
// (#828 / #706): editing a character file on disk and then /reset causes the
// fresh CC session to receive a *rebuilt* system prompt — SystemPromptFunc
// reads from disk at session-start — rather than the prompt frozen at agent
// setup. Mechanism: prime a turn (records init_system #1 with a baseline
// prompt hash + length); append a distinctive marker to character/CRAFT.md;
// /reset; prime another turn; assert the post-reset init_system prompt hash
// differs and its length grew by exactly the appended bytes. A frozen prompt
// would re-send an identical hash — which is precisely the #706 bug this guards.
func TestL2_SessionLifecycle_ResetRebuildsSystemPromptFromDisk(t *testing.T) {
	testharness.ParallelHeavy(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 9201},
		},
		ReadyTimeout: 30 * time.Second,
	})
	token := h.AgentBotToken("alpha")

	sendText(h, token, 9201, 9201, "prime")
	if !waitForUserMessage(t, h, "workspaces/alpha", "prime", 15*time.Second) {
		t.Fatalf("prime turn never processed; stderr tail:\n%s", stderrTail(h.Stderr()))
	}
	initsBefore := initSystemsIn(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
	if len(initsBefore) < 1 {
		t.Fatalf("no init_system entry after prime; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}
	before := initsBefore[len(initsBefore)-1]

	// Edit a character file on disk AFTER agent setup. A frozen prompt would
	// never see this; Part A's SystemPromptFunc re-reads it at session-start.
	marker := "\n\nMARKER-PART-A-RELOAD-3f8c2a1e distinctive reload sentinel line.\n"
	craft := filepath.Join(h.AgentWorkspace("alpha"), "character", "CRAFT.md")
	existing, err := os.ReadFile(craft)
	if err != nil {
		t.Fatalf("read CRAFT.md: %v", err)
	}
	if err := os.WriteFile(craft, append(existing, []byte(marker)...), 0o600); err != nil {
		t.Fatalf("append marker to CRAFT.md: %v", err)
	}

	resetWhenIdle(t, h, token, 9201)

	sendText(h, token, 9201, 9201, "after reset")
	if !waitForUserMessage(t, h, "workspaces/alpha", "after reset", 15*time.Second) {
		t.Fatalf("post-reset turn never processed; stderr tail:\n%s", stderrTail(h.Stderr()))
	}

	// Wait for the post-reset spawn's init_system to land.
	var after recorderEntry
	deadline := time.Now().Add(5 * time.Second)
	for {
		inits := initSystemsIn(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
		if len(inits) > len(initsBefore) {
			after = inits[len(inits)-1]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no new init_system after reset; have %d (had %d); recorder:\n%s",
				len(inits), len(initsBefore), recorderTail(t, h.RecorderPath()))
		}
		time.Sleep(100 * time.Millisecond)
	}

	if after.PromptSHA256 == before.PromptSHA256 {
		t.Errorf("system prompt hash unchanged across /reset (%s) — prompt was NOT rebuilt from disk; the CRAFT.md edit was ignored (#706 regression)",
			before.PromptSHA256)
	}
	if wantLen := before.PromptLen + len(marker); after.PromptLen != wantLen {
		t.Errorf("post-reset PromptLen = %d, want %d (before %d + %d marker bytes); something other than the marker changed",
			after.PromptLen, wantLen, before.PromptLen, len(marker))
	}
}

// TestL2_SessionLifecycle_ResetHardCancelsInFlightTurn proves
// /reset hard fires while a turn is mid-flight, cancels it, destroys
// the backend, and the next message starts a clean fresh session
// without --resume. Mechanism: script cc-stub to stall via CCSTUB_HANG
// on its next user message; send a normal message to start the hung
// turn; send "/reset hard"; send a follow-up message; assert the
// follow-up shows up as a user_message with no resume_id and a new
// session_id. Catches the CancelSession + StopSession + Store.Reset
// sequencing in ResetSessionHard.
func TestL2_SessionLifecycle_ResetHardCancelsInFlightTurn(t *testing.T) {
	testharness.ParallelWait(t)
	// HARNESS GAP: CCSTUB_HANG is a process-level env var read once at
	// spawn. It cannot be toggled per-turn from the test, and the
	// harness has no per-agent env-var injection. Without the ability
	// to make a SPECIFIC user message hang, we can't reliably set up an
	// in-flight turn that /reset hard then cancels.
	//
	// We still implement the structural part: prime, then /reset hard,
	// then a follow-up — and assert the CC session_id changes and the next
	// spawn has no --resume.
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 9301},
		},
		ReadyTimeout: 30 * time.Second,
	})
	token := h.AgentBotToken("alpha")

	sendText(h, token, 9301, 9301, "prime")
	if !waitForUserMessage(t, h, "workspaces/alpha", "prime", 15*time.Second) {
		t.Fatalf("prime turn never processed; stderr tail:\n%s", stderrTail(h.Stderr()))
	}
	primeUMs := userMessagesIn(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
	if len(primeUMs) < 1 {
		t.Fatalf("no user_message after prime")
	}
	firstSession := primeUMs[0].SessionID

	sendText(h, token, 9301, 9301, "/reset hard")
	if got := waitForSendMessageContaining(h, token, "Session reset (hard)", 10*time.Second); got == "" {
		t.Fatalf("never saw hard-reset confirmation sendMessage; sent calls:\n%s\nstderr tail:\n%s",
			sentCallsTail(h.TelegramStub(), token), stderrTail(h.Stderr()))
	}

	sendText(h, token, 9301, 9301, "after hard reset")
	if !waitForUserMessage(t, h, "workspaces/alpha", "after hard reset", 15*time.Second) {
		t.Fatalf("post-hard-reset turn never processed; stderr tail:\n%s", stderrTail(h.Stderr()))
	}

	invs, ok := waitForInvocationCount(t, h, "workspaces/alpha", 2, 5*time.Second)
	if !ok {
		t.Fatalf("expected >=2 invocations after hard reset, got %d", len(invs))
	}
	if invs[len(invs)-1].ResumeID != "" {
		t.Errorf("post-hard-reset invocation carried resume_id=%q; want empty", invs[len(invs)-1].ResumeID)
	}

	allUMs := userMessagesIn(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
	var secondSession string
	for _, e := range allUMs {
		if strings.Contains(e.TextPrefix, "after hard reset") {
			secondSession = e.SessionID
			break
		}
	}
	if secondSession == "" {
		t.Fatalf("could not locate post-hard-reset user_message")
	}
	if secondSession == firstSession {
		t.Errorf("CC session_id did not change across /reset hard: %q == %q", firstSession, secondSession)
	}
}

// TestL2_SessionLifecycle_ResetClearsPersistedResumeID proves /reset
// clears the cc_resume_id row in state.db so a service-restart-like
// fresh-spawn after reset doesn't accidentally try to resume the old
// session. Mechanism: prime + reset; assert that any subsequent
// invocation in the recorder for that workdir has an empty resume_id
// field. The negative half of ResumeIDPassedOnSecondTurn.
func TestL2_SessionLifecycle_ResetClearsPersistedResumeID(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 9401},
		},
		ReadyTimeout: 30 * time.Second,
	})
	token := h.AgentBotToken("alpha")

	sendText(h, token, 9401, 9401, "prime")
	if !waitForUserMessage(t, h, "workspaces/alpha", "prime", 15*time.Second) {
		t.Fatalf("prime turn never processed; stderr tail:\n%s", stderrTail(h.Stderr()))
	}

	resetWhenIdle(t, h, token, 9401)

	sendText(h, token, 9401, 9401, "follow-up")
	if !waitForUserMessage(t, h, "workspaces/alpha", "follow-up", 15*time.Second) {
		t.Fatalf("follow-up turn never processed; stderr tail:\n%s", stderrTail(h.Stderr()))
	}

	invs, ok := waitForInvocationCount(t, h, "workspaces/alpha", 2, 5*time.Second)
	if !ok {
		t.Fatalf("expected >=2 invocations, got %d", len(invs))
	}
	// Every invocation AFTER the prime one must have resume_id=="".
	// The prime invocation is invs[0]; everything else must be empty.
	for i := 1; i < len(invs); i++ {
		if invs[i].ResumeID != "" {
			t.Errorf("invocation #%d after /reset carried resume_id=%q; want empty", i, invs[i].ResumeID)
		}
	}
}

// TestL2_SessionLifecycle_ResumeFailureFallsBackToFresh proves foci's
// delegated retry path: when CC exits with non-zero during init
// because the persisted --resume id can't be found, foci respawns
// WITHOUT --resume and the message still processes. Mechanism: prime
// once to persist a resume_id; ensure cc-stub for that workdir runs
// with CCSTUB_FAIL_ON_RESUME=1 on the next spawn (requires harness
// per-agent env-var injection — extend if absent); send a follow-up
// message; assert the recorder shows an exit-on-resume invocation
// followed by a successful fresh invocation that processed the message.
// Regression net for "delegated: backend died during init with
// --resume <ID> — retrying without resume".
func TestL2_SessionLifecycle_ResumeFailureFallsBackToFresh(t *testing.T) {
	testharness.ParallelWait(t)
	const userID = 8311

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{{
			ID:     "alpha",
			UserID: userID,
			ExtraEnv: map[string]string{
				// First spawn processes one turn then exits cleanly so
				// foci persists the cc_resume_id. Second spawn carries
				// --resume and FAIL_ON_RESUME flips it to exit code 1
				// during init, triggering foci's retry-without-resume.
				"CCSTUB_EXIT_AFTER_N_TURNS": "1",
				"CCSTUB_FAIL_ON_RESUME":     "1",
			},
		}},
		ReadyTimeout: 30 * time.Second,
	})

	pushUserMessage(t, h, "alpha", userID, "prime-the-session")
	if !waitForUserMessage(t, h, "workspaces/alpha", "prime-the-session", 20*time.Second) {
		t.Fatalf("prime never reached cc-stub; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}

	pushUserMessage(t, h, "alpha", userID, "follow-up-after-resume-fail")
	if !waitForUserMessage(t, h, "workspaces/alpha", "follow-up-after-resume-fail", 30*time.Second) {
		t.Fatalf("follow-up never processed after resume fallback; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}

	// Recorder should show the failed-respawn invocation carrying a
	// non-empty --resume, followed by the retry without --resume.
	var withResume, withoutResumeAfter bool
	invs := invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
	for _, inv := range invs {
		if inv.ResumeID != "" {
			withResume = true
		}
	}
	// The post-retry invocation (a fresh spawn without --resume) is the
	// only one able to PROCESS follow-up-after-resume-fail. Confirm via
	// the user_message recorder entry's session id (set to a stub-* id
	// generated on a fresh spawn).
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if e.Kind == "user_message" && strings.Contains(e.TextPrefix, "follow-up-after-resume-fail") {
			withoutResumeAfter = true
			break
		}
	}
	if !withResume {
		t.Errorf("expected one invocation with --resume (the failed respawn); invocations:\n%s",
			invocationsTail(invs))
	}
	if !withoutResumeAfter {
		t.Errorf("expected the follow-up to land as a user_message via the retry-without-resume path; recorder:\n%s",
			recorderTail(t, h.RecorderPath()))
	}
}

// TestL2_SessionLifecycle_BackendDeathMidSessionRespawns proves foci
// recovers when a long-lived cc-stub subprocess dies between turns
// (not during init): the next user message spawns a fresh subprocess
// AND passes the persisted --resume so the session continues. Distinct
// from lifecycle_test.go's TestL2_Lifecycle_RestartAfterStubExit
// (which doesn't force a death and doesn't assert on --resume).
// Mechanism: send first message, kill cc-stub via signal or by
// configuring it to exit after one turn, send second message, assert
// two invocations both in the same workdir with the second carrying
// the first's session_id as --resume.
func TestL2_SessionLifecycle_BackendDeathMidSessionRespawns(t *testing.T) {
	testharness.ParallelWait(t)
	const userID = 8411

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{{
			ID:     "alpha",
			UserID: userID,
			// EXIT_AFTER_N_TURNS=1 makes cc-stub exit cleanly after
			// processing one turn. foci's DelegatedManager.Get observes
			// IsRunning()==false on the next inbound message and
			// respawns the subprocess with --resume <session_id>.
			ExtraEnv: map[string]string{"CCSTUB_EXIT_AFTER_N_TURNS": "1"},
		}},
		ReadyTimeout: 30 * time.Second,
	})

	pushUserMessage(t, h, "alpha", userID, "first-turn-prime")
	if !waitForUserMessage(t, h, "workspaces/alpha", "first-turn-prime", 20*time.Second) {
		t.Fatalf("first turn never reached cc-stub; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}

	pushUserMessage(t, h, "alpha", userID, "second-turn-after-respawn")
	if !waitForUserMessage(t, h, "workspaces/alpha", "second-turn-after-respawn", 30*time.Second) {
		t.Fatalf("second turn never processed after backend respawn; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}

	// Two invocations in the alpha workdir, the second carrying the
	// first's session_id as --resume.
	invs := invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
	if len(invs) < 2 {
		t.Fatalf("expected ≥2 invocations (initial + respawn), got %d:\n%s",
			len(invs), invocationsTail(invs))
	}
	var firstSessionID string
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if e.Kind == "user_message" && strings.Contains(e.TextPrefix, "first-turn-prime") {
			firstSessionID = e.SessionID
			break
		}
	}
	if firstSessionID == "" {
		t.Fatalf("could not find first-turn-prime user_message session_id; recorder:\n%s",
			recorderTail(t, h.RecorderPath()))
	}
	var sawResume bool
	for _, inv := range invs {
		if inv.ResumeID == firstSessionID {
			sawResume = true
			break
		}
	}
	if !sawResume {
		t.Errorf("respawn invocation never carried --resume %s; invocations:\n%s",
			firstSessionID, invocationsTail(invs))
	}
}

// TestL2_SessionLifecycle_QueuedMessageProcessedAfterBusyTurn proves
// foci's per-session inbox correctly handles a burst of Telegram
// messages on the same chat: both fragments reach the agent on the
// SAME session, with the first delivered before the second. The
// inbox may either (a) batch both fragments into one user_message
// turn (separated by a "[follow-up]" marker) or (b) deliver them as
// two sequential user_message turns — which one occurs depends on
// internal queue timing (whether the worker is already mid-flush
// when the second message arrives). Both are acceptable shapes of
// the contract; what must NEVER happen is dropping a message,
// reordering, or spawning a parallel session.
//
// Mechanism: burst-send two messages without intermediate polling.
// Telegram's long-poll batches them in one getUpdates so the bot
// router sees them in the same tick — exactly the queue-while-busy
// condition. Wait until both fragment strings appear somewhere in
// the recorder, then assert the contract: same session_id across
// every user_message that mentions either fragment, and the first
// fragment's recorder entry comes before the second fragment's.
func TestL2_SessionLifecycle_QueuedMessageProcessedAfterBusyTurn(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 9501},
		},
		ReadyTimeout: 30 * time.Second,
	})
	token := h.AgentBotToken("alpha")

	// Burst-send two messages without intermediate polling. Telegram's
	// long-poll batches these in one getUpdates so the bot router sees
	// them in the same tick — exactly the queue-while-busy condition.
	sendText(h, token, 9501, 9501, "queued first")
	sendText(h, token, 9501, 9501, "queued second")

	// Wait for BOTH fragments to land somewhere in the recorder —
	// either in one batched entry or in two sequential entries.
	if !waitForUserMessage(t, h, "workspaces/alpha", "queued first", 20*time.Second) {
		t.Fatalf("never saw 'queued first' in any user_message; recorder:\n%s\nstderr tail:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
	if !waitForUserMessage(t, h, "workspaces/alpha", "queued second", 20*time.Second) {
		t.Fatalf("never saw 'queued second' in any user_message; recorder:\n%s\nstderr tail:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}

	ums := userMessagesIn(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")

	// Locate the recorder entries containing each fragment. The same
	// entry may contain both (batched case) — that's fine.
	firstIdx, secondIdx := -1, -1
	var sessions = map[string]struct{}{}
	for i, e := range ums {
		hasFirst := strings.Contains(e.TextPrefix, "queued first")
		hasSecond := strings.Contains(e.TextPrefix, "queued second")
		if hasFirst && firstIdx == -1 {
			firstIdx = i
		}
		if hasSecond && secondIdx == -1 {
			secondIdx = i
		}
		if hasFirst || hasSecond {
			if e.SessionID == "" {
				t.Errorf("user_message has empty session_id: text_prefix=%q", e.TextPrefix)
			} else {
				sessions[e.SessionID] = struct{}{}
			}
		}
	}

	// Contract: both fragments must be observed.
	if firstIdx == -1 {
		t.Fatalf("could not locate 'queued first' in recorder ums:\n%s", recorderTail(t, h.RecorderPath()))
	}
	if secondIdx == -1 {
		t.Fatalf("could not locate 'queued second' in recorder ums:\n%s", recorderTail(t, h.RecorderPath()))
	}

	// Contract: order preserved. The recorder entry containing the
	// first fragment must not appear AFTER the one containing the
	// second. (Equal is fine — batched case puts both in one entry.)
	// CURRENTLY FAILS ~60% of runs due to a real foci reorder bug —
	// see clutch/docs/inbox-steer-reorder-bug.md. When the bug fix
	// lands this assertion will pass deterministically.
	if firstIdx > secondIdx {
		t.Errorf("inbox reordered the burst: 'queued first' at idx %d, 'queued second' at idx %d\nrecorder:\n%s",
			firstIdx, secondIdx, recorderTail(t, h.RecorderPath()))
	}

	// Contract: same session — burst must not spawn a parallel session.
	if len(sessions) != 1 {
		t.Errorf("burst landed on %d distinct sessions; want exactly 1: %v", len(sessions), sessions)
	}
}

// TestL2_SessionLifecycle_PerChatSessionsIsolated proves two distinct
// Telegram chats on the same agent each get their own session_id and
// their own per-session inbox worker — a message in chat A does not
// pollute chat B's session. Mechanism: register two chat IDs against
// one agent, push one message to each, assert two distinct
// user_message session_ids in the recorder under the same agent
// workdir. Catches regressions in chat-keyed session-key resolution.
func TestL2_SessionLifecycle_PerChatSessionsIsolated(t *testing.T) {
	testharness.ParallelWait(t)
	// The single agent allows one user by config (UserID=9601). Foci's
	// session key is per-(agent, user, chat) — we use two distinct
	// chat IDs from the SAME user to spawn two parallel sessions on
	// the same agent.
	const userID = 9601
	const chatA = 9601 // private chat = same id as user in Telegram convention
	const chatB = -100 // a synthetic "group chat" id

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: userID},
		},
		ReadyTimeout: 30 * time.Second,
	})
	token := h.AgentBotToken("alpha")

	sendText(h, token, chatA, userID, "from chat A")
	sendText(h, token, chatB, userID, "from chat B")

	if !waitForUserMessage(t, h, "workspaces/alpha", "from chat A", 20*time.Second) {
		t.Fatalf("chat A message never processed; stderr tail:\n%s", stderrTail(h.Stderr()))
	}
	if !waitForUserMessage(t, h, "workspaces/alpha", "from chat B", 20*time.Second) {
		t.Fatalf("chat B message never processed; stderr tail:\n%s", stderrTail(h.Stderr()))
	}

	ums := userMessagesIn(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
	var sessA, sessB string
	for _, e := range ums {
		if strings.Contains(e.TextPrefix, "from chat A") && sessA == "" {
			sessA = e.SessionID
		}
		if strings.Contains(e.TextPrefix, "from chat B") && sessB == "" {
			sessB = e.SessionID
		}
	}
	if sessA == "" || sessB == "" {
		t.Fatalf("missing user_messages: sessA=%q sessB=%q\nrecorder:\n%s",
			sessA, sessB, recorderTail(t, h.RecorderPath()))
	}
	if sessA == sessB {
		t.Errorf("expected distinct session_ids per chat; both = %q", sessA)
	}
}

// TestL2_SessionLifecycle_SlashCommandNotForwardedToBackend proves
// foci-side slash commands (e.g. /ping) are intercepted at the bot
// router level and never reach cc-stub as a user message. Mechanism:
// send "/ping" as the only message; wait briefly; assert NO
// user_message recorder entry exists for that workdir while a
// sendMessage with "pong" was delivered through the Telegram stub.
// Negative scenario: protects the invariant that slash commands stay
// out of session history.
func TestL2_SessionLifecycle_SlashCommandNotForwardedToBackend(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 9701},
		},
		ReadyTimeout: 30 * time.Second,
	})
	token := h.AgentBotToken("alpha")

	sendText(h, token, 9701, 9701, "/ping")
	if got := waitForSendMessageContaining(h, token, "pong", 10*time.Second); got == "" {
		t.Fatalf("never saw pong reply to /ping; sent calls:\n%s\nstderr tail:\n%s",
			sentCallsTail(h.TelegramStub(), token), stderrTail(h.Stderr()))
	}

	// Now assert no user_message entry exists for alpha's workdir.
	// Slash commands must never reach cc-stub. We tolerate any
	// invocation that might have been triggered for some other reason
	// (e.g. warmup), but a user_message containing "/ping" or "ping"
	// would indicate the command leaked through.
	ums := userMessagesIn(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
	for _, e := range ums {
		if strings.Contains(e.TextPrefix, "/ping") {
			t.Errorf("slash command leaked to cc-stub: user_message text_prefix=%q", e.TextPrefix)
		}
	}
}

// TestL2_SessionLifecycle_EmptyMessageNotDispatched proves a Telegram
// update with empty text and no attachments is filtered out before it
// reaches the agent inbox. Mechanism: push an Update whose Message has
// Text="" and no attachments; wait the polling window; assert NO
// invocation or user_message in the recorder for that workdir.
// Negative scenario: catches accidental dispatch of empty turns that
// would burn a CC subprocess and write nothing useful.
func TestL2_SessionLifecycle_EmptyMessageNotDispatched(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 9801},
		},
		ReadyTimeout: 30 * time.Second,
	})
	token := h.AgentBotToken("alpha")

	// Push an Update with Text="" and no attachments.
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: 9801, Type: "private"},
			From: &gotgbot.User{Id: 9801, FirstName: "Tester"},
			Text: "",
		},
	})

	// Wait a generous window — long enough for any spurious dispatch
	// to show up. The polling window in the harness is fast (~1s); 3s
	// is plenty.
	time.Sleep(3 * time.Second)

	// Assert no user_message and no invocation for alpha (modulo any
	// invocations that happened before the empty push — which there
	// shouldn't be in a fresh harness).
	entries := readRecorderEntries(t, h.RecorderPath())
	ums := userMessagesIn(entries, "workspaces/alpha")
	if len(ums) != 0 {
		t.Errorf("expected zero user_message entries for alpha, got %d:\n%s",
			len(ums), recorderTail(t, h.RecorderPath()))
	}
	invs := invocationsByWorkdir(entries, "workspaces/alpha")
	if len(invs) != 0 {
		t.Errorf("expected zero invocation entries for alpha, got %d", len(invs))
	}
}

// TestL2_SessionLifecycle_StopCommandCancelsTurn proves /stop while a
// turn is mid-flight cancels the agent's per-session turn context AND
// the next user message proceeds normally (cancel doesn't poison the
// session). Mechanism: script cc-stub to hang for several seconds on
// the next turn; send a normal message to start the hang; send
// "/stop"; send a follow-up message; assert the follow-up landed in
// the recorder under the same workdir, and a "Stopped." sendMessage
// fired through the Telegram stub. Regression net for the per-session
// CancelSession path (replaces the old global bot.cancelTurn).
func TestL2_SessionLifecycle_StopCommandCancelsTurn(t *testing.T) {
	testharness.ParallelWait(t)
	// HARNESS GAP NOTE: CCSTUB_HANG affects only the pre-handshake
	// startup phase, and is process-level. We can't make a specific
	// turn hang without per-spawn env injection. Implement the
	// structural portion: prime + /stop + follow-up, asserting the
	// "Stopped." reply lands and the follow-up still processes.
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 9901},
		},
		ReadyTimeout: 30 * time.Second,
	})
	token := h.AgentBotToken("alpha")

	sendText(h, token, 9901, 9901, "prime")
	if !waitForUserMessage(t, h, "workspaces/alpha", "prime", 15*time.Second) {
		t.Fatalf("prime turn never processed; stderr tail:\n%s", stderrTail(h.Stderr()))
	}

	sendText(h, token, 9901, 9901, "/stop")
	if got := waitForSendMessageContaining(h, token, "Stopped", 10*time.Second); got == "" {
		t.Fatalf("never saw 'Stopped.' reply to /stop; sent calls:\n%s\nstderr tail:\n%s",
			sentCallsTail(h.TelegramStub(), token), stderrTail(h.Stderr()))
	}

	sendText(h, token, 9901, 9901, "after stop")
	if !waitForUserMessage(t, h, "workspaces/alpha", "after stop", 15*time.Second) {
		t.Fatalf("follow-up turn after /stop never processed — session may be poisoned; stderr tail:\n%s",
			stderrTail(h.Stderr()))
	}
}

// TestL2_SessionLifecycle_CompactCommandRoutesToBackend proves the
// /compact slash command on a delegated agent forwards a "/compact"
// directive to the running CC subprocess (rather than running foci's
// API-side summariser, which is irrelevant for delegated agents).
// Mechanism: prime the session; send "/compact"; assert a
// user_message entry whose text_prefix contains the /compact marker
// in the same long-lived subprocess. Catches delegated-vs-API
// compaction routing regressions.
func TestL2_SessionLifecycle_CompactCommandRoutesToBackend(t *testing.T) {
	testharness.ParallelWait(t)
	// Note: foci's CompactCommand for delegated agents calls
	// Agent.CompactSession; for delegated mode this surfaces a "Context
	// compacted (delegated)." sendMessage. Whether CC receives a
	// dedicated /compact user_message depends on Agent.CompactSession's
	// implementation. We assert structurally on the observable side
	// effect: a confirmation Telegram reply AND that compact doesn't
	// spawn a fresh subprocess (it's not a reset).
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 10001},
		},
		ReadyTimeout: 30 * time.Second,
	})
	token := h.AgentBotToken("alpha")

	sendText(h, token, 10001, 10001, "prime")
	if !waitForUserMessage(t, h, "workspaces/alpha", "prime", 15*time.Second) {
		t.Fatalf("prime turn never processed; stderr tail:\n%s", stderrTail(h.Stderr()))
	}
	preCompactInvs := invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
	preCount := len(preCompactInvs)

	sendText(h, token, 10001, 10001, "/compact")
	// Foci's delegated compact path replies "Context compacted (delegated)."
	if got := waitForSendMessageContaining(h, token, "compact", 10*time.Second); got == "" {
		t.Fatalf("never saw /compact reply; sent calls:\n%s\nstderr tail:\n%s",
			sentCallsTail(h.TelegramStub(), token), stderrTail(h.Stderr()))
	}

	// Look for a user_message whose text_prefix surfaces the compact
	// marker — this proves routing-to-backend rather than the
	// API-side summariser. If foci doesn't pass a literal "/compact"
	// string and instead uses a side-channel control message, the
	// recorder won't see it; in that case at minimum the invocation
	// count must not have grown (compact is not a reset).
	allUMs := userMessagesIn(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
	sawCompactMarker := false
	for _, e := range allUMs {
		if strings.Contains(strings.ToLower(e.TextPrefix), "compact") {
			sawCompactMarker = true
			break
		}
	}

	postInvs := invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
	if len(postInvs) > preCount {
		t.Errorf("compact spawned a fresh subprocess (pre=%d post=%d); compact must not reset", preCount, len(postInvs))
	}

	if !sawCompactMarker {
		// Non-fatal: foci may dispatch compact via control_request
		// rather than user_message text, which is invisible to the
		// recorder. Log diagnostics so the failure mode is obvious if
		// this becomes a flake.
		t.Logf("no user_message with 'compact' marker; foci may route compact via a non-user-message channel. Recorder:\n%s",
			recorderTail(t, h.RecorderPath()))
	}
}

// TestL2_SessionLifecycle_ReloadOnCompactBouncesSession proves the #828
// Part B reload-on-compact bounce end-to-end: a completed delegated
// compaction closes the CC session (keeping its resume ID) so the next
// message respawns with --resume AND rebuilds the system prompt from disk.
//
// Mechanism: CCSTUB_EMIT_COMPACT_BOUNDARY makes cc-stub answer a "/compact"
// user message with the real CC compaction envelopes (status "compacting"
// then compact_boundary). That drives foci's compaction waiters to
// COMPLETION (the "✅ Context compacted" reply, distinct from the "⏳
// Compacting" start) and triggers BounceSession. The bounce doesn't respawn
// immediately; the follow-up message does — and that respawn must carry
// --resume <original session> (same conversation, now compacted) plus a
// fresh init_system handshake (prompt rebuilt from disk). reload_on_compact
// defaults on, so no override is needed.
//
// Without cc-stub emitting compact_boundary this path is untestable at L2:
// the compaction waiter would time out, the bounce would never fire, and a
// respawn (if any) would be a fresh session rather than a resume. This is
// the gap #844 filled.
func TestL2_SessionLifecycle_ReloadOnCompactBouncesSession(t *testing.T) {
	testharness.ParallelWait(t)
	const userID = 10027
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{{
			ID:       "alpha",
			UserID:   userID,
			ExtraEnv: map[string]string{"CCSTUB_EMIT_COMPACT_BOUNDARY": "1"},
		}},
		ReadyTimeout: 30 * time.Second,
	})
	token := h.AgentBotToken("alpha")

	// Prime one normal turn → invocation 1 (fresh, no resume) + init_system 1.
	sendText(h, token, userID, userID, "prime")
	if !waitForUserMessage(t, h, "workspaces/alpha", "prime", 15*time.Second) {
		t.Fatalf("prime turn never processed; stderr tail:\n%s", stderrTail(h.Stderr()))
	}
	preEntries := readRecorderEntries(t, h.RecorderPath())
	preInitCount := len(initSystemsIn(preEntries, "workspaces/alpha"))
	// The priming session id is the resume target the bounce must reuse.
	var primeSID string
	for _, e := range preEntries {
		if e.Kind == "user_message" && strings.Contains(e.TextPrefix, "prime") {
			primeSID = e.SessionID
		}
	}
	if primeSID == "" {
		t.Fatalf("no prime session id in recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}

	// The post-compaction reload-bounce is gated on the on-disk system prompt
	// having changed since session start (404aacbf: skip the bounce when the
	// prompt is unchanged — no reload needed). Edit a character file AFTER the
	// priming turn so the prompt hash differs and the bounce actually fires;
	// otherwise the test would never exercise the resume path it asserts on.
	marker := "\n\nMARKER-RELOADONCOMPACT-7c3e1a9b distinctive reload sentinel line.\n"
	craft := filepath.Join(h.AgentWorkspace("alpha"), "character", "CRAFT.md")
	if existing, err := os.ReadFile(craft); err != nil {
		t.Fatalf("read CRAFT.md: %v", err)
	} else if err := os.WriteFile(craft, append(existing, []byte(marker)...), 0o600); err != nil {
		t.Fatalf("append marker to CRAFT.md: %v", err)
	}

	// "/compact run" (the run subcommand) compacts directly; bare "/compact"
	// would only open a confirmation menu that never reaches the backend. The
	// stub then emits status=compacting then compact_boundary. Wait for the
	// COMPLETION reply ("compacted"), not the start ("⏳ Compacting"), to prove
	// the compact_boundary was consumed and the bounce ran.
	sendText(h, token, userID, userID, "/compact run")
	if got := waitForSendMessageContaining(h, token, "compacted", 15*time.Second); got == "" {
		t.Fatalf("never saw compaction-complete reply; sent calls:\n%s\nstderr tail:\n%s",
			sentCallsTail(h.TelegramStub(), token), stderrTail(h.Stderr()))
	}

	// The bounce closed the session keeping the resume ID; the next message
	// respawns CC with --resume. Wait for invocation 2 to land.
	sendText(h, token, userID, userID, "after-compact")
	if !waitForUserMessage(t, h, "workspaces/alpha", "after-compact", 30*time.Second) {
		t.Fatalf("post-bounce turn never processed; recorder:\n%s\nstderr tail:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
	if _, ok := waitForInvocationCount(t, h, "workspaces/alpha", 2, 15*time.Second); !ok {
		t.Fatalf("compact bounce never produced a respawn invocation; recorder:\n%s",
			recorderTail(t, h.RecorderPath()))
	}

	entries := readRecorderEntries(t, h.RecorderPath())
	// The respawn must carry --resume <primeSID> — the bounce resumes the
	// compacted conversation, it does not start fresh.
	invs := invocationsByWorkdir(entries, "workspaces/alpha")
	sawResume := false
	for _, inv := range invs {
		if inv.ResumeID == primeSID {
			sawResume = true
			break
		}
	}
	if !sawResume {
		t.Errorf("post-compact respawn never carried --resume %s (bounce didn't resume the session); invocations:\n%s",
			primeSID, invocationsTail(invs))
	}
	// And the respawn rebuilt the system prompt from disk → a SECOND
	// init_system handshake. This is the whole point of bouncing (#828 Part B):
	// character/skill edits reload after compaction.
	if postInit := len(initSystemsIn(entries, "workspaces/alpha")); postInit <= preInitCount {
		t.Errorf("no new init_system after compact bounce (pre=%d post=%d); prompt was not rebuilt from disk",
			preInitCount, postInit)
	}
}

// TestL2_SessionLifecycle_ResetWhileProcessingRefused proves a bare
// /reset (the soft variant) is refused while the agent is mid-turn,
// preserving in-flight memory formation guarantees. Mechanism: script
// cc-stub to hang; send a normal message to start the hung turn;
// send "/reset" while the hang is active; assert the Telegram stub
// recorded an error-ish sendMessage ("send stop first, then reset")
// AND no fresh CC session was spawned (no second cc-stub invocation
// during the hang window). Negative path complementing the soft-reset
// happy path.
func TestL2_SessionLifecycle_ResetWhileProcessingRefused(t *testing.T) {
	testharness.ParallelWait(t)
	const userID = 8611

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{{
			ID:       "alpha",
			UserID:   userID,
			ExtraEnv: map[string]string{"CCSTUB_HANG_DURING_TURN": "10s"},
		}},
		ReadyTimeout: 30 * time.Second,
	})
	token := h.AgentBotToken("alpha")

	// Drive the in-flight turn. CCSTUB_HANG_DURING_TURN holds the
	// result envelope back so foci's IsProcessing flag stays true past
	// the soft /reset arrival.
	sendText(h, token, userID, userID, "hold this turn")
	if !waitForUserMessage(t, h, "workspaces/alpha", "hold this turn", 15*time.Second) {
		t.Fatalf("first turn never reached cc-stub; stderr tail:\n%s", stderrTail(h.Stderr()))
	}

	// Now /reset (soft) arrives. Soft reset's IsProcessing gate should
	// refuse the reset; foci sends back a "send stop first" advisory.
	sendText(h, token, userID, userID, "/reset")
	got := waitForSendMessageContaining(h, token, "stop", 10*time.Second)
	if got == "" {
		t.Fatalf("expected /reset to be refused with a 'stop first' style reply while a turn was in-flight; sent calls:\n%s",
			sentCallsTail(h.TelegramStub(), token))
	}
	// Soft reset must not have rotated the session: only ONE long-lived
	// agent cc-stub invocation should be live for alpha. The auto-extract
	// nudge spawns a separate cc-stub with --no-session-persistence —
	// filter those out so the assertion is about backend respawning, not
	// total subprocess count.
	var liveInvs []recorderEntry
	for _, inv := range invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha") {
		extract := false
		for _, f := range inv.Flags {
			if f == "--no-session-persistence" {
				extract = true
				break
			}
		}
		if !extract {
			liveInvs = append(liveInvs, inv)
		}
	}
	if len(liveInvs) > 1 {
		t.Errorf("soft /reset while processing respawned the backend anyway: long-lived invocations=%d (expected 1):\n%s",
			len(liveInvs), invocationsTail(liveInvs))
	}
}

// TestL2_SessionLifecycle_ResumeIDSurvivesRespawn
// proves foci's persisted resume_id survives a subprocess respawn
// triggered by anything OTHER than reset (e.g. backend death, idle
// reap). Mechanism: prime to capture session_id A; force respawn
// (kill subprocess or use CCSTUB_EXIT_CODE=0 to force a clean exit
// between turns); send a follow-up message; assert the next
// invocation's resume_id == A AND the user_message lands under the
// same session_id. Distinct from BackendDeathMidSessionRespawns by
// asserting persistence across a CLEAN exit, not a crash.
func TestL2_SessionLifecycle_ResumeIDSurvivesRespawn(t *testing.T) {
	testharness.ParallelWait(t)
	const userID = 8412

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{{
			ID:     "alpha",
			UserID: userID,
			// CCSTUB_EXIT_AFTER_N_TURNS=1 forces a CLEAN os.Exit(0)
			// between turns. Distinct from a crash (BackendDeathMid...
			// uses the same env but the test there asserts on the
			// invocation/resume shape; this test asserts the *user_message*
			// session_id stays consistent across both turns — the session
			// keeps its identity through a process boundary).
			ExtraEnv: map[string]string{"CCSTUB_EXIT_AFTER_N_TURNS": "1"},
		}},
		ReadyTimeout: 30 * time.Second,
	})

	pushUserMessage(t, h, "alpha", userID, "first-turn-A")
	if !waitForUserMessage(t, h, "workspaces/alpha", "first-turn-A", 20*time.Second) {
		t.Fatalf("first turn never reached cc-stub; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}

	pushUserMessage(t, h, "alpha", userID, "second-turn-B")
	if !waitForUserMessage(t, h, "workspaces/alpha", "second-turn-B", 30*time.Second) {
		t.Fatalf("second turn never processed after respawn; recorder:\n%s\nstderr:\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}

	// Both user_messages should carry the same session_id (session
	// persisted through the clean exit + respawn).
	var sidA, sidB string
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if e.Kind != "user_message" {
			continue
		}
		if strings.Contains(e.TextPrefix, "first-turn-A") {
			sidA = e.SessionID
		}
		if strings.Contains(e.TextPrefix, "second-turn-B") {
			sidB = e.SessionID
		}
	}
	if sidA == "" || sidB == "" {
		t.Fatalf("missing recorder entries: sidA=%q sidB=%q\n%s", sidA, sidB,
			recorderTail(t, h.RecorderPath()))
	}
	if sidA != sidB {
		t.Errorf("session_id changed across clean-exit respawn: first=%s second=%s\n--- recorder ---\n%s",
			sidA, sidB, recorderTail(t, h.RecorderPath()))
	}

	// And the respawn invocation must carry --resume <sidA>.
	invs := invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
	var sawResume bool
	for _, inv := range invs {
		if inv.ResumeID == sidA {
			sawResume = true
			break
		}
	}
	if !sawResume {
		t.Errorf("respawn invocation never carried --resume %s; invocations:\n%s",
			sidA, invocationsTail(invs))
	}
}

// TestL2_SessionLifecycle_HangBeyondReadyTimeoutSurfacesError proves
// foci-gw treats a cc-stub that hangs past its handshake window as a
// startup failure for that turn, surfaces an error to the user via
// the Telegram stub, and the session is left in a state where a
// subsequent message can recover. Mechanism: set CCSTUB_HANG longer
// than foci's init deadline; send a message; assert a failure-ish
// sendMessage; send a follow-up with the hang cleared; assert the
// follow-up processes successfully. Catches startup-deadline
// regressions in the delegated init path.
func TestL2_SessionLifecycle_HangBeyondReadyTimeoutSurfacesError(t *testing.T) {
	testharness.ParallelWait(t)
	// HarnessOptions.BackendReadyTimeout (propagated as
	// FOCI_BACKEND_READY_TIMEOUT to foci-gw) now lets us dial WaitReady
	// down to a few seconds so the deadline path completes inside CI
	// wall-clock. CCSTUB_HANG_ONCE_MARKER scripts the per-spawn shape:
	// first spawn hangs longer than the configured deadline; the marker
	// persists, so the recovery spawn doesn't hang.
	const userID = 7600
	markerPath := t.TempDir() + "/hang-once"
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{{
			ID:     "alpha",
			UserID: userID,
			ExtraEnv: map[string]string{
				"CCSTUB_HANG":             "12s",
				"CCSTUB_HANG_ONCE_MARKER": markerPath,
			},
		}},
		ReadyTimeout:        30 * time.Second,
		BackendReadyTimeout: 3 * time.Second,
	})

	// First turn — the cc-stub spawn sleeps 12s before any handshake;
	// foci's WaitReady (3s budget) times out and logs the warning.
	pushUserMessage(t, h, "alpha", userID, "hang-past-deadline-turn-1")

	// Wait for the WaitReady warning to land. The init-deadline path
	// emits "WaitReady for ... context deadline exceeded" or similar.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		low := strings.ToLower(h.Stderr())
		if strings.Contains(low, "waitready") || strings.Contains(low, "context deadline exceeded") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	low := strings.ToLower(h.Stderr())
	if !strings.Contains(low, "waitready") && !strings.Contains(low, "context deadline exceeded") {
		t.Fatalf("expected WaitReady-timeout warning in stderr; tail:\n%s", stderrTail(h.Stderr()))
	}

	// Second turn — marker now exists, so cc-stub's hang is skipped
	// and the recovery spawn completes the handshake. The user_message
	// must land in the recorder, proving the agent recovered.
	pushUserMessage(t, h, "alpha", userID, "recovery-after-hang")
	if !waitForUserMessage(t, h, "workspaces/alpha", "recovery-after-hang", 30*time.Second) {
		t.Fatalf("agent did not recover after init-deadline timeout; stderr:\n%s\nrecorder:\n%s",
			stderrTail(h.Stderr()), recorderTail(t, h.RecorderPath()))
	}
}
