//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"foci/internal/fap"
	"foci/internal/testharness"
)

// waitForAppMedia reads server frames until a media frame named name arrives,
// or the timeout passes.
func waitForAppMedia(ws *websocket.Conn, name string, timeout time.Duration) (fap.Media, bool) {
	if timeout < testharness.CorrectnessWaitFloor {
		timeout = testharness.CorrectnessWaitFloor
	}
	_ = ws.SetReadDeadline(time.Now().Add(timeout))
	for {
		var env fap.Envelope
		if err := ws.ReadJSON(&env); err != nil {
			return fap.Media{}, false
		}
		if env.T != fap.TypeMedia {
			continue
		}
		var m fap.Media
		if err := json.Unmarshal(env.D, &m); err == nil && m.Name == name {
			return m, true
		}
	}
}

// TestL2_AppMedia_UnboundSessionSendsUp is #2755 in a real gateway: a session
// with no app conversation runs send_to_chat with a file, and the file lands in
// the chat the gateway's family-tree walk names (app.SetSessionChats, wired in
// main). Before, the send returned nil and emitted no frame.
func TestL2_AppMedia_UnboundSessionSendsUp(t *testing.T) {
	testharness.ParallelWait(t)
	const userID = 9455
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents:          []testharness.AgentSpec{{ID: "alpha", UserID: userID}},
		ExtraConfigTOML: "\n[[platforms]]\nid = \"app\"\n",
		ReadyTimeout:    30 * time.Second,
	})

	// The agent's app conversation: the one chat that exists.
	ws := dialAppDevice(t, h, "alpha")
	convID := fap.NewULID()
	sendAppFrame(t, ws, fap.TypeMessage, fap.ClientMessage{ConversationID: convID, AgentID: "alpha", Text: "open the chat"})
	if !waitForAppText(ws, "open the chat", 30*time.Second) {
		t.Fatalf("app turn got no reply; stderr:\n%s", stderrTail(h.Stderr()))
	}

	filePath := filepath.Join(t.TempDir(), "report-2755.md")
	if err := os.WriteFile(filePath, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	writeSendToChatScript(t, h, "alpha",
		fmt.Sprintf(`foci_send_to_chat --description %q --file %q`, "send the report", filePath),
		"[[NO_RESPONSE]]")

	// A named session has no chat in its key and no conversation of its own.
	body, err := json.Marshal(map[string]string{"agent": "alpha", "session": "report-l2?create=true", "text": "send it"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := gwUnixClient(h.SocketPath()).Post("http://foci-gw/send", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /send: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("POST /send: status %d", resp.StatusCode)
	}

	m, ok := waitForAppMedia(ws, "report-2755.md", 30*time.Second)
	if !ok {
		t.Fatalf("no media frame reached the app; stderr:\n%s", stderrTail(h.Stderr()))
	}
	if m.ConversationID != convID {
		t.Errorf("file landed in %s, want the agent's chat %s", m.ConversationID, convID)
	}
	if !strings.Contains(h.Stderr(), "the session has no conversation") {
		t.Errorf("no walk logged: the send did not go up the tree; stderr:\n%s", stderrTail(h.Stderr()))
	}
}
