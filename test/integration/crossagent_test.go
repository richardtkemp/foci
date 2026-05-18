//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"foci/internal/testharness"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// TestL2_CrossAgent_SendToSession_RoutesToTargetWorkdir is the
// regression net for the bug fixed in d87875c1 (foci-gw commit on
// 2026-05-15): when fotini's session calls send_to_session targeting
// clutch's session with reply_to=caller, foci must dispatch the message
// on clutch's Agent (which spawns CC in clutch's workdir), NOT on
// fotini's Agent (which would have spawned CC in fotini's workdir,
// leaving clutch's resume id pointing at an orphan JSONL in the wrong
// project directory).
//
// Mechanism:
//  1. Both agents share the test user id, so their session keys are
//     fotini/c<USER>/<ts> and clutch/c<USER>/<ts>.
//  2. Prime clutch first with a plain Telegram message so its session
//     exists and the partial-key "clutch/c<USER>" resolves.
//  3. Script fotini's cc-stub so that on the next user message it emits
//     a Bash tool_use running `foci_send_to_session clutch/c<USER>
//     --message "..."`. cc-stub literally runs the bash subshell — the
//     shell function reaches foci's exec bridge socket via FOCI_SOCK.
//  4. Send fotini a Telegram message → fotini's cc-stub emits the
//     tool_use → runs the bash → exec bridge fires send_to_session →
//     async notifier dispatches to clutch's Agent → clutch's cc-stub
//     receives a new user message containing the test marker.
//  5. Assert: a user_message recorder entry exists with workdir
//     containing "workspaces/clutch" AND text_prefix containing the
//     test marker string.
//
// Without the fix, the notifier would have called HandleMessage on
// fotini's Agent with clutch's session key. The invariant guard added
// in d87875c1 would today reject that as "invariant violation", so the
// user_message entry under clutch's workdir would never appear and the
// test would fail with a clear "send_to_session never reached clutch"
// signal. With the fix in place, the entry appears as expected.
func TestL2_CrossAgent_SendToSession_RoutesToTargetWorkdir(t *testing.T) {
	t.Parallel()
	const testUserID = 4242
	const testMarker = "MARKER_CROSS_AGENT_REGRESSION_NET"

	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "fotini", UserID: testUserID},
			{ID: "clutch", UserID: testUserID},
		},
		ReadyTimeout: 30 * time.Second,
	})

	// Step 1: prime clutch so its session exists and the partial-key
	// resolver in NewSendToSessionTool can find it.
	clutchToken := h.AgentBotToken("clutch")
	h.TelegramStub().PushUpdate(clutchToken, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "priming clutch",
		},
	})
	waitForUserMessage(t, h, "workspaces/clutch", "priming clutch", 15*time.Second)

	// Step 2: script fotini's cc-stub to emit a Bash tool_use that
	// invokes foci_send_to_session targeting clutch's session. Construct
	// via json.Marshal so embedded quotes / shell quoting don't smear
	// the JSON envelope (Go's %q produces a JSON-escape-incompatible
	// double-quoted string).
	partialKey := fmt.Sprintf("clutch/c%d", testUserID)
	bashCmd := fmt.Sprintf(`foci_send_to_session %s --message %q`, partialKey, testMarker+" from fotini")
	scriptBody, err := json.Marshal(map[string]any{
		"text": "okay, forwarding to clutch",
		"tool_uses": []map[string]any{
			{
				"name":  "Bash",
				"input": map[string]any{"command": bashCmd},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal script body: %v", err)
	}
	h.WriteCCStubScript(t, "fotini", scriptBody)

	// Step 3: send fotini a Telegram message. fotini's cc-stub emits
	// the scripted Bash tool_use, runs `foci_send_to_session`, and the
	// notifier routes the message to clutch's Agent.
	fotiniToken := h.AgentBotToken("fotini")
	h.TelegramStub().PushUpdate(fotiniToken, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: testUserID, Type: "private"},
			From: &gotgbot.User{Id: testUserID, FirstName: "Tester"},
			Text: "fotini, please tell clutch hi",
		},
	})

	// Step 4: poll the recorder for a user_message entry in clutch's
	// workdir containing the test marker. If the bug were present, the
	// marker would land under fotini/workdir; if the invariant guard
	// rejected the cross-agent dispatch, neither would appear in time.
	if !waitForUserMessage(t, h, "workspaces/clutch", testMarker, 20*time.Second) {
		t.Errorf("send_to_session marker %q never reached a user_message in workspaces/clutch\n--- recorder ---\n%s\n--- stderr tail ---\n%s",
			testMarker, recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
	}

	// Negative assertion: the marker MUST NOT land in fotini's workdir
	// AS THE ORIGINAL CROSS-AGENT DISPATCH. If the bug regresses, the
	// marker text arrives directly (a raw "MESSAGE FROM SESSION ..."
	// envelope addressed by fotini to itself). The reply_to=caller
	// echo path is benign and ALSO contains the marker (clutch's
	// default-echo response includes the original payload, then
	// async-injected back to fotini wrapped in a "SESSION RESPONSE"
	// header) — so plain "marker in fotini workdir" is racy: it
	// fires whenever the echo lands first.
	//
	// Discriminator: the bug path lacks the "[SESSION RESPONSE @"
	// header that send_to_session wraps every reply_to=caller round-
	// trip in (see notifier.InjectToAgent). We ignore entries that
	// carry that wrapper; what's left is original dispatch payload.
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if e.Kind != "user_message" || !strings.Contains(e.Workdir, "workspaces/fotini") {
			continue
		}
		if !strings.Contains(e.TextPrefix, testMarker) {
			continue
		}
		if strings.Contains(e.TextPrefix, "[SESSION RESPONSE @") {
			// Echo round-trip — expected with reply_to=caller. Not a regression.
			continue
		}
		t.Errorf("regression: marker landed in fotini's workdir as a bare cross-agent dispatch (%q) — fotini's Agent processed clutch's session key\n--- entry text ---\n%s", e.Workdir, e.TextPrefix)
	}
}

// waitForUserMessage polls the recorder until a user_message entry
// appears with workdir containing workdirSubstr AND text_prefix
// containing textSubstr. Returns true on hit, false on timeout.
func waitForUserMessage(t *testing.T, h *testharness.Harness, workdirSubstr, textSubstr string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, e := range readRecorderEntries(t, h.RecorderPath()) {
			if e.Kind == "user_message" && strings.Contains(e.Workdir, workdirSubstr) && strings.Contains(e.TextPrefix, textSubstr) {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// recorderTail returns the last ~1KB of the recorder file for failure logs.
func recorderTail(t *testing.T, path string) string {
	entries := readRecorderEntries(t, path)
	if len(entries) > 20 {
		entries = entries[len(entries)-20:]
	}
	var sb strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&sb, "  %s\tkind=%s\tworkdir=%s\tsession=%s\ttext=%q\n",
			e.Timestamp, e.Kind, e.Workdir, e.SessionID, e.TextPrefix)
	}
	return sb.String()
}

// stderrTail returns the last ~3KB of foci-gw stderr for failure logs.
func stderrTail(s string) string {
	const cap = 3000
	if len(s) > cap {
		return "..." + s[len(s)-cap:]
	}
	return s
}
