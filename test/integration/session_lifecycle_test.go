//go:build integration

package integration

import (
	"testing"

	"foci/internal/testharness"
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

// TestL2_SessionLifecycle_ResumeIDPassedOnSecondTurn proves foci tracks
// the cc_resume_id emitted in cc-stub's system/init message and passes
// it as --resume on the NEXT subprocess invocation for the same session
// key. Mechanism: send two messages with a pause forcing the first
// subprocess to exit (CCSTUB_EXIT_CODE on a control env, or by waiting
// past the idle window); assert the second cc-stub invocation in the
// recorder carries the same resume_id field. Regression net for the
// state.db "cc_resume_id" persistence path in DelegatedManager.
func TestL2_SessionLifecycle_ResumeIDPassedOnSecondTurn(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SessionLifecycle_MultiTurnSharesSessionID proves three
// sequential user messages within the same Telegram chat all process
// through ONE long-lived cc-stub subprocess and produce three
// user_message recorder entries that share a single session_id.
// Mechanism: push three updates on the same chat, poll the recorder
// until three user_message entries appear under that workdir, group by
// session_id and assert cardinality of 1. Catches accidental
// per-message subprocess spawning and session-key churn.
func TestL2_SessionLifecycle_MultiTurnSharesSessionID(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SessionLifecycle_ResetSoftRotatesSessionKey proves a /reset
// command on an established session destroys the delegated backend,
// rotates foci's session key, and the NEXT user message spawns a fresh
// cc-stub subprocess with NO --resume flag. Mechanism: prime the
// session, send "/reset", send another message; assert two
// invocations in the recorder where the second has empty resume_id
// and a different session_id from the first.
func TestL2_SessionLifecycle_ResetSoftRotatesSessionKey(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SessionLifecycle_ResetHardCancelsInFlightTurn proves
// /reset hard fires while a turn is mid-flight, cancels it, destroys
// the backend, and the next message starts a clean fresh session
// without --resume. Mechanism: script cc-stub to stall via CCSTUB_HANG
// on its next user message; send a normal message to start the hung
// turn; send "/reset hard"; send a follow-up message; assert the
// follow-up shows up as a user_message with no resume_id and a new
// session_id. Catches the CancelSession + StopSession + RotateKey
// sequencing in ResetSessionHard.
func TestL2_SessionLifecycle_ResetHardCancelsInFlightTurn(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SessionLifecycle_ResetClearsPersistedResumeID proves /reset
// clears the cc_resume_id row in state.db so a service-restart-like
// fresh-spawn after reset doesn't accidentally try to resume the old
// session. Mechanism: prime + reset; assert that any subsequent
// invocation in the recorder for that workdir has an empty resume_id
// field. The negative half of ResumeIDPassedOnSecondTurn.
func TestL2_SessionLifecycle_ResetClearsPersistedResumeID(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
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
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
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
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SessionLifecycle_QueuedMessageProcessedAfterBusyTurn proves
// foci's per-session inbox queues a follow-up Telegram message that
// arrives WHILE the current turn is still running, then drives it as
// a follow-up turn (or batches it into the current turn) once the
// first turn completes. Mechanism: script cc-stub with CCSTUB_HANG=2s
// so the first user message holds the worker; push a second update
// during the hang window; assert two user_message recorder entries
// for that session, in order. Catches inbox batching / queue-full
// regressions.
func TestL2_SessionLifecycle_QueuedMessageProcessedAfterBusyTurn(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SessionLifecycle_PerChatSessionsIsolated proves two distinct
// Telegram chats on the same agent each get their own session_id and
// their own per-session inbox worker — a message in chat A does not
// pollute chat B's session. Mechanism: register two chat IDs against
// one agent, push one message to each, assert two distinct
// user_message session_ids in the recorder under the same agent
// workdir. Catches regressions in chat-keyed session-key resolution.
func TestL2_SessionLifecycle_PerChatSessionsIsolated(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
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
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SessionLifecycle_EmptyMessageNotDispatched proves a Telegram
// update with empty text and no attachments is filtered out before it
// reaches the agent inbox. Mechanism: push an Update whose Message has
// Text="" and no attachments; wait the polling window; assert NO
// invocation or user_message in the recorder for that workdir.
// Negative scenario: catches accidental dispatch of empty turns that
// would burn a CC subprocess and write nothing useful.
func TestL2_SessionLifecycle_EmptyMessageNotDispatched(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
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
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
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
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SessionLifecycle_ResetWhileProcessingRefused proves a bare
// /reset (the soft variant) is refused while the agent is mid-turn,
// preserving in-flight memory formation guarantees. Mechanism: script
// cc-stub to hang; send a normal message to start the hung turn;
// send "/reset" while the hang is active; assert the Telegram stub
// recorded an error-ish sendMessage ("send stop first, then reset")
// AND no session rotation occurred (no second cc-stub invocation
// during the hang window). Negative path complementing the soft-reset
// happy path.
func TestL2_SessionLifecycle_ResetWhileProcessingRefused(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_SessionLifecycle_ResumeIDPersistsAcrossSubprocessRespawn
// proves foci's persisted resume_id survives a subprocess respawn
// triggered by anything OTHER than reset (e.g. backend death, idle
// reap). Mechanism: prime to capture session_id A; force respawn
// (kill subprocess or use CCSTUB_EXIT_CODE=0 to force a clean exit
// between turns); send a follow-up message; assert the next
// invocation's resume_id == A AND the user_message lands under the
// same session_id. Distinct from BackendDeathMidSessionRespawns by
// asserting persistence across a CLEAN exit, not a crash.
func TestL2_SessionLifecycle_ResumeIDPersistsAcrossSubprocessRespawn(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
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
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}
