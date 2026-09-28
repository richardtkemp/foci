//go:build integration

package integration

import (
	"encoding/json"
	"testing"
	"time"

	"foci/internal/fap"
	"foci/internal/testharness"
)

// TestL2_App_MessageConsumedTick: an app message the claude-code backend
// acknowledges consuming (cc-stub echoes user_message_uuids, as real CC does)
// comes back to the app as message.consumed carrying the message's own
// envelope id — the ✓✓ tick (#2050).
func TestL2_App_MessageConsumedTick(t *testing.T) {
	testharness.ParallelWait(t)
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:          []testharness.AgentSpec{{ID: "alpha", UserID: 20505}},
		ExtraConfigTOML: "\n[[platforms]]\nid = \"app\"\n",
		ReadyTimeout:    30 * time.Second,
	})
	ws := dialAppDevice(t, h, "alpha")

	convID := fap.NewULID()
	msgID := fap.NewULID()
	d, _ := json.Marshal(fap.ClientMessage{ConversationID: convID, AgentID: "alpha", Text: "MARKER_2050E tick me"})
	if err := ws.WriteJSON(fap.Envelope{T: fap.TypeMessage, ID: msgID, V: fap.ProtocolVersion, D: d}); err != nil {
		t.Fatalf("send: %v", err)
	}

	_ = ws.SetReadDeadline(time.Now().Add(testharness.CorrectnessWaitFloor))
	for {
		var env fap.Envelope
		if err := ws.ReadJSON(&env); err != nil {
			t.Fatalf("no message.consumed for %s before the deadline (%v); recorder:\n%s\nstderr:\n%s",
				msgID, err, recorderTail(t, h.RecorderPath()), stderrTail(h.Stderr()))
		}
		if env.T != fap.TypeMessageConsumed {
			continue
		}
		var mc fap.MessageConsumed
		if err := json.Unmarshal(env.D, &mc); err != nil {
			t.Fatalf("decode message.consumed: %v", err)
		}
		if mc.ConversationID != convID || mc.MessageID != msgID {
			t.Fatalf("message.consumed = %+v, want conversation %s message %s", mc, convID, msgID)
		}
		return
	}
}
