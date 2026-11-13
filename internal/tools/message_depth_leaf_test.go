package tools

import (
	"encoding/json"
	"testing"

	"foci/internal/platform"
	"foci/internal/session"
)

// TestSendToChat_DepthLeafRoute (#3087): an open depth leaf sends to its own
// conversation (chat 0 = the sender's own session); once it has reported it
// keeps today's route, the root chat its key names. Not parallel: the depth
// manager is registered process-wide.
func TestSendToChat_DepthLeafRoute(t *testing.T) {
	h := newDepthHarness(t, nil)
	kids := h.fanout(t, depthRoot, "Alpha", "Beta")
	rootChat := ChatIDFromSessionKey(depthRoot)

	send := func(key string) *mockSender {
		t.Helper()
		mock := &mockSender{sessionKey: key}
		tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil,
			func(string) session.SessionType { return session.SessionTypeDepth }, nil)
		params, _ := json.Marshal(map[string]string{"text": "hi"})
		if _, err := tool.Execute(depthCtx(key), params); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		return mock
	}

	if m := send(kids[0]); len(m.textCalls) != 1 || len(m.chatTextCalls) != 0 {
		t.Errorf("open leaf: own=%v chat=%v, want one send to its own session", m.textCalls, m.chatTextCalls)
	}
	if err := h.report(t, kids[1], "settled"); err != nil {
		t.Fatal(err)
	}
	if m := send(kids[1]); len(m.chatTextCalls) != 1 || m.chatTextCalls[0].chatID != rootChat || len(m.textCalls) != 0 {
		t.Errorf("reported leaf: own=%v chat=%v, want one send to root chat %d", m.textCalls, m.chatTextCalls, rootChat)
	}
}
