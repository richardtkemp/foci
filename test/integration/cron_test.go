//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/provision"
	"foci/internal/testharness"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// Cron / scheduled-trigger tests for the L2 layer.
//
// foci has three families of "fires-on-its-own" machinery, all of which
// reach foci's agent dispatch path without a Telegram message arriving:
//
//  1. Internal periodic timers in internal/periodic — keepalive,
//     background work, reflection, consolidation. Each ticks every 30s
//     and decides whether to fire based on config interval, in-flight
//     state, mana/rate-limit gating, etc.
//
//  2. Wake reminders via the `remind` tool (internal/tools/remind.go +
//     buildWakeScheduler in cmd/foci-gw/agents_notify.go). An agent
//     schedules a deferred message to itself, which fires via a
//     time.After goroutine and lands as an injected turn.
//
//  3. External system cron entries provisioned from
//     shared/crontab.template via internal/provision/crontab.go. These
//     shell out to `foci branch --oneshot`, so L2 can drive them by
//     invoking the same CLI path.
//
// Per the README, L2 stays away from real time-warp: tests configure
// short intervals (via the config writer) or pre-seed in-memory state
// so the next tick fires. Tests requiring hour-scale waits or true
// wall-clock fidelity belong in L3.

// gwSocketPath derives the foci-gw Unix socket path from the harness's
// known layout. The harness creates `tempDir/data/foci-gw.sock` (writes
// data_dir=tempDir/data in the test config; foci-gw's default socket
// path is "<data_dir>/foci-gw.sock"). The recorder path lives at
// tempDir/cc-recorder.jsonl, so we recover tempDir by taking its dir.
func gwSocketPath(h *testharness.Harness) string {
	tempDir := filepath.Dir(h.RecorderPath())
	return filepath.Join(tempDir, "data", "foci-gw.sock")
}

// gwUnixClient returns an *http.Client that dials the foci-gw Unix
// socket. Same-user peer credentials are checked by the gateway; the
// test process is the same user that spawned foci-gw so the check
// passes. Use base URL "http://foci-gw" — the host is ignored.
func gwUnixClient(sockPath string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}
}

// waitForSocket polls until the gateway's unix socket is reachable.
func waitForSocket(t *testing.T, sockPath string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fi, err := os.Lstat(sockPath); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for gateway socket at %s", sockPath)
}

// TestL2_Cron_KeepaliveFiresAtConfiguredInterval proves the keepalive
// timer in internal/periodic dispatches a branch session with the
// keepalive prompt when the cache-age threshold is crossed. With a
// sub-minute interval set in the test config and the cache age forced
// past it, the next 30s tick should call branchFn("keepalive", ...),
// which routes through Agent.Branch and spawns cc-stub in the agent's
// workdir. The recorder should show an extra invocation for the agent
// without any Telegram update having been pushed.
func TestL2_Cron_KeepaliveFiresAtConfiguredInterval(t *testing.T) {
	t.Skip("HARNESS GAP: writeTestConfig has no way to inject [keepalive] section " +
		"(enabled, interval). Without that, keepalive is disabled by default " +
		"and the tick will never fire within a test-scale window. Needs " +
		"HarnessOptions.Keepalive / .ExtraConfigTOML or similar.")
}

// TestL2_Cron_KeepaliveSkippedWhenCachingUnavailable proves the
// keepalive guard at the top of maybeKeepalive: if the provider client
// reports caching is unavailable (or the explicit cachingOverride is
// false), no branch is dispatched even after the interval elapses. The
// stub backend exposes no real caching, so configuring keepalive
// without a caching-capable model should leave the recorder free of
// keepalive invocations across multiple ticks.
func TestL2_Cron_KeepaliveSkippedWhenCachingUnavailable(t *testing.T) {
	t.Skip("HARNESS GAP: requires [keepalive] config injection AND a way to " +
		"select a non-caching model in the test config. writeTestConfig only " +
		"emits the anthropic/claude-haiku-4-5 stub model. Needs harness support " +
		"for selecting/overriding the model and enabling keepalive.")
}

// TestL2_Cron_KeepaliveSkippedWhenTurnInFlight proves the in-flight
// guard added by TODO #760: if a user turn is mid-flight on the parent
// session when the keepalive tick fires, the runner defers rather than
// queueing the keepalive prompt as a SourceUser follow-up. The test
// holds a turn open (cc-stub script with a long-running Bash tool_use)
// while the keepalive interval elapses, then verifies no keepalive
// invocation was recorded until after the held turn completes.
func TestL2_Cron_KeepaliveSkippedWhenTurnInFlight(t *testing.T) {
	t.Skip("HARNESS GAP: requires [keepalive] config injection so the periodic " +
		"runner is actually configured. See TestL2_Cron_KeepaliveFiresAtConfiguredInterval.")
}

