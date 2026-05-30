//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
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
	t.Parallel()
	const testUserID = 5301

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		ExtraConfigTOML: "\n[keepalive]\nenabled = true\ninterval = \"1s\"\n",
		ReadyTimeout:    30 * time.Second,
	})

	// Keepalive requires a default parent session before it can fire
	// (defaultParentKey returns "" otherwise). Push one Telegram message
	// to create that session and consume the initial cc-stub script.
	scriptBody, err := json.Marshal(map[string]any{
		"text": "bootstrap",
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
			Text: "bootstrap session",
		},
	})

	if !waitForUserMessage(t, h, "workspaces/alpha", "bootstrap session", 15*time.Second) {
		t.Fatalf("initial user message never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// Settle the bootstrap turn before counting.
	time.Sleep(500 * time.Millisecond)

	// The keepalive scheduler ticks every 30s (internal/periodic
	// tickInterval). With interval=1s, the cache-age threshold is already
	// crossed; the next tick after the bootstrap turn should fire
	// keepalive. For the delegated (claude-code) backend keepalive does
	// NOT spawn a new cc-stub process — it injects the keepalive prompt
	// into the running main session as a user_message. So we assert on
	// user_message TextPrefix carrying the keepalive prompt marker.
	time.Sleep(35 * time.Second)

	found := false
	for _, e := range userMessagesForWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha") {
		if strings.Contains(e.TextPrefix, "[KEEPALIVE]") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("keepalive prompt never reached cc-stub as user_message; stderr:\n%s",
			stderrTail(h.Stderr()))
	}
}

// TestL2_Cron_KeepaliveSkippedWhenCachingUnavailable proves the
// keepalive guard at the top of maybeKeepalive: if the provider client
// reports caching is unavailable (or the explicit cachingOverride is
// false), no branch is dispatched even after the interval elapses. The
// stub backend exposes no real caching, so configuring keepalive
// without a caching-capable model should leave the recorder free of
// keepalive invocations across multiple ticks.
func TestL2_Cron_KeepaliveSkippedWhenCachingUnavailable(t *testing.T) {
	t.Parallel()
	// SCOPE: API-backend behaviour. The caching gate evaluates
	// r.cachingOverride or r.client.IsCachingAvailable() at runtime
	// (internal/periodic/keepalive.go:294-301). For L2's claude-code
	// (delegated) backend the provider client isn't the path that
	// reflects real caching capability — the test would be asserting
	// against a stub, not the real gate. The interesting behaviour lives
	// on the direct-API path (claude-anthropic backend) where
	// provider.Client.IsCachingAvailable() is a real signal.
	//
	// Not covered for delegated backends. Unblock by either:
	//   - adding a non-delegated L2 mode (harness backend selection), or
	//   - covering the gate logic with a unit test against the Runner
	//     in internal/periodic.
	t.Skip("SCOPE: keepalive's caching gate is API-backend behaviour; L2 uses the claude-code (delegated) backend. Not tested for delegated backends. See comment above for unblock paths.")
}

// TestL2_Cron_KeepaliveSkippedWhenTurnInFlight proves the in-flight
// guard added by TODO #760: if a user turn is mid-flight on the parent
// session when the keepalive tick fires, the runner defers rather than
// queueing the keepalive prompt as a SourceUser follow-up. The test
// holds a turn open (cc-stub script with a long-running Bash tool_use)
// while the keepalive interval elapses, then verifies no keepalive
// invocation was recorded until after the held turn completes.
func TestL2_Cron_KeepaliveSkippedWhenTurnInFlight(t *testing.T) {
	t.Parallel()
	const testUserID = 5302

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		ExtraConfigTOML: "\n[keepalive]\nenabled = true\ninterval = \"1s\"\n",
		ReadyTimeout:    30 * time.Second,
	})

	// Bootstrap a parent session so keepalive has a default to fire on.
	scriptBody, err := json.Marshal(map[string]any{"text": "bootstrap"})
	if err != nil {
		t.Fatalf("marshal bootstrap script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "bootstrap session",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "bootstrap session", 15*time.Second) {
		t.Fatalf("bootstrap user message never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// Hold the parent session in-flight across the T+30s keepalive tick.
	// cc-stub caps each Bash tool_use at 10s (runBashToolUse wall-clock
	// guard), but multiple tool_uses each get a fresh budget. Chain four
	// `sleep 9` calls for ~36s of in-flight wall time — long enough to
	// span the T+30s tick comfortably while leaving room for the post-
	// hang T+60s tick to fire keepalive within the observation window.
	hangSleep := map[string]any{"name": "Bash", "input": map[string]any{"command": "sleep 9"}}
	hangBody, err := json.Marshal(map[string]any{
		"text": "holding",
		"tool_uses": []map[string]any{
			hangSleep, hangSleep, hangSleep, hangSleep,
		},
	})
	if err != nil {
		t.Fatalf("marshal hang script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", hangBody)

	const holdText = "hold turn for keepalive test"
	holdPushTime := time.Now()
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: holdText,
		},
	})

	// Wait long enough to cover (a) the in-flight window (T+30s tick must
	// defer) and (b) at least one tick AFTER the hang completes (T+60s tick
	// should fire). The hang runs ~40s, so the post-hang tick lands around
	// T+60s. Sleeping 65s gives us a comfortable observation window.
	time.Sleep(65 * time.Second)

	// Locate the hold user_message timestamp and any [KEEPALIVE] injection.
	var holdTS, keepaliveTS time.Time
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if e.Kind != "user_message" || !strings.Contains(e.Workdir, "workspaces/alpha") {
			continue
		}
		ts, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
		if strings.Contains(e.TextPrefix, holdText) && holdTS.IsZero() {
			holdTS = ts
		}
		if strings.Contains(e.TextPrefix, "[KEEPALIVE]") && keepaliveTS.IsZero() {
			keepaliveTS = ts
		}
	}
	if holdTS.IsZero() {
		t.Fatalf("hold-turn user_message never recorded\n%s", recorderTail(t, h.RecorderPath()))
	}
	if keepaliveTS.IsZero() {
		t.Fatalf("keepalive never fired post-hang within 65s observation window\n%s",
			recorderTail(t, h.RecorderPath()))
	}
	// The in-flight guard must have deferred keepalive past the hang. Hang
	// length is ~40s; keepalive must therefore appear at least ~35s after
	// the hold message was pushed. If the gap is much smaller the guard
	// fired during the hang (or didn't engage).
	gap := keepaliveTS.Sub(holdPushTime)
	if gap < 35*time.Second {
		t.Errorf("keepalive fired %s after hold push — in-flight guard did not defer past hang",
			gap.Round(time.Second))
	}
}

