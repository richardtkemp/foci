//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"foci/internal/fap"
	"foci/internal/testharness"
)

// TestL2_PlatformConnect_TelegramDownAtBootDoesNotBlockStartup is #2043: with
// Telegram unreachable at boot (getMe failing, transient), foci-gw must still
// finish starting, the app platform must serve a turn end to end meanwhile,
// and once Telegram answers again the bot attaches and delivers — with no
// restart.
//
// Before #2043 the getMe retry ran inside the sequential per-agent platform
// setup, so StartGateway never saw the "started N agent(s)" line at all.
func TestL2_PlatformConnect_TelegramDownAtBootDoesNotBlockStartup(t *testing.T) {
	testharness.ParallelWait(t)
	const userID = 9431
	h, err := testharness.TryStartGateway(t, testharness.HarnessOptions{
		Agents:          []testharness.AgentSpec{{ID: "alpha", UserID: userID}},
		ExtraConfigTOML: "\n[[platforms]]\nid = \"app\"\n",
		ReadyTimeout:    30 * time.Second,
		BeforeSpawn: func(stub *testharness.TelegramStub) {
			// 502 is transient: the connect must keep retrying, not give up.
			stub.InjectErrorPersistent("getMe", 502, "Bad Gateway")
		},
	})
	if err != nil {
		t.Fatalf("foci-gw did not finish starting while Telegram was down: %v", err)
	}
	if !waitForStderr(h, "attempt 1 failed (transient, will retry)", 20*time.Second) {
		t.Fatalf("Telegram's getMe was never retried in the background; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// The app platform works while Telegram is still down.
	ws := dialAppDevice(t, h, "alpha")
	convID := fap.NewULID()
	sendAppFrame(t, ws, fap.TypeMessage, fap.ClientMessage{ConversationID: convID, AgentID: "alpha", Text: "app while telegram down"})
	if !waitForAppText(ws, "app while telegram down", 30*time.Second) {
		t.Fatalf("app turn got no reply while Telegram was down; stderr:\n%s", stderrTail(h.Stderr()))
	}
	if strings.Contains(h.Stderr(), "connected on attempt") {
		t.Fatal("Telegram connected before the fault was cleared: the test proved nothing about the outage")
	}

	// Telegram comes back: the bot attaches and delivers without a restart.
	h.TelegramStub().ClearInjections("getMe")
	if !waitForStderr(h, "connected on attempt", 60*time.Second) {
		t.Fatalf("Telegram never attached after getMe recovered; stderr:\n%s", stderrTail(h.Stderr()))
	}
	pushUserMessage(t, h, "alpha", userID, "telegram after recovery")
	if got := waitForSendMessageContaining(h, h.AgentBotToken("alpha"), "telegram after recovery", 30*time.Second); got == "" {
		t.Fatalf("attached Telegram bot did not deliver a reply; sent:\n%s\nstderr:\n%s",
			sentCallsTail(h.TelegramStub(), h.AgentBotToken("alpha")), stderrTail(h.Stderr()))
	}
}

// dialAppDevice pairs a device with the gateway over its unix socket (the
// same-user path, no API key) and returns a FAP socket that has sent hello.
func dialAppDevice(t *testing.T, h *testharness.Harness, agentID string) *websocket.Conn {
	t.Helper()
	sock := h.SocketPath()
	hc := gwUnixClient(sock)

	// Mint a single-use pairing key (what `foci pair-key` does).
	body, _ := json.Marshal(map[string]string{"command": "/pair", "agent": agentID})
	resp, err := hc.Post("http://foci-gw/command", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /command /pair: %v", err)
	}
	var cmdOut struct {
		Response string `json:"response"`
	}
	err = json.NewDecoder(resp.Body).Decode(&cmdOut)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("decode /pair response: %v", err)
	}
	_, after, ok := strings.Cut(cmdOut.Response, "single use):\n")
	if !ok {
		t.Fatalf("no pairing key in /pair response: %q", cmdOut.Response)
	}
	key, _, _ := strings.Cut(after, "\n")

	// Exchange it for a device token.
	body, _ = json.Marshal(map[string]string{"deviceId": "l2-2043", "label": "l2"})
	req, _ := http.NewRequest(http.MethodPost, "http://foci-gw/app/pair", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(key))
	req.Header.Set("Content-Type", "application/json")
	resp, err = hc.Do(req)
	if err != nil {
		t.Fatalf("POST /app/pair: %v", err)
	}
	var pairOut struct {
		DeviceToken string `json:"deviceToken"`
	}
	err = json.NewDecoder(resp.Body).Decode(&pairOut)
	_ = resp.Body.Close()
	if err != nil || pairOut.DeviceToken == "" {
		t.Fatalf("pairing failed (status %d): %v", resp.StatusCode, err)
	}

	d := websocket.Dialer{
		NetDial:          func(_, _ string) (net.Conn, error) { return net.Dial("unix", sock) },
		HandshakeTimeout: 10 * time.Second,
	}
	ws, _, err := d.Dial("ws://foci-gw/app/ws", http.Header{
		"Authorization":          {"Bearer " + pairOut.DeviceToken},
		"Sec-WebSocket-Protocol": {fap.Subprotocol},
	})
	if err != nil {
		t.Fatalf("dial /app/ws: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	sendAppFrame(t, ws, fap.TypeHello, fap.ClientHello{Client: fap.ClientInfo{App: "l2", OS: "test", Version: "0", DeviceID: "l2-2043"}})
	return ws
}

// sendAppFrame writes one client FAP envelope.
func sendAppFrame(t *testing.T, ws *websocket.Conn, typ string, payload any) {
	t.Helper()
	d, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s: %v", typ, err)
	}
	env := fap.Envelope{T: typ, ID: fap.NewULID(), V: fap.ProtocolVersion, D: d}
	if err := ws.WriteJSON(env); err != nil {
		t.Fatalf("send %s: %v", typ, err)
	}
}

// waitForAppText reads server frames until a message/text frame carries
// cc-stub's echo of probe, or the timeout passes.
func waitForAppText(ws *websocket.Conn, probe string, timeout time.Duration) bool {
	if timeout < testharness.CorrectnessWaitFloor {
		timeout = testharness.CorrectnessWaitFloor
	}
	_ = ws.SetReadDeadline(time.Now().Add(timeout))
	for {
		var env fap.Envelope
		if err := ws.ReadJSON(&env); err != nil {
			return false // deadline passed (or the socket closed)
		}
		switch env.T {
		case fap.TypeMessage, fap.TypeTextDelta, fap.TypeTextEnd:
			if raw := string(env.D); strings.Contains(raw, "stub-reply") && strings.Contains(raw, probe) {
				return true
			}
		}
	}
}