// TestL2_Cron_KeepaliveDoesNotReplyToTelegram proves the keepalive
// prompt "[KEEPALIVE] ... respond with [[NO_RESPONSE]]" is treated as
// internal: even though cc-stub by default echoes the user text, the
// runner branches with noCompact=true into a fresh session and the
// branch egress path must not produce a sendMessage to the Telegram
// stub. The assertion is the absence of any user-visible message for
// the keepalive prompt body in the Telegram stub's call log.
func TestL2_Cron_KeepaliveDoesNotReplyToTelegram(t *testing.T) {
	t.Skip("HARNESS GAP: requires [keepalive] config injection. " +
		"See TestL2_Cron_KeepaliveFiresAtConfiguredInterval.")
}

// TestL2_Cron_BackgroundFiresWhenIdleWithOpenTodos proves the
// background-work scheduler in internal/periodic fires when (a) the
// configured idle interval has elapsed since last interaction, (b)
// there's at least one open todo tagged "background", and (c) mana /
// rate-limit gating allows. The test seeds a background todo via the
// todo store path, leaves the agent idle past the interval, and
// asserts a recorder entry with the background prompt appears.
func TestL2_Cron_BackgroundFiresWhenIdleWithOpenTodos(t *testing.T) {
	t.Skip("HARNESS GAP: requires (a) [background] config injection to set a " +
		"sub-minute interval, and (b) a way to seed an open background-tagged " +
		"todo into the per-agent todo.db before/while foci-gw is running. " +
		"The harness exposes neither. Needs HarnessOptions.Background / " +
		"ExtraConfigTOML AND a SeedTodo(agentID, text, tags) helper or " +
		"workspace-path accessor.")
}

// TestL2_Cron_BackgroundSkippedWithNoOpenTodos proves the explicit
// "no open background todos" skip branch in maybeBackgroundWork: even
// when idle past the interval, with no qualifying todos the scheduler
// must not dispatch. Asserts the recorder shows no background-prompt
// invocation across multiple ticks when the todo store is empty.
func TestL2_Cron_BackgroundSkippedWithNoOpenTodos(t *testing.T) {
	t.Skip("HARNESS GAP: requires [background] config injection (sub-minute " +
		"interval). See TestL2_Cron_BackgroundFiresWhenIdleWithOpenTodos.")
}

// TestL2_Cron_BackgroundCooldownPreventsSelfChaining proves the
// post-completion cooldown enforced by maybeBackgroundWork: the
// scheduler refuses to start another background session within the
// configured interval of the last one ending. This is the regression
// net for the orphaned-tmux-child accumulation bug — without the
// cooldown, each completed background session would immediately trigger
// the next. The test runs one background session, then verifies the
// next tick declines despite open todos remaining.
func TestL2_Cron_BackgroundCooldownPreventsSelfChaining(t *testing.T) {
	t.Skip("HARNESS GAP: requires [background] config injection + todo seeding. " +
		"See TestL2_Cron_BackgroundFiresWhenIdleWithOpenTodos.")
}

// TestL2_Cron_BackgroundSkippedWhileActiveTmuxWatches proves the
// hasActiveWorkFn gate: when the agent has live tmux watches (the
// callback returns >0), the background scheduler defers regardless of
// idle time or open todos. Test wires a stub HasActiveWorkFn returning
// 1 and confirms no dispatch occurs.
func TestL2_Cron_BackgroundSkippedWhileActiveTmuxWatches(t *testing.T) {
	t.Skip("HARNESS GAP: HasActiveWorkFn is wired internally in foci-gw " +
		"(periodic_setup.go) from inst.tmuxWatchCount — there's no test " +
		"surface to inject a stub. Needs harness support to override the " +
		"callback, OR a way to spawn a real foci_tmux watch in the agent's " +
		"workdir to drive the count via the existing path.")
}

// TestL2_Cron_ReflectionFiresOnInterval proves the periodic reflection
// pass dispatches a branch session with the reflection prompt for each
// session reported as needing reflection by SessionIndex. After a
// Telegram message creates an active session, a configured short
// reflection interval should cause the next eligible tick to branch
// from that session key and run the reflection prompt in cc-stub. The
// assertion is a recorder entry whose text prefix matches the
// reflection prompt header.
func TestL2_Cron_ReflectionFiresOnInterval(t *testing.T) {
	t.Skip("HARNESS GAP: requires [reflection] config injection (interval, " +
		"interval_enabled). Defaults are 1h interval — the periodic ticker " +
		"won't fire within any reasonable test window. Needs " +
		"HarnessOptions.Reflection / ExtraConfigTOML.")
}