// TestL2_Cron_KeepaliveDoesNotReplyToTelegram proves the keepalive
// prompt "[KEEPALIVE] ... respond with [[NO_RESPONSE]]" is treated as
// internal: even though cc-stub by default echoes the user text, the
// runner branches with noCompact=true into a fresh session and the
// branch egress path must not produce a sendMessage to the Telegram
// stub. The assertion is the absence of any user-visible message for
// the keepalive prompt body in the Telegram stub's call log.
func TestL2_Cron_KeepaliveDoesNotReplyToTelegram(t *testing.T) {
	t.Parallel()
	const testUserID = 5303

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		ExtraConfigTOML: "\n[keepalive]\nenabled = true\ninterval = \"1s\"\n",
		ReadyTimeout:    30 * time.Second,
	})

	scriptBody, err := json.Marshal(map[string]any{"text": "bootstrap"})
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "bootstrap session",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "bootstrap session", 15*time.Second) {
		t.Fatalf("bootstrap user message never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}
	time.Sleep(500 * time.Millisecond)

	// Drain anything Telegram received from the bootstrap turn so the post-
	// keepalive scan only sees sends emitted after the tick (if any).
	_ = h.TelegramStub().DrainSent(token)

	// One full 30s tickInterval + buffer. The proven KeepaliveFires test
	// shows the marker appears in the recorder's user_message stream within
	// 35s for interval=1s.
	time.Sleep(35 * time.Second)

	// First sanity-check: the keepalive prompt DID reach cc-stub as a
	// user_message (test would otherwise be a tautology).
	sawKeepalive := false
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if e.Kind != "user_message" || !strings.Contains(e.Workdir, "workspaces/alpha") {
			continue
		}
		if strings.Contains(e.TextPrefix, "[KEEPALIVE]") {
			sawKeepalive = true
			break
		}
	}
	if !sawKeepalive {
		t.Fatalf("keepalive prompt never reached cc-stub as user_message — cannot meaningfully assert Telegram silence\n%s",
			recorderTail(t, h.RecorderPath()))
	}

	// Real assertion: no payload sent to Telegram carries the keepalive
	// prompt text. If the branch/inject egress path were misrouted, the
	// cc-stub reply (default "stub-reply: <user prompt>") would echo the
	// [KEEPALIVE] body to the chat — visible in the stub's sent buffer.
	for _, call := range h.TelegramStub().PeekSent(token) {
		body := string(call.Body)
		if strings.Contains(body, "[KEEPALIVE]") || strings.Contains(body, "Cache keepalive ping") {
			t.Errorf("keepalive leaked to Telegram via %s: %s", call.Method, body)
		}
	}
}

