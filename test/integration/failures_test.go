//go:build integration

package integration

import (
	"testing"

	"foci/internal/testharness"
)

// Tests in this file exercise foci's failure-injection surfaces at the L2
// layer: every test wires foci-gw against stubbed Telegram + cc-stub edges
// and uses one of cc-stub's CCSTUB_* env vars (or the Telegram stub's
// future error-injection hooks) to force the failure, then asserts that
// foci responds without panic, surfaces a coherent error/log/Telegram
// message, and stays alive for the next turn.
//
// All tests are currently skipped — they each require a piece of harness
// scaffolding that doesn't yet exist (per-test cc-stub env injection,
// Telegram stub fault hooks, malformed-stream emitters). The comments
// capture the exact mechanism each scenario will use once that
// scaffolding lands, so the implementation work is a fill-in rather
// than a re-think.

// ---------------------------------------------------------------------------
// Backend exit / lifecycle failures
// ---------------------------------------------------------------------------

// TestL2_Failures_BackendExitsNonZeroBeforeHandshake proves foci's
// delegated path tolerates a CC subprocess that exits before the
// initialize handshake completes. cc-stub is launched with
// CCSTUB_EXIT_CODE=1 so the process dies before emitting system/init;
// foci-gw should mark the backend dead, log a coherent error, and not
// panic the gateway. A subsequent Telegram message must still spawn a
// fresh subprocess (the agent stays usable across the failure).
func TestL2_Failures_BackendExitsNonZeroBeforeHandshake(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_BackendExitsAfterHandshakeMidTurn proves foci handles
// a CC subprocess that completes the init handshake but dies before
// emitting a result message. The scenario seeds cc-stub to exit between
// the assistant block and the result line; foci's reader sees EOF mid-
// turn and must finalize the in-flight turn (so OnTurnComplete fires
// with an error), restart the subprocess on the next user message, and
// not leak the dangling turn-handler.
func TestL2_Failures_BackendExitsAfterHandshakeMidTurn(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_BackendFailsOnResumeRetriesFresh is the explicit
// regression net for the CCSTUB_FAIL_ON_RESUME path (the
// "delegated: backend died during init with --resume <ID> — retrying
// without resume" warning in delegated_manager.go). The harness pre-
// seeds a stale cc_resume_id, sets CCSTUB_FAIL_ON_RESUME=1, and pushes
// a Telegram update. Foci should observe the first spawn's exit-on-
// resume, clear the resume id, relaunch cc-stub WITHOUT --resume, and
// process the message. Assertion: two invocation entries in the
// recorder for the agent's workdir — the first with non-empty
// resume_id, the second with empty resume_id — and one user_message
// entry confirming the turn finished.
func TestL2_Failures_BackendFailsOnResumeRetriesFresh(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_BackendHangsBeforeReady proves foci's WaitReady path
// respects the configured ready timeout when CC never emits system/init.
// cc-stub is launched with CCSTUB_HANG longer than the agent's ready
// budget; foci should abandon the spawn, log the timeout, and not block
// the platform poll loop. A second Telegram update after the timeout
// must still trigger a fresh subprocess spawn.
func TestL2_Failures_BackendHangsBeforeReady(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_BackendHangsDuringTurn proves foci's per-turn idle
// detector (the ActivityChecker path in delegator) fires when cc-stub
// emits system/init then goes silent past the agent's turn-stall
// threshold. Foci should surface a turn-failure result to the user
// (Telegram sendMessage containing a stall error) without killing the
// subprocess prematurely, and a follow-up Telegram update must process
// normally — proving the stall path is recoverable.
func TestL2_Failures_BackendHangsDuringTurn(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_BackendKilledMidTurnByGateway proves Close()'s
// SIGTERM→SIGKILL escalation works when cc-stub ignores SIGTERM. The
// scenario forces a backend restart while a turn is in flight; foci's
// finalizeOnce gate must ensure OnTurnComplete fires exactly once even
// when both the waiter goroutine and the reader goroutine race to
// notice the death. Assertion: exactly one user_message entry per
// dispatched turn — no duplicates, no losses.
func TestL2_Failures_BackendKilledMidTurnByGateway(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// ---------------------------------------------------------------------------
// Malformed stream-json from CC
// ---------------------------------------------------------------------------

// TestL2_Failures_MalformedJSONLineSurfacesError proves foci's
// ccstream reader surfaces a parse error via OnReaderStopped when CC
// emits an unparseable NDJSON line. cc-stub is extended with a script
// flag that injects a literal "{not json" line between init and the
// assistant message; foci should not panic and should mark the backend
// dead so the next user message triggers a clean relaunch.
func TestL2_Failures_MalformedJSONLineSurfacesError(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_UnknownEnvelopeTypeIgnored proves foci tolerates
// stream-json envelopes with unrecognised "type" fields (e.g. a future
// CC release that adds a new message kind foci hasn't taught itself).
// cc-stub emits `{"type":"unknown_future_type","payload":...}` between
// system/init and the assistant message; foci should log+skip and let
// the turn complete normally. Negative: no error reaches the user.
func TestL2_Failures_UnknownEnvelopeTypeIgnored(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_OversizedJSONLineRejected proves foci's 1MB
// per-line scanner cap (maxTokenSize in ccstream/reader.go) trips
// cleanly when cc-stub emits a >1MB assistant text block. The reader
// should stop with bufio.ErrTooLong wrapped via OnReaderStopped, the
// backend marks dead, and the next turn relaunches. Assertion: stderr
// contains the scanner-overflow log line and the second message lands
// in the recorder.
func TestL2_Failures_OversizedJSONLineRejected(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_AssistantMessageMissingContent proves foci tolerates
// a malformed assistant envelope where the message.content array is
// absent or empty. cc-stub emits the envelope shell with no blocks;
// foci should finalize the turn with empty text rather than firing
// OnText("") repeatedly or panicking on a nil dereference. Assertion:
// no sendMessage call (empty text suppressed) and the next turn
// processes normally.
func TestL2_Failures_AssistantMessageMissingContent(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_ResultMessageMissingSessionID proves foci doesn't
// crash when a result envelope omits session_id (real CC always sets
// it, but the contract isn't enforced at the type layer). cc-stub
// emits a bare result line; foci should still close the turn cleanly
// — the session_id from the prior init message is the authoritative
// one for resume purposes.
func TestL2_Failures_ResultMessageMissingSessionID(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// ---------------------------------------------------------------------------
// Telegram API failures (require TelegramStub error-injection hooks)
// ---------------------------------------------------------------------------

// TestL2_Failures_TelegramSendMessage5xxLogsAndDrops proves foci's
// outbound sendMessage path logs a sanitized error and does NOT panic
// when Telegram returns a 502/503. The harness extends TelegramStub
// with InjectError(method, code) so the next sendMessage call fails;
// foci should record the failure in stderr and continue polling for
// new updates (the bot stays alive).
func TestL2_Failures_TelegramSendMessage5xxLogsAndDrops(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_TelegramSendMessage429SurfacesRateLimit proves foci
// distinguishes Telegram's 429 (rate limited, retry_after present)
// from generic 5xx. The stub returns
// `{"ok":false,"error_code":429,"parameters":{"retry_after":2}}` on
// the next sendMessage; foci should log the rate-limit condition and
// not retry within the retry_after window. After the window, a
// subsequent send goes through.
func TestL2_Failures_TelegramSendMessage429SurfacesRateLimit(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_TelegramGetUpdatesConnectionDropReconnects proves
// foci's polling loop in bot_poll.go recovers from a connection drop
// mid-long-poll. The stub closes the connection after 100ms on the
// next getUpdates; foci's consecutiveErrors counter should increment,
// stay below the errorEscalateThreshold for a few cycles, then succeed
// once the stub stops injecting. A Telegram message sent after recovery
// must process — proving the bot didn't permanently wedge.
func TestL2_Failures_TelegramGetUpdatesConnectionDropReconnects(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_TelegramGetUpdatesPersistent5xxEscalatesLog proves
// foci escalates the get-updates failure log from Debug to Error after
// errorEscalateThreshold (5) consecutive failures. The stub returns
// 502 to every getUpdates call; the test reads stderr and asserts that
// the "5 consecutive failures" line is present. Recovery: stop the
// injection and confirm polling resumes.
func TestL2_Failures_TelegramGetUpdatesPersistent5xxEscalatesLog(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_TelegramSendMessageMalformedJSONResponse proves
// foci tolerates a Telegram response whose body isn't `{"ok":...}` —
// e.g. a CDN error page intercepted between bot and stub. The stub is
// extended to return `<html>...</html>` with HTTP 200; foci should log
// the parse error via sanitizeError and continue polling.
func TestL2_Failures_TelegramSendMessageMalformedJSONResponse(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_TelegramUnknownTokenReceives404 proves the stub's
// own contract: a bot foci-gw starts for an unregistered token gets a
// 404 on getMe and the gateway fails to come ready. This is a meta-
// test guarding against config drift between agents.toml and the
// harness's RegisterBot calls — silent token mismatches would
// otherwise produce a 60s hang on the ready signal.
func TestL2_Failures_TelegramUnknownTokenReceives404(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// ---------------------------------------------------------------------------
// Tool dispatch / exec bridge failures
// ---------------------------------------------------------------------------

// TestL2_Failures_ToolReturnsErrorJSONReachesBackend proves foci's
// exec bridge surfaces a tool's error JSON as a tool_result with
// is_error=true back to the CC subprocess — the contract real CC
// expects so it can decide whether to retry. cc-stub scripts a Bash
// tool_use that runs `foci_http_request bogus://url` (deliberately
// malformed scheme); the bridge dispatches to the http tool, which
// returns an error map. Assertion: the next user_message recorder
// entry on the same workdir contains the error marker, confirming
// foci re-fed it to CC.
func TestL2_Failures_ToolReturnsErrorJSONReachesBackend(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_UnknownToolInBashCommandFailsCleanly proves the exec
// bridge rejects unknown tool names without crashing the subprocess.
// cc-stub runs `foci_does_not_exist arg1 arg2`; the shell function
// isn't defined so bash returns 127, cc-stub's bash subshell exits
// non-zero, but the assistant message + result still flow back and
// the agent stays alive.
func TestL2_Failures_UnknownToolInBashCommandFailsCleanly(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_SendToSessionUnknownTargetLogged proves the
// send_to_session tool surfaces a clear error when the partial key
// doesn't resolve to any registered agent. cc-stub scripts
// `foci_send_to_session ghost/c1234 --message hi`; foci's
// NewSendToSessionTool resolver returns an error which the exec bridge
// JSON-encodes back to the shell function. The test asserts the stderr
// contains the "no session matches" line AND that no user_message
// entry lands in any workspaces/* dir (the dispatch was correctly
// dropped).
func TestL2_Failures_SendToSessionUnknownTargetLogged(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_SendToSessionAmbiguousPartialKeyRejects proves the
// partial-key resolver rejects ambiguous matches. Two agents are
// registered for the same user_id (alpha and beta), and the script
// calls `foci_send_to_session c<USER>/x --message hi` — a prefix that
// matches both. Foci should refuse to dispatch and surface "ambiguous
// session key" rather than silently picking one. Negative: neither
// agent's workdir receives the marker.
func TestL2_Failures_SendToSessionAmbiguousPartialKeyRejects(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_ExecBridgeSocketUnreachable proves a tool call that
// can't reach the per-session bridge socket (e.g. FOCI_SOCK points at
// a stale path because the previous backend Close() didn't fire)
// surfaces a connection error to the calling Bash, doesn't hang the
// turn, and lets the turn complete with the surfaced error in the
// tool's stderr. The harness simulates this by deleting the socket
// file out from under cc-stub between init and the scripted tool_use.
func TestL2_Failures_ExecBridgeSocketUnreachable(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_BashToolUseExceedsCCStubTimeout proves cc-stub's own
// 10-second wall-clock guard on Bash tool_use commands fires when a
// script triggers a deliberately slow command (`sleep 30`). The stub
// kills the subshell, logs the timeout to stderr, and emits the
// assistant + result so foci finalizes the turn rather than hanging.
// Assertion: stderr contains "Bash command timed out" and a follow-up
// Telegram message processes within the normal time budget.
func TestL2_Failures_BashToolUseExceedsCCStubTimeout(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// ---------------------------------------------------------------------------
// Notifier / cross-agent dispatch failures
// ---------------------------------------------------------------------------

// TestL2_Failures_CrossAgentDispatchToStoppedAgentDrops proves the
// session_router's invariant guard catches an attempt to dispatch to
// an agent whose Bot is no longer running. The harness starts two
// agents, stops one mid-test (via a /reload-equivalent hook), then
// triggers a send_to_session targeting the stopped agent. Foci should
// log the drop and not panic; the calling agent's turn completes with
// the error injected as a tool_result.
func TestL2_Failures_CrossAgentDispatchToStoppedAgentDrops(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_CrossAgentDispatchPanicIsRecovered proves the
// notifier's per-dispatch defer-recover (the goroutine that calls
// HandleMessage on the target Agent) catches a panic in the receiving
// agent's path and logs a stack trace without taking down the
// gateway. The harness uses a cc-stub script that triggers a code
// path with a deliberate panic-injection env flag (extension to
// cc-stub).
func TestL2_Failures_CrossAgentDispatchPanicIsRecovered(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// ---------------------------------------------------------------------------
// Session storage corruption
// ---------------------------------------------------------------------------

// TestL2_Failures_SessionStoreCorruptedJSONLOnStartup proves the
// session store skips unparseable lines on load rather than refusing
// to start. The harness pre-seeds the data dir with a sessions JSONL
// containing a mix of valid + truncated lines; foci should log the
// parse warnings and still serve the agent. Negative: foci-gw doesn't
// crash on startup.
func TestL2_Failures_SessionStoreCorruptedJSONLOnStartup(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_SessionStoreMissingBranchMetaTreatedAsRoot proves a
// branch JSONL with no branch_meta first line is treated as a root
// session, not silently dropped. The harness pre-seeds a branch file
// missing the meta line; foci's session loader should log the
// missing-meta warning and surface the session in /sessions.
func TestL2_Failures_SessionStoreMissingBranchMetaTreatedAsRoot(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_SessionStoreReadOnlyDirSurfacesError proves foci's
// session-append path surfaces a coherent "permission denied" log when
// the sessions dir is chmod'd read-only mid-run. The harness chmods
// the dir after StartGateway returns; the next user message's turn
// should fail with a logged error and the gateway should not crash.
// Recovery: restore permissions, send another message, confirm it
// lands.
func TestL2_Failures_SessionStoreReadOnlyDirSurfacesError(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// ---------------------------------------------------------------------------
// Configuration / startup failures
// ---------------------------------------------------------------------------

// TestL2_Failures_MissingClaudeBinaryFailsAgentStartup proves foci's
// agent setup logs a clear error when [cc_backend].claude_binary
// points at a non-existent path. The harness writes a foci.toml with
// claude_binary="/nonexistent/cc-stub"; the gateway should NOT come
// ready (no "started N agent(s)" line within the timeout) and stderr
// should name the missing path.
func TestL2_Failures_MissingClaudeBinaryFailsAgentStartup(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_DuplicateBotTokenAcrossAgentsRejected proves
// agents_setup.go's validator catches two agents pointing at the same
// Telegram bot token (which would cause cross-talk in the long-poll).
// The harness assigns the same token to both fotini and clutch; the
// gateway should fail to come ready with a clear "duplicate token"
// error in stderr.
func TestL2_Failures_DuplicateBotTokenAcrossAgentsRejected(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_MalformedTOMLConfigFailsLoad proves the gateway's
// config loader surfaces a parser error rather than silently using
// defaults when foci.toml has a syntax error. The harness writes a
// truncated toml file; the gateway must exit non-zero with the parse
// error on stderr, and the harness's waitForReady must observe the
// process death (the "exited before signalling ready" path).
func TestL2_Failures_MalformedTOMLConfigFailsLoad(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_MissingSecretsFileWarnsButStarts proves the gateway
// can start with no secrets file (none of the test agents reference
// secrets) and logs a single warning rather than crashing. The
// harness omits writeTestSecrets; agents should still come up because
// cc-stub doesn't need any keys. Asserts the "no secrets file" warning
// is present and a Telegram round-trip works.
func TestL2_Failures_MissingSecretsFileWarnsButStarts(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// ---------------------------------------------------------------------------
// Concurrency / queueing failures
// ---------------------------------------------------------------------------

// TestL2_Failures_ConcurrentMessagesDuringHangNotLost proves the
// agent's per-session inbox queues incoming messages while a turn is
// stalled inside cc-stub (CCSTUB_HANG) and drains them in arrival
// order once the stall clears. The harness sends 3 Telegram messages
// in rapid succession to an agent whose first message is hanging;
// after the hang releases, all 3 should appear as user_message entries
// in arrival order. Proves no inbox drop on slow turns.
func TestL2_Failures_ConcurrentMessagesDuringHangNotLost(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Failures_RestartDuringInFlightTurnDoesNotDoubleCount proves
// foci's finalizeOnce gate guarantees OnTurnComplete fires exactly
// once when both Close() and the subprocess's natural exit race. The
// harness triggers Restart() at the moment cc-stub emits its result
// line; the assertion counts user_message entries in the recorder
// and expects exactly one per dispatched Telegram update — not zero,
// not two.
func TestL2_Failures_RestartDuringInFlightTurnDoesNotDoubleCount(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}