// TestL2_Cron_ReflectionStampPreventsImmediateRefire proves the
// SessionIndex.StampReflection write inside maybeReflection: once a
// session has been reflected on, subsequent ticks within the configured
// interval skip it (the "no sessions need reflection" path). Asserts at
// most one reflection invocation per session across a window of
// multiple ticks.
func TestL2_Cron_ReflectionStampPreventsImmediateRefire(t *testing.T) {
	t.Skip("HARNESS GAP: requires [reflection] config injection. " +
		"See TestL2_Cron_ReflectionFiresOnInterval.")
}

// TestL2_Cron_ReflectionDeferredWhenAllSessionsBusy proves the
// in-flight filter in maybeReflection (TODO #760): when every session
// flagged as needing reflection has an active turn, the reflection
// pass logs "all candidate sessions have in-flight turns" and skips.
// Test holds a turn open via cc-stub and asserts no reflection
// recorder entry appears for that session until the turn completes.
func TestL2_Cron_ReflectionDeferredWhenAllSessionsBusy(t *testing.T) {
	t.Skip("HARNESS GAP: requires [reflection] config injection. " +
		"See TestL2_Cron_ReflectionFiresOnInterval.")
}

// TestL2_Cron_ReflectionDisabledWhenIntervalEnabledFalse proves the
// IntervalEnabled=false config path: even with eligible sessions and a
// short interval, the scheduler returns immediately without
// dispatching. Asserts the recorder shows zero reflection invocations.
func TestL2_Cron_ReflectionDisabledWhenIntervalEnabledFalse(t *testing.T) {
	t.Skip("HARNESS GAP: requires [reflection] config injection (specifically " +
		"interval_enabled=false). See TestL2_Cron_ReflectionFiresOnInterval.")
}

// TestL2_Cron_ConsolidationFiresOnLongerInterval proves consolidation
// dispatches via RunOnceFunc (when set) or branchFn (otherwise) with
// the memory-consolidation prompt. With a short ConsolidationInterval
// and recent interaction, the scheduler should call the RunOnce path
// once per interval. Asserts on the recorder for a consolidation
// prompt invocation in the agent's workdir.
func TestL2_Cron_ConsolidationFiresOnLongerInterval(t *testing.T) {
	t.Skip("HARNESS GAP: requires [reflection] config injection " +
		"(consolidation_interval, consolidation_enabled). Default is 20h. " +
		"See TestL2_Cron_ReflectionFiresOnInterval.")
}

// TestL2_Cron_ConsolidationSkippedWhileReflectionRunning proves the
// "reflection running" guard in maybeConsolidation: if a reflection
// pass is in flight when the consolidation tick fires, consolidation
// defers. Test forces overlap by scripting cc-stub to hang during
// reflection and asserts consolidation only runs after reflection
// completes.
func TestL2_Cron_ConsolidationSkippedWhileReflectionRunning(t *testing.T) {
	t.Skip("HARNESS GAP: requires [reflection] config injection for both " +
		"reflection and consolidation timers. See TestL2_Cron_ReflectionFiresOnInterval.")
}

// TestL2_Cron_ConsolidationTimestampPersistsAcrossRestart proves the
// SessionIndex.SetAgentMetadata("consolidation_last", ...) write at
// the end of maybeConsolidation survives a foci-gw restart: a second
// harness pointed at the same DataDir should pick up the timestamp
// and refuse to fire consolidation again within the configured
// interval. Asserts at most one consolidation invocation across both
// processes.
func TestL2_Cron_ConsolidationTimestampPersistsAcrossRestart(t *testing.T) {
	t.Skip("HARNESS GAP: requires (a) [reflection] config injection AND (b) a " +
		"way to start a second harness against the same DataDir / workspaces. " +
		"StartGateway always allocates a fresh t.TempDir() for both, so state " +
		"can't survive a restart. Needs HarnessOptions.ReuseDir / .DataDir.")
}