// TestL2_Cron_BackgroundFiresWhenIdleWithOpenTodos proves the
// background-work scheduler in internal/periodic fires when (a) the
// configured idle interval has elapsed since last interaction, (b)
// there's at least one open todo tagged "background", and (c) mana /
// rate-limit gating allows. The test seeds a background todo via the
// todo store path, leaves the agent idle past the interval, and
// asserts a recorder entry with the background prompt appears.
func TestL2_Cron_BackgroundFiresWhenIdleWithOpenTodos(t *testing.T) {
	t.Parallel()
	const testUserID = 5305

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		// Background enabled at 1s interval so the idle gate passes within
		// the test window. Disable reflection's interval + consolidation so
		// they don't add noise to the user_message stream we're scanning.
		ExtraConfigTOML: "\n[background]\nenabled = true\ninterval = \"1s\"\n\n[reflection]\ninterval_enabled = false\nconsolidation_enabled = false\n",
		ReadyTimeout:    30 * time.Second,
	})

	// Bootstrap turn seeds a background-tagged todo via the exec bridge.
	// The harness has no SeedTodo helper, but foci_todo is exported to
	// cc-stub's Bash tool_uses through the funcs.sh shell wiring — the
	// add lands in the agent's todo store, just like a real agent run.
	seedBash := `foci_todo add --text "background test todo" --tag background`
	scriptBody, err := json.Marshal(map[string]any{
		"text": "bootstrap with seed",
		"tool_uses": []map[string]any{
			{"name": "Bash", "input": map[string]any{"command": seedBash}},
		},
	})
	if err != nil {
		t.Fatalf("marshal bootstrap script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "bootstrap session",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "bootstrap session", 15*time.Second) {
		t.Fatalf("bootstrap user message never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// Give the bash tool_use time to land the todo add before the first
	// scheduler tick. Then sleep one tickInterval + interval + buffer so
	// the T+30s tick sees idle > interval and a non-zero open todo count.
	time.Sleep(1 * time.Second)
	time.Sleep(35 * time.Second)

	// In delegated mode, background work injects into the main session as
	// a user_message — same shape as keepalive. Marker is the first line
	// of background.md: "[background] # Background Work".
	found := false
	for _, e := range userMessagesForWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha") {
		if strings.Contains(e.TextPrefix, "[background]") && strings.Contains(e.TextPrefix, "Background Work") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("background prompt never reached cc-stub as user_message after seeding a background todo; stderr:\n%s",
			stderrTail(h.Stderr()))
	}
}

// TestL2_Cron_BackgroundSkippedWithNoOpenTodos proves the explicit
// "no open background todos" skip branch in maybeBackgroundWork: even
// when idle past the interval, with no qualifying todos the scheduler
// must not dispatch. Asserts the recorder shows no background-prompt
// invocation across multiple ticks when the todo store is empty.
func TestL2_Cron_BackgroundSkippedWithNoOpenTodos(t *testing.T) {
	t.Parallel()
	const testUserID = 5304

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		// Background enabled with a sub-minute interval so the idle gate
		// passes after the bootstrap turn settles. Background should
		// still skip because the per-agent todo store is empty. We also
		// disable reflection's interval + consolidation timers so they
		// don't contribute recorder growth unrelated to this assertion.
		ExtraConfigTOML: "\n[background]\nenabled = true\ninterval = \"1s\"\n\n[reflection]\ninterval_enabled = false\nconsolidation_enabled = false\n",
		ReadyTimeout:    30 * time.Second,
	})

	scriptBody, err := json.Marshal(map[string]any{"text": "bootstrap"})
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "bootstrap session",
		},
	})

	if !waitForUserMessage(t, h, "workspaces/alpha", "bootstrap session", 15*time.Second) {
		t.Fatalf("bootstrap user message never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	time.Sleep(500 * time.Millisecond)
	before := len(invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha"))

	// One full 30s tick + buffer. With empty todo store the scheduler
	// must not dispatch regardless of idle/interval state.
	time.Sleep(35 * time.Second)

	after := len(invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha"))
	if after != before {
		t.Errorf("background fired with no open todos: recorder entries before=%d after=%d; stderr:\n%s",
			before, after, stderrTail(h.Stderr()))
	}
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
	t.Parallel()
	const testUserID = 5306

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		// interval=30s shapes the test window:
		//   t≈+30s tick — idle 29s < 30s → idle gate blocks (no fire)
		//   t≈+60s tick — idle 59s > 30s → fires; ends ~T+61s
		//   t≈+90s tick — sinceLastBgEnd ~29s < 30s → cooldown blocks
		// Total expected [background] fires across the run: exactly 1.
		// Without the cooldown the T+90s tick would fire again because
		// idle is also ≥ interval.
		ExtraConfigTOML: "\n[background]\nenabled = true\ninterval = \"30s\"\n\n[reflection]\ninterval_enabled = false\nconsolidation_enabled = false\n",
		ReadyTimeout:    30 * time.Second,
	})

	// Seed two background todos so the bg session has work even after
	// notionally completing one (cc-stub doesn't actually close todos —
	// the queue stays open — but seeding two makes the intent explicit).
	seedBash := `foci_todo add --text "bg todo A" --tag background && foci_todo add --text "bg todo B" --tag background`
	scriptBody, err := json.Marshal(map[string]any{
		"text": "bootstrap with seed",
		"tool_uses": []map[string]any{
			{"name": "Bash", "input": map[string]any{"command": seedBash}},
		},
	})
	if err != nil {
		t.Fatalf("marshal bootstrap script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "bootstrap session",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "bootstrap session", 15*time.Second) {
		t.Fatalf("bootstrap user message never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// Run past the third tick (~T+95s) so we observe both the first fire
	// (T+60s tick) and the cooldown-blocked third tick (T+90s).
	time.Sleep(95 * time.Second)

	// Count background prompt injections in the recorder. Cooldown must
	// prevent the third tick from firing despite idle and open todos.
	count := 0
	for _, e := range userMessagesForWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha") {
		if strings.Contains(e.TextPrefix, "[background]") && strings.Contains(e.TextPrefix, "Background Work") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 background fire across 95s with cooldown=30s; got %d\n%s",
			count, recorderTail(t, h.RecorderPath()))
	}
}

// TestL2_Cron_BackgroundSkippedWhileActiveTmuxWatches proves the
// hasActiveWorkFn gate: when the callback returns >0, the background
// scheduler defers regardless of idle time or open todos.
//
// Driver: the harness's testharness control socket pins the agent's
// HasActiveWorkFn return value via h.SetActiveWork. Production
// inst.tmuxWatchCount is nil for delegated agents — the override is
// the only path that exercises this gate from L2.
func TestL2_Cron_BackgroundSkippedWhileActiveTmuxWatches(t *testing.T) {
	t.Parallel()
	const testUserID = 5306

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		// Background enabled at 1s interval so the idle gate would
		// otherwise pass within the test window — we're proving the
		// HasActiveWork gate alone blocks dispatch.
		ExtraConfigTOML: "\n[background]\nenabled = true\ninterval = \"1s\"\n\n[reflection]\ninterval_enabled = false\nconsolidation_enabled = false\n",
		ReadyTimeout:    30 * time.Second,
	})

	// Pin HasActiveWorkFn to 1 BEFORE any tick can fire. The override
	// survives across all subsequent ticks until cleared.
	if err := h.SetActiveWork("alpha", 1); err != nil {
		t.Fatalf("SetActiveWork: %v", err)
	}

	// Seed a qualifying background todo so without the gate this test
	// would fire (matches TestL2_Cron_BackgroundFiresWhenIdleWithOpenTodos).
	seedBash := `foci_todo add --text "background test todo" --tag background`
	scriptBody, err := json.Marshal(map[string]any{
		"text": "bootstrap with seed",
		"tool_uses": []map[string]any{
			{"name": "Bash", "input": map[string]any{"command": seedBash}},
		},
	})
	if err != nil {
		t.Fatalf("marshal bootstrap script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "bootstrap session",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "bootstrap session", 15*time.Second) {
		t.Fatalf("bootstrap user message never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// One full 30s tick + buffer. With HasActiveWork pinned to 1 the
	// background scheduler must not dispatch despite an open background
	// todo and the idle gate having passed.
	time.Sleep(35 * time.Second)

	for _, e := range userMessagesForWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha") {
		if strings.Contains(e.TextPrefix, "[background]") && strings.Contains(e.TextPrefix, "Background Work") {
			t.Fatalf("background fired despite HasActiveWork override pinned to 1\nmatching entry: %q\nstderr:\n%s", e.TextPrefix, stderrTail(h.Stderr()))
		}
	}
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
	t.Parallel()
	const testUserID = 5302

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		// interval=20s passes the lastReflection time-gate after one 30s
		// tick AND keeps the idle-vs-interval window achievable. Reflection
		// SKIPS when sinceLastInteraction > interval (dormant sessions);
		// the test sends a refresh message before the tick so idle stays
		// low. consolidation_enabled=false isolates the assertion.
		ExtraConfigTOML: "\n[reflection]\ninterval = \"20s\"\nbackend_quiet_period = \"1s\"\nconsolidation_enabled = false\n",
		ReadyTimeout:    30 * time.Second,
	})

	// Bootstrap so SessionIndex has a session to consider.
	scriptBody, err := json.Marshal(map[string]any{"text": "bootstrap"})
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "bootstrap session",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "bootstrap session", 15*time.Second) {
		t.Fatalf("bootstrap never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// Wait, then push a refresh message so lastInteraction is recent
	// when the 30s reflection tick lands. Without this the bootstrap-
	// at-T+~3s becomes idle ~27s by tick time, which exceeds the
	// 20s interval and reflection skips ("idle > interval").
	time.Sleep(20 * time.Second)
	refreshBody, _ := json.Marshal(map[string]any{"text": "refresh"})
	h.WriteCCStubScript(t, "alpha", refreshBody)
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "keep session warm",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "keep session warm", 15*time.Second) {
		t.Fatalf("refresh never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// Wait through the next 30s tick. With the refresh ~22s in, the
	// next tick lands within a window where sinceLastInteraction < 20s
	// interval AND lastReflection-time-gate has elapsed.
	time.Sleep(20 * time.Second)

	found := false
	for _, e := range userMessagesForWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha") {
		if strings.Contains(e.TextPrefix, "Reflection Pass") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("reflection prompt never reached cc-stub as user_message; stderr:\n%s",
			stderrTail(h.Stderr()))
	}
}

// TestL2_Cron_ReflectionStampPreventsImmediateRefire proves the
// SessionIndex.StampReflection write inside maybeReflection: once a
// session has been reflected on, subsequent ticks within the configured
// interval skip it (the "no sessions need reflection" path). Asserts at
// most one reflection invocation per session across a window of
// multiple ticks.
func TestL2_Cron_ReflectionStampPreventsImmediateRefire(t *testing.T) {
	t.Parallel()
	// SKIP: structurally infeasible with the current implementation +
	// 30s tickInterval. To observe "fires then skips" we need both:
	//   - first 30s tick must fire (so interval ≤ 30s)
	//   - second 30s tick must skip via the lastReflection time-gate
	//     (so now < lastReflection + interval, i.e. interval > tick gap)
	// Those two are unsatisfiable with a single interval value.
	//
	// The other potential guard — SessionIndex's "needs reflection" stamp
	// — also can't isolate, because any user-message refresh used to keep
	// the idle gate satisfied ALSO restamps the session as needing
	// reflection on the next tick. Catch-22 with the warm-session
	// requirement.
	//
	// Unblock paths: (a) make tickInterval configurable so a test can
	// fit interval > tick gap, (b) expose a SessionIndex.MarkReflected
	// helper for the harness so the test can stamp without a user msg.
	t.Skip("INFEASIBLE: cannot observe 'fires then skips' isolated to the stamp with the current 30s tickInterval and idle-gate constraints. See comment for unblock paths.")
	const testUserID = 5305

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		// interval=20s — chosen so:
		//   - lastReflection time-gate passes after one 30s tick
		//   - sinceLastInteraction can stay < interval across two refreshes
		// consolidation_enabled=false isolates the assertion to reflection.
		ExtraConfigTOML: "\n[reflection]\ninterval = \"20s\"\nbackend_quiet_period = \"1s\"\nconsolidation_enabled = false\n",
		ReadyTimeout:    30 * time.Second,
	})

	scriptBody, err := json.Marshal(map[string]any{"text": "bootstrap"})
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "bootstrap session",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "bootstrap session", 15*time.Second) {
		t.Fatalf("bootstrap never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	countReflectionInjects := func() int {
		n := 0
		for _, e := range userMessagesForWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha") {
			if strings.Contains(e.TextPrefix, "Reflection Pass") {
				n++
			}
		}
		return n
	}

	// Push a refresh ~20s in to keep lastInteraction recent for the
	// next 30s tick — see TestL2_Cron_ReflectionFiresOnInterval for the
	// idle-vs-interval gate reasoning.
	pushRefresh := func(text string) {
		body, _ := json.Marshal(map[string]any{"text": "refresh"})
		h.WriteCCStubScript(t, "alpha", body)
		h.TelegramStub().PushUpdate(token, gotgbot.Update{
			Message: &gotgbot.Message{
				Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
				From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
				Text: text,
			},
		})
		if !waitForUserMessage(t, h, "workspaces/alpha", text, 15*time.Second) {
			t.Fatalf("refresh %q never processed; stderr:\n%s", text, stderrTail(h.Stderr()))
		}
	}

	// First reflection cycle: warm session, wait through first 30s tick,
	// assert reflection fired exactly once.
	time.Sleep(20 * time.Second)
	pushRefresh("warm-1")
	time.Sleep(20 * time.Second)
	afterFirstTick := countReflectionInjects()
	if afterFirstTick == 0 {
		t.Fatalf("reflection did not fire on first tick (no 'Reflection Pass' user_message recorded); stderr:\n%s",
			stderrTail(h.Stderr()))
	}

	// Second reflection cycle: refresh again to keep idle gate satisfied,
	// then wait through the next 30s tick. The session was just stamped
	// by the first reflection, so SessionIndex should report "no sessions
	// need reflection" and the count must not grow. With interval=20s
	// the time-gate alone would not block (20s elapsed since first fire),
	// so the stamp is the only remaining guard.
	pushRefresh("warm-2")
	time.Sleep(30 * time.Second)
	afterSecondTick := countReflectionInjects()
	if afterSecondTick != afterFirstTick {
		t.Errorf("reflection refired despite recent stamp: afterFirst=%d afterSecond=%d; stderr:\n%s",
			afterFirstTick, afterSecondTick, stderrTail(h.Stderr()))
	}
}

