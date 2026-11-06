//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"foci/internal/fap"
	"foci/internal/testharness"
)

// compactQuietWindow is how long the #2891 test keeps reading after the
// completion notice, to catch a second confirmation. /compact's reply followed
// the notice by 332 ms in the frames that showed the bug. Load can only make
// this window miss a duplicate, never fail a correct build.
const compactQuietWindow = 3 * time.Second

// compactFrames is what one app /compact showed the user.
type compactFrames struct {
	notices []string // notification frames saying compaction finished
	replies []string // system-role messages mentioning compaction
}

// readCompactFrames reads server frames until stop reports true (then for
// compactQuietWindow more) or the timeout passes.
func readCompactFrames(t *testing.T, ws *websocket.Conn, timeout time.Duration, stop func(compactFrames) bool) compactFrames {
	t.Helper()
	if timeout < testharness.CorrectnessWaitFloor {
		timeout = testharness.CorrectnessWaitFloor
	}
	var got compactFrames
	deadline := time.Now().Add(timeout)
	stopped := false
	for {
		_ = ws.SetReadDeadline(deadline)
		var env fap.Envelope
		if err := ws.ReadJSON(&env); err != nil {
			return got // deadline passed (or the socket closed)
		}
		switch env.T {
		case fap.TypeNotification:
			var n fap.Notification
			if err := json.Unmarshal(env.D, &n); err == nil && strings.Contains(n.Text, "ontext compacted") {
				got.notices = append(got.notices, n.Text)
			}
		case fap.TypeMessage:
			var m fap.ServerMessage
			if err := json.Unmarshal(env.D, &m); err == nil && m.Role == "system" && strings.Contains(m.Text, "ompact") {
				got.replies = append(got.replies, m.Text)
			}
		}
		if !stopped && stop(got) {
			stopped = true
			deadline = time.Now().Add(compactQuietWindow)
		}
	}
}

// runAppCompact opens an app chat and runs /compact in it, against a cc-stub
// that really completes the compaction.
func runAppCompact(t *testing.T, platformTOML string, stop func(compactFrames) bool) compactFrames {
	t.Helper()
	testharness.ParallelWait(t)
	const userID = 9891
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{{
			ID:       "alpha",
			UserID:   userID,
			ExtraEnv: map[string]string{"CCSTUB_EMIT_COMPACT_BOUNDARY": "1"},
		}},
		ExtraConfigTOML: platformTOML,
		ReadyTimeout:    30 * time.Second,
	})
	ws := dialAppDevice(t, h, "alpha")
	convID := fap.NewULID()
	sendAppFrame(t, ws, fap.TypeMessage, fap.ClientMessage{ConversationID: convID, AgentID: "alpha", Text: "prime"})
	if !waitForAppText(ws, "prime", 30*time.Second) {
		t.Fatalf("app turn got no reply; stderr:\n%s", stderrTail(h.Stderr()))
	}
	sendAppFrame(t, ws, fap.TypeCommand, fap.Command{ConversationID: convID, AgentID: "alpha", Name: "compact"})
	return readCompactFrames(t, ws, 60*time.Second, stop)
}

// TestL2_AppCompact_ConfirmsOnce is #2891: /compact in the app showed
// "✅ Context compacted (delegated)." and then "Context compacted (delegated).".
// The notice stays (it carries the summary chit); the separate message goes.
func TestL2_AppCompact_ConfirmsOnce(t *testing.T) {
	got := runAppCompact(t, "\n[[platforms]]\nid = \"app\"\n", func(f compactFrames) bool {
		return len(f.notices) > 0
	})
	if len(got.notices) != 1 {
		t.Fatalf("completion notices = %q, want exactly one", got.notices)
	}
	if len(got.replies) != 0 {
		t.Errorf("/compact also sent a separate message: %q", got.replies)
	}
}

// TestL2_AppCompact_NotifyOffKeepsReply: with compaction_notify off there is no
// notice, so the reply is the only sign that /compact worked and must stay.
func TestL2_AppCompact_NotifyOffKeepsReply(t *testing.T) {
	toml := "\n[[platforms]]\nid = \"app\"\n[platforms.notify]\ncompaction_notify = false\n"
	got := runAppCompact(t, toml, func(f compactFrames) bool {
		return len(f.replies) > 0
	})
	if len(got.notices) != 0 {
		t.Errorf("compaction_notify=false still sent notices: %q", got.notices)
	}
	if len(got.replies) != 1 || !strings.Contains(got.replies[0], "Context compacted") {
		t.Errorf("replies = %q, want the one confirmation", got.replies)
	}
}