// TestL2_Cron_WakeReminderFiresAfterDelay proves the remind tool's
// wake path end-to-end via the exec bridge: scripted cc-stub runs
// `foci_remind --text "wake me" --when 2s --wake true`, which writes
// a row to ReminderStore and spawns a time.After goroutine. After the
// delay, buildWakeScheduler.wakeScheduleFn injects a SCHEDULED WAKE
// turn into the originating session. Asserts the recorder shows a
// user_message containing the SCHEDULED WAKE header plus the original
// reminder text.
func TestL2_Cron_WakeReminderFiresAfterDelay(t *testing.T) {
	const testUserID = 5101
	const reminderText = "MARKER_WAKE_FIRES_AFTER_DELAY"

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		ReadyTimeout: 30 * time.Second,
	})

	// Script alpha's cc-stub: on the next user message, the assistant
	// emits a Bash tool_use running foci_remind ... --wake. The exec
	// bridge dispatches to the remind tool, which schedules the wake
	// via buildWakeScheduler's time.After goroutine.
	bashCmd := fmt.Sprintf(`foci_remind --text %q --when 2s --wake`, reminderText)
	scriptBody, err := json.Marshal(map[string]any{
		"text": "scheduling wake",
		"tool_uses": []map[string]any{
			{"name": "Bash", "input": map[string]any{"command": bashCmd}},
		},
	})
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "please set a wake",
		},
	})

	// Wait for the wake to fire (2s delay + injection time + processing).
	// The injected user message will carry "[SCHEDULED WAKE @ ..." plus
	// the reminder text in its body.
	if !waitForUserMessage(t, h, "workspaces/alpha", "SCHEDULED WAKE", 20*time.Second) {
		t.Fatalf("SCHEDULED WAKE never landed in alpha's workdir\n--- recorder ---\n%s\n--- stderr tail ---\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
	// Also verify the reminder text reaches the user_message body.
	found := false
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if e.Kind == "user_message" && strings.Contains(e.Workdir, "workspaces/alpha") &&
			strings.Contains(e.TextPrefix, "SCHEDULED WAKE") && strings.Contains(e.TextPrefix, reminderText) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("SCHEDULED WAKE injection lacked reminder text %q in body\n%s",
			reminderText, recorderTail(t, h.RecorderPath()))
	}
}

// TestL2_Cron_WakeReminderSurvivesRestart proves
// ReminderStore.PendingWakes + buildWakeScheduler's restore loop:
// when a wake is scheduled, foci-gw is stopped before it fires, and a
// fresh foci-gw is started against the same DataDir, the pending wake
// should be re-scheduled and still fire. The injected turn lands in
// the second process's recorder.
func TestL2_Cron_WakeReminderSurvivesRestart(t *testing.T) {
	t.Skip("HARNESS GAP: StartGateway always creates a fresh t.TempDir() and " +
		"there's no way to (a) shut down the first instance from within the " +
		"test (cleanup is via t.Cleanup), or (b) point a second StartGateway " +
		"at the first DataDir/workspaces. Needs HarnessOptions.DataDir + " +
		"Harness.Shutdown() (or equivalent) to drive cross-process restart.")
}

// TestL2_Cron_WakeReminderRejectsNegativeDelay proves the
// resolveWakeDuration validation: when the cc-stub script invokes
// `foci_remind ... --when -5s --wake true`, the tool returns an error
// without writing to ReminderStore or scheduling a goroutine. Asserts
// the recorder shows no SCHEDULED WAKE injection and the tool exec
// produced a non-zero result.
func TestL2_Cron_WakeReminderRejectsNegativeDelay(t *testing.T) {
	const testUserID = 5102

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		ReadyTimeout: 30 * time.Second,
	})

	bashCmd := `foci_remind --text "bad" --when -5s --wake`
	scriptBody, err := json.Marshal(map[string]any{
		"text": "trying invalid wake",
		"tool_uses": []map[string]any{
			{"name": "Bash", "input": map[string]any{"command": bashCmd}},
		},
	})
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "kick off bad wake",
		},
	})

	// First, wait for the initial user_message to be recorded so we
	// know the bash tool_use has run.
	if !waitForUserMessage(t, h, "workspaces/alpha", "kick off bad wake", 15*time.Second) {
		t.Fatalf("initial user message never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// Now wait a reasonable window and assert NO SCHEDULED WAKE landed.
	// The negative delay would have fired ~5s ago if it were honoured.
	time.Sleep(2 * time.Second)
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if e.Kind == "user_message" && strings.Contains(e.Workdir, "workspaces/alpha") &&
			strings.Contains(e.TextPrefix, "SCHEDULED WAKE") {
			t.Errorf("unexpected SCHEDULED WAKE injection with negative delay: %+v", e)
		}
	}
}