// TestL2_Cron_ReflectionDeferredWhenAllSessionsBusy proves the
// in-flight filter in maybeReflection (TODO #760): when every session
// flagged as needing reflection has an active turn, the reflection
// pass logs "all candidate sessions have in-flight turns" and skips.
// Test holds a turn open via cc-stub and asserts no reflection
// recorder entry appears for that session until the turn completes.
func TestL2_Cron_ReflectionDeferredWhenAllSessionsBusy(t *testing.T) {
	t.Parallel()
	const testUserID = 5306
	const hangMarker = "REFL_DEFER_HOLD"

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		// interval=1s so the gate doesn't block; the in-flight filter is
		// the only thing that should stop reflection. Disable
		// consolidation so its timer doesn't muddy the count.
		ExtraConfigTOML: "\n[reflection]\ninterval = \"1s\"\nbackend_quiet_period = \"1s\"\nconsolidation_enabled = false\n",
		ReadyTimeout:    30 * time.Second,
	})

	// Bootstrap so the agent has a session at all.
	bootstrapBody, err := json.Marshal(map[string]any{"text": "bootstrap"})
	if err != nil {
		t.Fatalf("marshal bootstrap: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", bootstrapBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "bootstrap session",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "bootstrap session", 15*time.Second) {
		t.Fatalf("bootstrap never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// Second turn: hold the session in-flight for 35s so the first 30s
	// tick lands while parent is still processing. Bash sleep is the
	// canonical hold pattern (see TestL2_Cron_WakeReminderWaitsForActiveTurn).
	hangBash := fmt.Sprintf(`echo %s; sleep 35`, hangMarker)
	hangBody, err := json.Marshal(map[string]any{
		"text": "holding",
		"tool_uses": []map[string]any{
			{"name": "Bash", "input": map[string]any{"command": hangBash}},
		},
	})
	if err != nil {
		t.Fatalf("marshal hang: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", hangBody)

	holdText := "hold the turn open"
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: holdText,
		},
	})

	// Wait for the hang's user_message to be recorded (proves the turn
	// started). The hang's bash will run for 35s.
	if !waitForUserMessage(t, h, "workspaces/alpha", holdText, 15*time.Second) {
		t.Fatalf("hang turn never started; stderr:\n%s", stderrTail(h.Stderr()))
	}

	beforeTick := len(invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha"))

	// Wait through the 30s tick (which lands during the 35s hold). The
	// in-flight filter should defer reflection.
	time.Sleep(33 * time.Second)
	afterTick := len(invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha"))
	if afterTick != beforeTick {
		t.Errorf("reflection fired while parent session was in-flight: before=%d after=%d; stderr:\n%s",
			beforeTick, afterTick, stderrTail(h.Stderr()))
	}
}

// TestL2_Cron_ReflectionDisabledWhenIntervalEnabledFalse proves the
// IntervalEnabled=false config path: even with eligible sessions and a
// short interval, the scheduler returns immediately without
// dispatching. Asserts the recorder shows zero reflection invocations.
func TestL2_Cron_ReflectionDisabledWhenIntervalEnabledFalse(t *testing.T) {
	t.Parallel()
	const testUserID = 5303

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		// interval_enabled=false but interval=1s so the time gate would
		// otherwise permit firing. The IntervalEnabled gate must short-
		// circuit before the interval check is even consulted.
		// consolidation_enabled=false suppresses the sibling memory-
		// consolidation timer that otherwise fires on the first tick
		// and would create recorder growth unrelated to this assertion.
		ExtraConfigTOML: "\n[reflection]\ninterval_enabled = false\ninterval = \"1s\"\nbackend_quiet_period = \"1s\"\nconsolidation_enabled = false\n",
		ReadyTimeout:    30 * time.Second,
	})

	scriptBody, err := json.Marshal(map[string]any{"text": "bootstrap"})
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "bootstrap session",
		},
	})

	if !waitForUserMessage(t, h, "workspaces/alpha", "bootstrap session", 15*time.Second) {
		t.Fatalf("bootstrap user message never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	time.Sleep(500 * time.Millisecond)
	before := len(invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha"))

	// One tick + buffer. Since IntervalEnabled=false, reflection must
	// not dispatch regardless of how long the interval has elapsed.
	time.Sleep(35 * time.Second)

	after := len(invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha"))
	if after != before {
		t.Errorf("reflection fired despite interval_enabled=false: recorder entries before=%d after=%d; stderr:\n%s",
			before, after, stderrTail(h.Stderr()))
	}
}

// TestL2_Cron_ConsolidationFiresOnLongerInterval proves consolidation
// dispatches via RunOnceFunc (when set) or branchFn (otherwise) with
// the memory-consolidation prompt. With a short ConsolidationInterval
// and recent interaction, the scheduler should call the RunOnce path
// once per interval. Asserts on the recorder for a consolidation
// prompt invocation in the agent's workdir.
func TestL2_Cron_ConsolidationFiresOnLongerInterval(t *testing.T) {
	t.Parallel()
	const testUserID = 5307

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		// Disable the interval-reflection timer to isolate consolidation,
		// then enable consolidation with a sub-minute interval so it
		// fires within the test window.
		ExtraConfigTOML: "\n[reflection]\ninterval_enabled = false\nconsolidation_enabled = true\nconsolidation_interval = \"1s\"\n",
		ReadyTimeout:    30 * time.Second,
	})

	scriptBody, err := json.Marshal(map[string]any{"text": "bootstrap"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "bootstrap session",
		},
	})

	if !waitForUserMessage(t, h, "workspaces/alpha", "bootstrap session", 15*time.Second) {
		t.Fatalf("bootstrap never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	time.Sleep(500 * time.Millisecond)
	before := len(invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha"))

	// First 30s tick should see consolidation_interval elapsed and
	// dispatch the consolidation branch. No Telegram update is pushed
	// in this window so any recorder growth comes from consolidation.
	time.Sleep(35 * time.Second)

	after := len(invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha"))
	if after <= before {
		t.Fatalf("consolidation did not fire: before=%d after=%d; stderr:\n%s",
			before, after, stderrTail(h.Stderr()))
	}
}

// TestL2_Cron_ConsolidationSkippedWhileReflectionRunning proves the
// "reflection running" guard in maybeConsolidation: if a reflection
// pass is in flight when the consolidation tick fires, consolidation
// defers. Test forces overlap by scripting cc-stub to hang during
// reflection and asserts consolidation only runs after reflection
// completes.
func TestL2_Cron_ConsolidationSkippedWhileReflectionRunning(t *testing.T) {
	t.Parallel()
	const testUserID = 5307

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		// Reflection at interval=20s (passes time-gate after one 30s tick),
		// consolidation enabled at interval=1s so its time-gate trivially
		// passes every tick. The runner ticks at 30s and runs maybeReflection
		// before maybeConsolidation in the same tick — so when reflection
		// fires, consolidation sees reflectionRunning=true on the SAME tick.
		ExtraConfigTOML: "\n[reflection]\ninterval = \"20s\"\nbackend_quiet_period = \"1s\"\nconsolidation_enabled = true\nconsolidation_interval = \"1s\"\n",
		ReadyTimeout:    30 * time.Second,
	})

	scriptBody, err := json.Marshal(map[string]any{"text": "bootstrap"})
	if err != nil {
		t.Fatalf("marshal bootstrap script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "bootstrap session",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "bootstrap session", 15*time.Second) {
		t.Fatalf("bootstrap never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// Refresh ~22s in (with a quick script) so lastInteraction stays recent
	// for the reflection idle gate at T+30s. After refresh completes, write
	// the HANG script so the upcoming reflection turn keeps reflectionRunning
	// true for ~36s across the next tick (where consolidation would otherwise
	// fire).
	time.Sleep(20 * time.Second)
	refreshBody, _ := json.Marshal(map[string]any{"text": "refresh"})
	h.WriteCCStubScript(t, "alpha", refreshBody)
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "keep session warm",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "keep session warm", 15*time.Second) {
		t.Fatalf("refresh never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// 4× sleep 9 ≈ 36s of in-flight time for the reflection turn. branchFn
	// (delegated) blocks until cc-stub finishes the turn, so reflectionRunning
	// stays true for the duration. The T+60s consolidation tick lands inside
	// this window and MUST defer.
	hangSleep := map[string]any{"name": "Bash", "input": map[string]any{"command": "sleep 9"}}
	hangBody, _ := json.Marshal(map[string]any{
		"text": "reflecting (held open)",
		"tool_uses": []map[string]any{
			hangSleep, hangSleep, hangSleep, hangSleep,
		},
	})
	h.WriteCCStubScript(t, "alpha", hangBody)

	// Wait through reflection fire (T+30s tick), the hang window (~T+30s to
	// ~T+66s), and the post-hang consolidation tick (T+90s).
	time.Sleep(70 * time.Second)

	// Find (a) the reflection user_message in main session, (b) the first
	// consolidation invocation AFTER the reflection. Consolidation creates a
	// new isolated CC session in delegated mode (not in branchTypesForMainSession),
	// so it shows up as kind=invocation. We must filter on timestamp because
	// startup RunOnce calls (nudge rule extraction) also create invocations.
	entries := readRecorderEntries(t, h.RecorderPath())
	var reflectionTS time.Time
	for _, e := range entries {
		if e.Kind != "user_message" || !strings.Contains(e.Workdir, "workspaces/alpha") {
			continue
		}
		if strings.Contains(e.TextPrefix, "Reflection Pass") {
			reflectionTS, _ = time.Parse(time.RFC3339Nano, e.Timestamp)
			break
		}
	}
	if reflectionTS.IsZero() {
		t.Fatalf("reflection user_message never recorded — test cannot prove gate\n%s",
			recorderTail(t, h.RecorderPath()))
	}

	var consolidationTS time.Time
	for _, e := range entries {
		if e.Kind != "invocation" || !strings.Contains(e.Workdir, "workspaces/alpha") {
			continue
		}
		ts, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
		// Skip startup invocations (nudge-rule RunOnce, etc.) by requiring
		// the invocation to land AFTER the reflection prompt was injected.
		if ts.After(reflectionTS) {
			consolidationTS = ts
			break
		}
	}
	if consolidationTS.IsZero() {
		t.Fatalf("no consolidation invocation recorded after reflection — gate may be over-blocking, OR consolidation interval mis-configured\n%s",
			recorderTail(t, h.RecorderPath()))
	}
	// The first post-reflection invocation must arrive AFTER the hang would
	// have ended (~36s after reflection started). If it appeared during the
	// hang window, the reflectionRunning gate (or the in-flight gate) didn't
	// engage and consolidation fired while reflection was mid-flight.
	gap := consolidationTS.Sub(reflectionTS)
	if gap < 30*time.Second {
		t.Errorf("first post-reflection invocation %s after reflection — gate did not defer past reflection's in-flight window\n%s",
			gap.Round(time.Second), recorderTail(t, h.RecorderPath()))
	}
}

// TestL2_Cron_ConsolidationTimestampPersistsAcrossRestart proves the
// SessionIndex.SetAgentMetadata("consolidation_last", ...) write at
// the end of maybeConsolidation survives a foci-gw restart: a second
// harness pointed at the same DataDir should pick up the timestamp
// and refuse to fire consolidation again within the configured
// interval. Asserts at most one consolidation invocation across both
// processes.
func TestL2_Cron_ConsolidationTimestampPersistsAcrossRestart(t *testing.T) {
	t.Parallel()
	const testUserID = 5310
	// Strategy: configure consolidation_interval = "1h" — well past the
	// test wall-clock — so a SECOND consolidation would only fire if the
	// persisted timestamp is NOT loaded on restart. The first
	// consolidation fires on the very first cron tick because zero-time
	// + interval rounds well into the past; that fire writes
	// consolidation_last via SetAgentMetadata. After Restart, the new
	// Runner reads consolidation_last in init (keepalive.go:178) and
	// computes nextFire ~1h ahead — every post-restart tick must skip.
	//
	// Harness.Restart() reuses h.configPath (and therefore h.dataDir),
	// so the SessionIndex sqlite file persists across the restart. The
	// recorder file is shared across spawns, so invocations from both
	// processes accumulate to the same JSONL.
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		ExtraConfigTOML: "\n[reflection]\ninterval_enabled = false\nconsolidation_enabled = true\nconsolidation_interval = \"1h\"\n",
		ReadyTimeout:    30 * time.Second,
	})

	scriptBody, err := json.Marshal(map[string]any{"text": "bootstrap"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	// Bootstrap the agent so lastInteraction is recent (consolidation's
	// idle guard at keepalive.go:625 skips when idle > 1h).
	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "bootstrap session",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "bootstrap session", 15*time.Second) {
		t.Fatalf("bootstrap never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// Wait for one cron tick — consolidation fires and writes
	// consolidation_last via SetAgentMetadata. The cron tick interval
	// is 30s; budget 40s to land inside the tick window comfortably.
	time.Sleep(40 * time.Second)
	firstInvocations := len(invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha"))
	if firstInvocations < 2 {
		// 1 bootstrap + 1 consolidation expected. Surface stderr so a
		// scheduling miss is debuggable.
		t.Fatalf("expected at least 2 invocations (bootstrap + first consolidation) before restart; got %d. stderr:\n%s",
			firstInvocations, stderrTail(h.Stderr()))
	}

	// Restart foci-gw — same DataDir, fresh subprocess. Runner re-init
	// reads consolidation_last from the persisted SessionIndex.
	if err := h.Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	// Observe two full cron ticks post-restart (~70s). With
	// consolidation_interval = "1h" and a fresh lastConsolidation from
	// disk pointing at ~70s ago, nextFire is ~59m in the future, so
	// every tick must skip.
	time.Sleep(70 * time.Second)
	secondInvocations := len(invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha"))
	if secondInvocations > firstInvocations {
		// The persisted timestamp would mean nextFire is ~1h ahead, so
		// no consolidation should fire. Any growth implies the persisted
		// value did not gate the second process.
		t.Errorf("consolidation fired after restart despite persisted timestamp: before=%d after=%d. Persisted consolidation_last was ignored.\n--- recorder ---\n%s\n--- stderr ---\n%s",
			firstInvocations, secondInvocations,
			recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}
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
	t.Parallel()
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
	t.Parallel()
	const testUserID = 5400
	const reminderText = "MARKER_WAKE_SURVIVES_RESTART"

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{{
			ID:     "alpha",
			UserID: testUserID,
		}},
		ReadyTimeout: 30 * time.Second,
	})

	// Schedule a wake far enough in the future that we can shut down +
	// restart before it fires. The reminder lives in ReminderStore
	// (per-agent SQLite under workspace .data) which persists across
	// restart.
	scheduleBash := fmt.Sprintf(`foci_remind --text %q --when 10s --wake`, reminderText)
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

	// Sanity: the wake hasn't fired yet (only ~2s have passed).
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if strings.Contains(e.TextPrefix, "SCHEDULED WAKE") && strings.Contains(e.TextPrefix, reminderText) {
			t.Fatalf("wake fired before restart — interval too short to exercise the survival path")
		}
	}

	// Shut down the first foci-gw and start a fresh one against the
	// same data_dir / configPath / workspaces. buildWakeScheduler in
	// the new process must call ReminderStore.PendingWakes at startup
	// and re-arm the persisted wake.
	if err := h.Restart(); err != nil {
		t.Fatalf("Restart(): %v", err)
	}

	// Wait for the SCHEDULED WAKE injection to land in the second
	// process. Budget: 10s original delay + a few seconds of restart
	// overhead — total ~15s.
	deadline := time.Now().Add(15 * time.Second)
	var fired bool
	for time.Now().Before(deadline) {
		for _, e := range readRecorderEntries(t, h.RecorderPath()) {
			if e.Kind == "user_message" && strings.Contains(e.Workdir, "workspaces/alpha") &&
				strings.Contains(e.TextPrefix, "SCHEDULED WAKE") &&
				strings.Contains(e.TextPrefix, reminderText) {
				fired = true
				break
			}
		}
		if fired {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !fired {
		t.Errorf("SCHEDULED WAKE did not fire after restart — persistence/restore path may be broken; stderr (post-restart):\n%s\nrecorder:\n%s",
			stderrTail(h.Stderr()), recorderTail(t, h.RecorderPath()))
	}
}

// TestL2_Cron_WakeReminderRejectsNegativeDelay proves the
// resolveWakeDuration validation: when the cc-stub script invokes
// `foci_remind ... --when -5s --wake true`, the tool returns an error
// without writing to ReminderStore or scheduling a goroutine. Asserts
// the recorder shows no SCHEDULED WAKE injection and the tool exec
// produced a non-zero result.
func TestL2_Cron_WakeReminderRejectsNegativeDelay(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	// NOTE: the previous skip claimed "allowed_users rejects messages
	// from other chat IDs" — that's inaccurate. allowed_users gates on
	// the sender's user_id (Update.Message.From.Id), not the chat id.
	// Two different Telegram chats from the same user produce two
	// distinct session keys (foci's session key includes the chat_id),
	// so a single AgentSpec.UserID is sufficient.
	const testUserID = 5320
	const chatA = 5320 // older session
	const chatB = 5321 // newer session
	const reminderText = "MARKER_ROUTE_TO_ORIGIN"

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{{
			ID:     "alpha",
			UserID: testUserID,
		}},
		ReadyTimeout: 30 * time.Second,
	})

	token := h.AgentBotToken("alpha")
	sendFromChat := func(chatID int64, text string) {
		h.TelegramStub().PushUpdate(token, gotgbot.Update{
			Message: &gotgbot.Message{
				Chat: gotgbot.Chat{Id: chatID, Type: "private"},
				From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
				Text: text,
			},
		})
	}

	// Session A (older): schedule a wake from this chat.
	scheduleBash := fmt.Sprintf(`foci_remind --text %q --when 3s --wake`, reminderText)
	scheduleBody, err := json.Marshal(map[string]any{
		"text": "scheduled-from-A",
		"tool_uses": []map[string]any{
			{"name": "Bash", "input": map[string]any{"command": scheduleBash}},
		},
	})
	if err != nil {
		t.Fatalf("marshal schedule script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scheduleBody)
	sendFromChat(chatA, "schedule from session A")
	if !waitForUserMessage(t, h, "workspaces/alpha", "schedule from session A", 15*time.Second) {
		t.Fatalf("schedule turn (A) never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// Capture session A's cc-stub session_id from the schedule turn's
	// user_message entry. Each foci session_key spawns its own cc-stub
	// subprocess with a distinct session_id, so session_id is a stable
	// proxy for "which foci session".
	sessionAID := ""
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if e.Kind == "user_message" && strings.Contains(e.TextPrefix, "schedule from session A") {
			sessionAID = e.SessionID
			break
		}
	}
	if sessionAID == "" {
		t.Fatalf("could not capture session A's cc-stub session_id from recorder")
	}

	// Drive traffic to session B between schedule and fire. This
	// ensures "most recent active session" != "originating session"
	// so the routing assertion is meaningful.
	sendFromChat(chatB, "traffic to session B before wake fires")
	if !waitForUserMessage(t, h, "workspaces/alpha", "traffic to session B before wake fires", 15*time.Second) {
		t.Fatalf("session B turn never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	sessionBID := ""
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if e.Kind == "user_message" && strings.Contains(e.TextPrefix, "traffic to session B before wake fires") {
			sessionBID = e.SessionID
			break
		}
	}
	if sessionBID == "" || sessionBID == sessionAID {
		t.Fatalf("session B did not spawn a distinct cc-stub subprocess (got session_id=%q, A=%q)",
			sessionBID, sessionAID)
	}

	// Wait for the SCHEDULED WAKE injection to land. Cap at 12s — the
	// 3s wake delay plus some slack for scheduler tick cadence.
	deadline := time.Now().Add(12 * time.Second)
	var wakeEntry recorderEntry
	for time.Now().Before(deadline) {
		for _, e := range readRecorderEntries(t, h.RecorderPath()) {
			if e.Kind == "user_message" && strings.Contains(e.Workdir, "workspaces/alpha") &&
				strings.Contains(e.TextPrefix, "SCHEDULED WAKE") &&
				strings.Contains(e.TextPrefix, reminderText) {
				wakeEntry = e
				break
			}
		}
		if wakeEntry.Kind != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if wakeEntry.Kind == "" {
		t.Fatalf("SCHEDULED WAKE never landed; recorder:\n%s", recorderTail(t, h.RecorderPath()))
	}

	// The wake must route to session A (the originator), not session
	// B (the most-recent). The cc-stub session_id distinguishes them.
	if wakeEntry.SessionID != sessionAID {
		t.Errorf("SCHEDULED WAKE routed to wrong cc-stub session: got session_id=%q, expected %q (session A originator); session B id was %q",
			wakeEntry.SessionID, sessionAID, sessionBID)
	}
}

// TestL2_Cron_WakeReminderWaitsForActiveTurn proves the
// IsProcessing() spin in wakeScheduleFn: when the wake delay elapses
// while a user turn is still being processed on the target session,
// the injection waits rather than racing the active turn. Test
// schedules a 1s wake, immediately sends a Telegram message that
// scripts cc-stub to hang for 3s, and asserts the SCHEDULED WAKE
// user_message appears only after the held turn's user_message.
func TestL2_Cron_WakeReminderWaitsForActiveTurn(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	// INFEASIBLE for L2 as written. The nil-reminderStore branch exists
	// only as a unit-test construct (agents_delegated_test.go:79 omits
	// p.reminderStore). In production, initStandaloneStores (memory_init.go:62)
	// unconditionally creates the store and log.Fatalf's on failure — there
	// is no path that reaches a running foci-gw with reminderStore == nil.
	// The "read-only workspace .data" idea from the previous skip exits the
	// gateway before ready (Fatalf), so the L2 startup observation fails too.
	//
	// To unblock, foci would need a real config opt-out for reminders (e.g.
	// `[reminders] enabled = false`), and initStandaloneStores would have to
	// honour it by skipping reminderStore creation. That's a small foci-side
	// change — not just harness scaffolding. Covered already by the agents-
	// shared unit test at agents_notify_test.go:260 which exercises the nil-
	// store branch of buildWakeScheduler directly.
	t.Skip("INFEASIBLE: foci has no production opt-out for reminderStore. The nil-store branch exists only as a unit-test construct (agents_delegated_test.go:79). Covered by agents_notify_test.go:260. Unblock: add `[reminders] enabled = false` config + skip-on-false in initStandaloneStores (memory_init.go:62).")
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	const testUserID = 5202

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		ReadyTimeout: 30 * time.Second,
	})

	// Snapshot recorder entry count before the bad call so we can
	// assert nothing new lands.
	before := len(invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha"))

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
	after := len(invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha"))
	if after != before {
		t.Errorf("rejected /wake still produced recorder entries (before=%d after=%d)", before, after)
	}
}

// TestL2_Cron_BranchOneshotMalformedPromptFile proves the CLI surfaces
// a file-not-found / unreadable error when -mf points at a missing
// path, rather than dispatching with an empty prompt. Asserts the CLI
// exits non-zero with a path-related error and the recorder shows no
// branch invocation.
//
// Implementation note: cmdBranch's resolveMessage() reads the message
// file BEFORE issuing the HTTP /wake request, so the failure path is
// purely CLI-side. We don't need to wire the CLI's transport to the
// harness gateway — the file read fails before any connection attempt.
// We still pass --addr <unreachable> for hygiene to guarantee the CLI
// doesn't accidentally talk to a production foci-gw if one is running.
func TestL2_Cron_BranchOneshotMalformedPromptFile(t *testing.T) {
	t.Parallel()
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:       []testharness.AgentSpec{{ID: "alpha", UserID: 9300}},
		ReadyTimeout: 30 * time.Second,
	})

	cliBin := h.FociCLI(t)
	missingPath := filepath.Join(h.TempDir(), "does-not-exist", "prompt.txt")

	// Snapshot recorder entries before invocation so we can assert no
	// NEW invocations follow (filters out any startup-time RunOnce work).
	before := len(readRecorderEntries(t, h.RecorderPath()))

	cmd := exec.Command(cliBin,
		"--addr", "127.0.0.1:1", // unreachable; CLI should never reach it
		"branch",
		"-a", "alpha",
		"-mf", missingPath,
	)
	// Clear FOCI_* env that might leak from the test process (e.g.
	// FOCI_GW_SOCK pointing at production /home/foci/...).
	cmd.Env = []string{"HOME=" + h.TempDir(), "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("CLI exit was 0 but should have failed on missing -mf path\nstdout+stderr:\n%s", string(out))
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("CLI did not exit with ExitError (run failed at exec level): %v\noutput:\n%s", err, string(out))
	}
	if exitErr.ExitCode() == 0 {
		t.Fatalf("CLI exit code was 0 despite ExitError; want non-zero")
	}
	combined := string(out)
	if !strings.Contains(combined, "reading message file") {
		t.Errorf("CLI stderr/stdout missing expected file-read error tag.\ngot:\n%s", combined)
	}

	// Give any in-flight startup recorder writes ~500ms to settle, then
	// assert no new entries appeared as a side effect of the failed CLI run.
	time.Sleep(500 * time.Millisecond)
	after := len(readRecorderEntries(t, h.RecorderPath()))
	if after != before {
		t.Errorf("recorder grew across failed CLI run (before=%d after=%d) — CLI may have dispatched a branch despite the malformed -mf", before, after)
	}
}

// TestL2_Cron_RateLimitGateBlocksAllSchedulers proves the shared
// canFireFn / sessionKeyFn check at the top of every scheduler
// (background, reflection, consolidation): when the rate-limit/mana
// callback returns canFire=false with a reason, none of the three
// schedulers dispatch despite all other conditions being met.
//
// Driver: h.SetCanFire pins the agent's CanFireFunc return value via
// the testharness control socket. The override is checked before the
// final dispatch in maybeBackgroundWork, maybeReflection, and
// maybeConsolidation — all three sit downstream of the eligibility
// gates (open-todo count, idle-vs-interval, lastConsolidation), so
// the test still must set up eligible conditions to prove canFire is
// what's blocking.
func TestL2_Cron_RateLimitGateBlocksAllSchedulers(t *testing.T) {
	t.Parallel()
	const testUserID = 5311

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		// All three schedulers enabled with short intervals. With the
		// canFire override locked to false, none should dispatch.
		ExtraConfigTOML: "\n[background]\nenabled = true\ninterval = \"1s\"\n\n[reflection]\ninterval = \"20s\"\nbackend_quiet_period = \"1s\"\nconsolidation_enabled = true\nconsolidation_interval = \"1s\"\n",
		ReadyTimeout:    30 * time.Second,
	})

	// Pin canFire to (false, "test-block-all") BEFORE any tick fires.
	if err := h.SetCanFire("alpha", false, "test-block-all"); err != nil {
		t.Fatalf("SetCanFire: %v", err)
	}

	// Bootstrap turn seeds a background-tagged todo so the bg
	// open-todo gate is satisfied. Without the canFire block this
	// matches TestL2_Cron_BackgroundFiresWhenIdleWithOpenTodos.
	seedBash := `foci_todo add --text "background test todo" --tag background`
	scriptBody, err := json.Marshal(map[string]any{
		"text": "bootstrap with seed",
		"tool_uses": []map[string]any{
			{"name": "Bash", "input": map[string]any{"command": seedBash}},
		},
	})
	if err != nil {
		t.Fatalf("marshal bootstrap script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "bootstrap session",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "bootstrap session", 15*time.Second) {
		t.Fatalf("bootstrap user message never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	time.Sleep(500 * time.Millisecond)
	beforeInvocations := len(invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha"))

	// Push a refresh ~5s before the next 30s tick to keep reflection
	// eligible (sinceLastInteraction < 20s interval). Without this the
	// reflection eligibility gate skips before canFire is consulted,
	// which weakens the "canFire blocked reflection" claim.
	time.Sleep(20 * time.Second)
	refreshBody, _ := json.Marshal(map[string]any{"text": "refresh"})
	h.WriteCCStubScript(t, "alpha", refreshBody)
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "keep session warm",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "keep session warm", 15*time.Second) {
		t.Fatalf("refresh never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// Wait through the next 30s tick. With canFire blocked, no
	// scheduler should dispatch.
	time.Sleep(20 * time.Second)

	// Consolidation creates new invocations; bg + reflection inject
	// user_messages. Assert all three markers are absent.
	afterInvocations := len(invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha"))
	if afterInvocations > beforeInvocations {
		t.Errorf("consolidation may have fired despite canFire=false: invocations before=%d after=%d\nstderr:\n%s",
			beforeInvocations, afterInvocations, stderrTail(h.Stderr()))
	}

	for _, e := range userMessagesForWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha") {
		if strings.Contains(e.TextPrefix, "[background]") && strings.Contains(e.TextPrefix, "Background Work") {
			t.Errorf("background fired despite canFire=false: %q\nstderr:\n%s", e.TextPrefix, stderrTail(h.Stderr()))
		}
		if strings.Contains(e.TextPrefix, "Reflection Pass") {
			t.Errorf("reflection fired despite canFire=false: %q\nstderr:\n%s", e.TextPrefix, stderrTail(h.Stderr()))
		}
	}
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
	t.Parallel()
	const testUserID = 5308

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: testUserID},
		},
		ExtraConfigTOML: "\n[keepalive]\nenabled = true\ninterval = \"1s\"\n",
		ReadyTimeout:    30 * time.Second,
	})

	// Bootstrap with a quick script — keepalive needs a default parent
	// session before it can fire.
	bootstrapBody, err := json.Marshal(map[string]any{"text": "bootstrap"})
	if err != nil {
		t.Fatalf("marshal bootstrap script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", bootstrapBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "bootstrap session",
		},
	})
	if !waitForUserMessage(t, h, "workspaces/alpha", "bootstrap session", 15*time.Second) {
		t.Fatalf("bootstrap never processed; stderr:\n%s", stderrTail(h.Stderr()))
	}
	time.Sleep(500 * time.Millisecond)

	// Write a HANG script for the upcoming keepalive turn. branchFn
	// (delegated, keepalive) injects the [KEEPALIVE] prompt as a
	// user_message in the main session — cc-stub picks up this script
	// to script the response. 4× sleep 9 ≈ 36s of in-flight time.
	hangSleep := map[string]any{"name": "Bash", "input": map[string]any{"command": "sleep 9"}}
	hangBody, _ := json.Marshal(map[string]any{
		"text": "keepalive-acknowledged",
		"tool_uses": []map[string]any{
			hangSleep, hangSleep, hangSleep, hangSleep,
		},
	})
	h.WriteCCStubScript(t, "alpha", hangBody)

	// Wait until the keepalive tick at T+30s has fired and the hang turn
	// is well underway. T+35s is safely inside the in-flight window.
	time.Sleep(35 * time.Second)

	// Push a Telegram message during the in-flight keepalive turn. foci's
	// receive path should queue it (not drop it) and process once the
	// keepalive turn completes.
	const userMsgText = "queued-during-keepalive"
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: userMsgText,
		},
	})

	// Wait until the hang completes (~T+66s) and the queued user message
	// has a chance to process (cc-stub default echo, no script needed).
	// 40s past push gives ~T+75s — comfortable.
	time.Sleep(40 * time.Second)

	// Recorder should contain BOTH the keepalive prompt AND the user
	// message, with the keepalive arriving first.
	var keepaliveTS, userMsgTS time.Time
	for _, e := range userMessagesForWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha") {
		ts, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
		if strings.Contains(e.TextPrefix, "[KEEPALIVE]") && keepaliveTS.IsZero() {
			keepaliveTS = ts
		}
		if strings.Contains(e.TextPrefix, userMsgText) && userMsgTS.IsZero() {
			userMsgTS = ts
		}
	}
	if keepaliveTS.IsZero() {
		t.Fatalf("keepalive prompt never reached cc-stub — cannot prove queuing behaviour\n%s",
			recorderTail(t, h.RecorderPath()))
	}
	if userMsgTS.IsZero() {
		t.Fatalf("user message %q never processed — was dropped instead of queued\n%s",
			userMsgText, recorderTail(t, h.RecorderPath()))
	}
	if !userMsgTS.After(keepaliveTS) {
		t.Errorf("user message arrived before/equal to keepalive (ka=%s user=%s) — queue order violated",
			keepaliveTS.Format(time.RFC3339Nano), userMsgTS.Format(time.RFC3339Nano))
	}
}