// TestL2_Cron_WakeReminderRejectsUnparseableWhen proves the catch-all
// branch in resolveWakeDuration: an unrecognised `when` value (not a
// Go duration, not RFC3339, not a YYYY-MM-DD date, not a known tag)
// fails the tool call. Asserts the tool returns "cannot parse when ..."
// and no DB row or goroutine is created.
func TestL2_Cron_WakeReminderRejectsUnparseableWhen(t *testing.T) {
	const testUserID = 5103

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		ReadyTimeout: 30 * time.Second,
	})

	// "next_friday_at_3pm" matches none of the supported formats.
	bashCmd := `foci_remind --text "what" --when "next_friday_at_3pm" --wake`
	scriptBody, err := json.Marshal(map[string]any{
		"text": "trying unparseable when",
		"tool_uses": []map[string]any{
			{"name": "Bash", "input": map[string]any{"command": bashCmd}},
		},
	})
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "bad when arg",
		},
	})

	if !waitForUserMessage(t, h, "workspaces/alpha", "bad when arg", 15*time.Second) {
		t.Fatalf("initial user message never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// Give any (incorrect) schedule a chance to fire. Then assert nothing did.
	time.Sleep(2 * time.Second)
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if e.Kind == "user_message" && strings.Contains(e.Workdir, "workspaces/alpha") &&
			strings.Contains(e.TextPrefix, "SCHEDULED WAKE") {
			t.Errorf("unexpected SCHEDULED WAKE for unparseable when: %+v", e)
		}
	}

	// The tool should have surfaced a parse error to stderr (cc-stub
	// tees Bash output to stderr). Look for the canonical message body.
	if !strings.Contains(h.Stderr(), "cannot parse when") {
		t.Errorf("expected 'cannot parse when' in foci-gw stderr; tail:\n%s", stderrTail(h.Stderr()))
	}
}

// TestL2_Cron_WakeReminderRoutesToOriginatingSession proves the
// session-key threading in wakeScheduleFn: when the reminder is
// scheduled from agent A's session S1, the fired wake must inject into
// S1 specifically — not into "the most recent session on A" if A has
// since served other sessions. Test creates two sessions on one agent,
// schedules a wake from the older one, drives traffic to the newer
// one, and asserts the wake lands in the older session's recorder
// entries (matched by session_id).
func TestL2_Cron_WakeReminderRoutesToOriginatingSession(t *testing.T) {
	t.Skip("HARNESS GAP: synthesising two distinct sessions on the same agent " +
		"requires sending from two different Telegram chats. The harness ties " +
		"each agent to one UserID, and platform.access.allowed_users = [UserID] " +
		"rejects messages from other chat IDs. Needs HarnessOptions.AgentSpec " +
		"to support multiple allowed users / chat IDs, or a way to drive a " +
		"named session directly (e.g. via the unix socket /send endpoint).")
}

// TestL2_Cron_WakeReminderWaitsForActiveTurn proves the
// IsProcessing() spin in wakeScheduleFn: when the wake delay elapses
// while a user turn is still being processed on the target session,
// the injection waits rather than racing the active turn. Test
// schedules a 1s wake, immediately sends a Telegram message that
// scripts cc-stub to hang for 3s, and asserts the SCHEDULED WAKE
// user_message appears only after the held turn's user_message.
func TestL2_Cron_WakeReminderWaitsForActiveTurn(t *testing.T) {
	const testUserID = 5104
	const reminderText = "MARKER_WAKE_WAITS_FOR_TURN"
	const hangMarker = "HOLD_TURN_OPEN"

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		ReadyTimeout: 30 * time.Second,
	})

	// First turn: schedule a wake with a delay long enough that we can
	// reliably push the hold-turn message and have it enter processing
	// before the wake timer expires. cc-stub's script writes a tool_use
	// that runs foci_remind, then returns immediately. After this the
	// stub file is auto-deleted (one-shot).
	scheduleBash := fmt.Sprintf(`foci_remind --text %q --when 4s --wake`, reminderText)
	scheduleBody, err := json.Marshal(map[string]any{
		"text": "scheduled",
		"tool_uses": []map[string]any{
			{"name": "Bash", "input": map[string]any{"command": scheduleBash}},
		},
	})
	if err != nil {
		t.Fatalf("marshal schedule script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scheduleBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "schedule the wake",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "schedule the wake", 15*time.Second) {
		t.Fatalf("schedule turn never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// Second turn: pin the session by sending a message whose Bash
	// tool_use sleeps 6s. This holds the turn in-flight while the
	// 4s wake delay elapses; wakeScheduleFn's IsProcessing spin should
	// defer the injection until this turn completes. The 6s > 4s gap
	// ensures the wake's timer fires while we're still in-flight, so
	// the IsProcessing spin (2s poll cadence) actually engages.
	hangBash := fmt.Sprintf(`echo %s; sleep 6`, hangMarker)
	hangBody, err := json.Marshal(map[string]any{
		"text": "holding turn",
		"tool_uses": []map[string]any{
			{"name": "Bash", "input": map[string]any{"command": hangBash}},
		},
	})
	if err != nil {
		t.Fatalf("marshal hang script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", hangBody)

	holdTurnText := "hold the turn open"
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: holdTurnText,
		},
	})

	// Both the hold-turn user_message AND the SCHEDULED WAKE should
	// eventually appear, with the hold-turn FIRST (its timestamp is
	// strictly earlier because the wake had to wait for it to finish).
	deadline := time.Now().Add(25 * time.Second)
	var holdTS, wakeTS time.Time
	for time.Now().Before(deadline) {
		holdTS, wakeTS = time.Time{}, time.Time{}
		for _, e := range readRecorderEntries(t, h.RecorderPath()) {
			if e.Kind != "user_message" || !strings.Contains(e.Workdir, "workspaces/alpha") {
				continue
			}
			ts, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
			if strings.Contains(e.TextPrefix, holdTurnText) && holdTS.IsZero() {
				holdTS = ts
			}
			if strings.Contains(e.TextPrefix, "SCHEDULED WAKE") && strings.Contains(e.TextPrefix, reminderText) && wakeTS.IsZero() {
				wakeTS = ts
			}
		}
		if !holdTS.IsZero() && !wakeTS.IsZero() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if holdTS.IsZero() {
		t.Fatalf("hold-turn user_message never recorded\n%s", recorderTail(t, h.RecorderPath()))
	}
	if wakeTS.IsZero() {
		t.Fatalf("SCHEDULED WAKE never recorded\n%s", recorderTail(t, h.RecorderPath()))
	}
	if !wakeTS.After(holdTS) {
		t.Errorf("wake fired (%s) before/equal to hold-turn user_message (%s) — IsProcessing spin not respected",
			wakeTS.Format(time.RFC3339Nano), holdTS.Format(time.RFC3339Nano))
	}
}

// TestL2_Cron_RemindToolUnavailableWithoutWakeFn proves the negative
// path documented in agents_delegated_test.go: when reminderStore is
// nil (reminders disabled), buildWakeScheduler returns nil and the
// remind tool must not be registered. Scripted cc-stub running
// `foci_remind ...` should see an "unknown tool" / missing exec
// bridge function rather than a successful schedule. Asserts the
// recorder shows no SCHEDULED WAKE injection regardless of how long
// the test waits.
func TestL2_Cron_RemindToolUnavailableWithoutWakeFn(t *testing.T) {
	t.Skip("HARNESS GAP: writeTestConfig + initStandaloneStores always create " +
		"a per-agent ReminderStore. There's no test surface to disable it. " +
		"Needs harness support to suppress reminder store creation (e.g. " +
		"HarnessOptions.DisableReminders) or set the workspace .data dir to a " +
		"read-only location to force the store-init path to fail cleanly.")
}

// TestL2_Cron_GenerateCrontabRendersAgentPlaceholders proves the
// crontab generator path that foci-install runs: with the agent spec
// + shared/crontab.template, GenerateCrontab returns one line per
// non-comment template entry with AGENT_NAME / HOMEDIR / WORKSPACE
// substituted. L2 drives this via the foci CLI subcommand (no real
// crontab is touched — RunCrontabCmd is overridden in tests).
// Asserts the rendered lines contain the agent id and the agent's
// resolved workspace path.
func TestL2_Cron_GenerateCrontabRendersAgentPlaceholders(t *testing.T) {
	tmpDir := t.TempDir()
	templatePath := filepath.Join(tmpDir, "crontab.template")
	template := `# header comment, must be stripped
40 2 * * 1 foci branch --oneshot -a AGENT_NAME -mf HOMEDIR/shared/prompts/x.md 2>&1 >> HOMEDIR/logs/cron.log
*/30 * * * * foci send -a AGENT_NAME --workspace WORKSPACE "[ping]"
`
	if err := os.WriteFile(templatePath, []byte(template), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	spec := provision.AgentSpec{
		ID:      "ruslan",
		HomeDir: "/home/foci",
	}
	lines, err := provision.GenerateCrontab(templatePath, spec, 0)
	if err != nil {
		t.Fatalf("GenerateCrontab: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 non-comment lines, got %d: %v", len(lines), lines)
	}

	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "AGENT_NAME") || strings.Contains(joined, "HOMEDIR") || strings.Contains(joined, "WORKSPACE") {
		t.Errorf("placeholders not all replaced:\n%s", joined)
	}
	for _, want := range []string{"ruslan", "/home/foci", "/home/foci/ruslan"} {
		if !strings.Contains(joined, want) {
			t.Errorf("rendered crontab missing %q:\n%s", want, joined)
		}
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "#") {
			t.Errorf("comment leaked into output: %q", l)
		}
	}
}

// TestL2_Cron_GenerateCrontabStaggersMultipleAgents proves
// StaggerCrontabLine: when GenerateCrontab is called with
// existingAgentCount>0, the absolute minute fields are offset by
// existingAgentCount*3 (mod 60) while */N interval fields are left
// alone. Test generates entries for three agents and asserts the
// minute fields differ by the stagger offset.
func TestL2_Cron_GenerateCrontabStaggersMultipleAgents(t *testing.T) {
	tmpDir := t.TempDir()
	templatePath := filepath.Join(tmpDir, "crontab.template")
	template := `0 4 * * * foci branch --oneshot -a AGENT_NAME -mf HOMEDIR/p.md
*/30 * * * * foci send -a AGENT_NAME "[ping]"
15 * * * * foci branch --oneshot -a AGENT_NAME -mf HOMEDIR/q.md
`
	if err := os.WriteFile(templatePath, []byte(template), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	// Three agents: existingAgentCount values 0, 1, 2 (offsets 0, 3, 6).
	minuteFields := func(line string) string {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return ""
		}
		return fields[0]
	}

	// Agent 0 (offset 0): first line minute "0", second "*/30", third "15".
	agent0, err := provision.GenerateCrontab(templatePath, provision.AgentSpec{ID: "alpha", HomeDir: "/h"}, 0)
	if err != nil {
		t.Fatalf("GenerateCrontab 0: %v", err)
	}
	if got := minuteFields(agent0[0]); got != "0" {
		t.Errorf("agent0 line0 minute = %q, want 0", got)
	}
	if got := minuteFields(agent0[1]); got != "*/30" {
		t.Errorf("agent0 line1 minute = %q, want */30", got)
	}
	if got := minuteFields(agent0[2]); got != "15" {
		t.Errorf("agent0 line2 minute = %q, want 15", got)
	}

	// Agent 1 (offset 3): absolutes shift, */30 unchanged.
	agent1, err := provision.GenerateCrontab(templatePath, provision.AgentSpec{ID: "beta", HomeDir: "/h"}, 1)
	if err != nil {
		t.Fatalf("GenerateCrontab 1: %v", err)
	}
	if got := minuteFields(agent1[0]); got != "3" {
		t.Errorf("agent1 line0 minute = %q, want 3", got)
	}
	if got := minuteFields(agent1[1]); got != "*/30" {
		t.Errorf("agent1 line1 minute = %q, want */30 (interval untouched)", got)
	}
	if got := minuteFields(agent1[2]); got != "18" {
		t.Errorf("agent1 line2 minute = %q, want 18", got)
	}

	// Agent 2 (offset 6): another shift.
	agent2, err := provision.GenerateCrontab(templatePath, provision.AgentSpec{ID: "gamma", HomeDir: "/h"}, 2)
	if err != nil {
		t.Fatalf("GenerateCrontab 2: %v", err)
	}
	if got := minuteFields(agent2[0]); got != "6" {
		t.Errorf("agent2 line0 minute = %q, want 6", got)
	}
	if got := minuteFields(agent2[2]); got != "21" {
		t.Errorf("agent2 line2 minute = %q, want 21", got)
	}
}

// TestL2_Cron_BranchOneshotEndToEnd proves the external-cron contract
// rendered into the crontab template: `foci branch --oneshot -a <agent>
// -mf <prompt-file>` reaches foci-gw via the local unix socket, the
// gateway dispatches a branch session on the agent, and cc-stub
// records an invocation with the prompt body. This is the L2 hook
// for any system-cron-driven workflow (the weekly character review
// in the template, future scheduled jobs).
func TestL2_Cron_BranchOneshotEndToEnd(t *testing.T) {
	const testUserID = 5201
	const promptBody = "MARKER_BRANCH_ONESHOT_PROMPT"

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		ReadyTimeout: 30 * time.Second,
	})

	// Prime alpha so the gateway has an active session to branch from.
	// /wake errors with 412 "no active session" otherwise.
	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "priming alpha",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "priming alpha", 15*time.Second) {
		t.Fatalf("priming message never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// `foci branch --oneshot -a alpha -mf <file>` ultimately POSTs to
	// /wake with {agent: "alpha", text: <file contents>, no_compact:true,
	// no_reset_hook: true, silent: true, async: true}. We drive the HTTP
	// side directly via the unix socket — this exercises the same gateway
	// path the CLI takes, which is the L2 unit under test.
	sockPath := gwSocketPath(h)
	waitForSocket(t, sockPath, 5*time.Second)

	body := map[string]any{
		"agent":         "alpha",
		"text":          promptBody,
		"no_compact":    true,
		"no_reset_hook": true,
		"silent":        true,
		"async":         true,
	}
	bodyBytes, _ := json.Marshal(body)
	client := gwUnixClient(sockPath)
	resp, err := client.Post("http://foci-gw/wake", "application/json", strings.NewReader(string(bodyBytes)))
	if err != nil {
		t.Fatalf("POST /wake: %v", err)
	}
	defer resp.Body.Close()
	// Async dispatch returns 202 Accepted; sync would be 200 OK. Either
	// indicates the gateway accepted the work — failure modes are 4xx/5xx.
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /wake: status %d", resp.StatusCode)
	}

	if !waitForUserMessage(t, h, "workspaces/alpha", promptBody, 20*time.Second) {
		t.Fatalf("branch --oneshot prompt never reached cc-stub\n%s\n%s",
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
}

// TestL2_Cron_BranchOneshotRejectsUnknownAgent proves the gateway
// rejects a `foci branch --oneshot -a <missing>` call with a clear
// error rather than silently dropping it. Asserts the CLI exits
// non-zero with a message naming the missing agent, and no recorder
// invocation is created.
func TestL2_Cron_BranchOneshotRejectsUnknownAgent(t *testing.T) {
	const testUserID = 5202

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		ReadyTimeout: 30 * time.Second,
	})

	// Snapshot recorder entry count before the bad call so we can
	// assert nothing new lands.
	before := len(readRecorderEntries(t, h.RecorderPath()))

	sockPath := gwSocketPath(h)
	waitForSocket(t, sockPath, 5*time.Second)
	client := gwUnixClient(sockPath)

	body := map[string]any{
		"agent":         "nonexistent-agent",
		"text":          "should be rejected",
		"no_compact":    true,
		"no_reset_hook": true,
		"silent":        true,
		"async":         true,
	}
	bodyBytes, _ := json.Marshal(body)
	resp, err := client.Post("http://foci-gw/wake", "application/json", strings.NewReader(string(bodyBytes)))
	if err != nil {
		t.Fatalf("POST /wake: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /wake for missing agent: status %d, want 400", resp.StatusCode)
	}
	respBytes := make([]byte, 4096)
	n, _ := resp.Body.Read(respBytes)
	respText := string(respBytes[:n])
	if !strings.Contains(respText, "nonexistent-agent") {
		t.Errorf("error body did not name the missing agent: %q", respText)
	}

	// Give the gateway time to NOT do work.
	time.Sleep(2 * time.Second)
	after := len(readRecorderEntries(t, h.RecorderPath()))
	if after != before {
		t.Errorf("rejected /wake still produced recorder entries (before=%d after=%d)", before, after)
	}
}

// TestL2_Cron_BranchOneshotMalformedPromptFile proves the CLI surfaces
// a file-not-found / unreadable error when -mf points at a missing
// path, rather than dispatching with an empty prompt. Asserts the CLI
// exits non-zero with a path-related error and the recorder shows no
// branch invocation.
func TestL2_Cron_BranchOneshotMalformedPromptFile(t *testing.T) {
	t.Skip("HARNESS GAP: this is a CLI-side check (cmd/foci's cmdBranch reads " +
		"the file before issuing the HTTP request). The harness builds " +
		"foci-gw + cc-stub but not the foci CLI binary, and there's no public " +
		"helper to do so. Could be implemented by adding a Harness.BuildFociCLI " +
		"method (or by running 'go run ./cmd/foci' from the repo root in the " +
		"test), but neither path uses the existing harness surface.")
}

// TestL2_Cron_RateLimitGateBlocksAllSchedulers proves the shared
// canFireFn / sessionKeyFn check at the top of every scheduler
// (background, reflection, consolidation): when the rate-limit/mana
// callback returns canFire=false with a reason, none of the three
// schedulers dispatch. Test wires a stub canFireFn returning false
// and confirms zero invocations across multiple ticks despite all
// other conditions being met.
func TestL2_Cron_RateLimitGateBlocksAllSchedulers(t *testing.T) {
	t.Skip("HARNESS GAP: CanFireFunc is wired internally in foci-gw's " +
		"periodic_setup.go from Agent.CanFireBackgroundOperation — there's no " +
		"test surface to inject a stub. Needs harness support to override " +
		"the callback OR a way to force mana exhaustion / rate-limit state " +
		"from the test side. Compounding gap: also requires [background], " +
		"[reflection] config injection to enable the schedulers at all.")
}

// TestL2_Cron_IncomingMessageDuringKeepaliveQueues proves the
// interaction between an ingress Telegram message and an in-flight
// keepalive branch: the user's message should not be dropped, and
// must process as a normal turn once the keepalive branch completes.
// This is the cron-side companion to the message-queueing behaviour
// documented in MEMORY for permission waits. Asserts both the
// keepalive recorder entry AND the user-message recorder entry are
// present, in that order.
func TestL2_Cron_IncomingMessageDuringKeepaliveQueues(t *testing.T) {
	t.Skip("HARNESS GAP: requires [keepalive] config injection. " +
		"See TestL2_Cron_KeepaliveFiresAtConfiguredInterval.")
}
