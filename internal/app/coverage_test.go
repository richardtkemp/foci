package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"foci/internal/agent"
	"foci/internal/app/fap"
	"foci/internal/command"
)

// fakeAgent is a minimal agentCore that captures the enqueued envelope.
type fakeAgent struct {
	env *agent.Envelope
}

func (f *fakeAgent) Enqueue(e agent.Envelope)                 { f.env = &e }
func (f *fakeAgent) MetaStatus(string) (*int, string, string) { return nil, "", "" }

// registerFakeAgent wires a conn backed by a fakeAgent into the hub.
func registerFakeAgent(h *Hub, agentID string) *fakeAgent {
	fa := &fakeAgent{}
	h.agents[agentID] = &appConn{hub: h, agentID: agentID, agentRef: fa}
	h.agentOrder = append(h.agentOrder, agentID)
	return fa
}

func TestRouteUserTurn_EnqueuesEnvelope(t *testing.T) {
	h := newTestHub()
	fa := registerFakeAgent(h, "ag")
	c := fakeClient()
	c.hub = h
	c.deviceID = "dev-1"

	h.routeUserTurn(c, "conv-1", "ag", "hello world", nil, "env-1", 1)

	if fa.env == nil {
		t.Fatal("no envelope enqueued")
	}
	if fa.env.Text != "hello world" {
		t.Errorf("env.Text = %q", fa.env.Text)
	}
	if fa.env.UserID != "dev-1" {
		t.Errorf("env.UserID = %q, want dev-1 (deviceID)", fa.env.UserID)
	}
	if fa.env.Original != "conv-1" {
		t.Errorf("env.Original = %q, want conv-1", fa.env.Original)
	}
	if fa.env.SessionKey == "" {
		t.Error("env.SessionKey empty — binding not resolved")
	}
	if fa.env.Driver == nil {
		t.Error("env.Driver nil — agent can't stream back")
	}
}

// The first message on a cold binding bypasses the reliability gate (no binding
// exists yet) and creates the binding in routeUserTurn. It must seed the binding's
// dedup set so a copy replayed from the client outbox after a reconnect is dropped
// — otherwise the turn is delivered twice (the image double-send bug).
func TestRouteUserTurn_SeedsDedupForReplay(t *testing.T) {
	h := newTestHub()
	registerFakeAgent(h, "ag")
	c := fakeClient()
	c.hub = h
	c.deviceID = "dev-1"

	// First delivery on a brand-new conversation: creates + seeds the binding.
	h.routeUserTurn(c, "conv-dup", "ag", "hello", nil, "env-A", 1)

	b := h.convForReliability("conv-dup")
	if b == nil {
		t.Fatal("binding not created by first message")
	}
	// A replayed copy carries the same envelope id; the gate must now drop it.
	if b.acceptInbound("env-A", 1) {
		t.Error("replayed first message accepted — dedup set was not seeded")
	}
}

// One socket multiplexes multiple agents' conversations. A turn's agent is the
// conversation's owner: once a binding exists, it stays authoritative even if a
// later frame names the *wrong* agent (e.g. a stale/confused client). Routing by
// the frame's agentId instead would enqueue to helen carrying clutch's session
// key, which the ownership invariant rejects and the turn is silently dropped
// (#906/#907). There is no socket-wide "current agent".
func TestRouteUserTurn_BindingOwnerWinsOverFrameAgent(t *testing.T) {
	h := newTestHub()
	clutch := registerFakeAgent(h, "clutch")
	helen := registerFakeAgent(h, "helen")

	// 1) conv-x is created owned by clutch (the frame names clutch).
	c := fakeClient()
	c.hub = h
	c.deviceID = "dev-1"
	h.routeUserTurn(c, "conv-x", "clutch", "first", nil, "env-1", 1)
	if clutch.env == nil {
		t.Fatal("clutch did not receive the first turn")
	}
	ownerKey := clutch.env.SessionKey

	// 2) a later turn for conv-x wrongly carries agentId=helen. It must still land
	// on clutch (the binding owner), not helen, and must not be dropped.
	clutch.env, helen.env = nil, nil
	h.routeUserTurn(c, "conv-x", "helen", "second", nil, "env-2", 2)

	if helen.env != nil {
		t.Fatalf("turn wrongly routed to helen (frame agentId) — sk=%q", helen.env.SessionKey)
	}
	if clutch.env == nil {
		t.Fatal("turn was dropped — not routed to the conversation owner")
	}
	if clutch.env.Text != "second" || clutch.env.SessionKey != ownerKey {
		t.Errorf("clutch env = {Text:%q SessionKey:%q}, want {second %q}", clutch.env.Text, clutch.env.SessionKey, ownerKey)
	}
}

func TestRouteUserTurn_NoAgentEmitsError(t *testing.T) {
	h := newTestHub()
	c := fakeClient()
	c.hub = h

	h.routeUserTurn(c, "conv-1", "ghost", "hi", nil, "env-1", 1) // agent not registered

	got := drain(t, c)
	if len(got) != 1 || got[0].t != fap.TypeError {
		t.Fatalf("frames = %v, want [error]", types(got))
	}
	if got[0].d["code"] != "no_agent" {
		t.Errorf("error code = %v, want no_agent", got[0].d["code"])
	}
}

func TestRouteUserTurn_EmptyContentNoOp(t *testing.T) {
	h := newTestHub()
	fa := registerFakeAgent(h, "ag")
	c := fakeClient()
	c.hub = h
	h.routeUserTurn(c, "conv-1", "ag", "", nil, "env-1", 1)
	if fa.env != nil {
		t.Error("empty text + no attachments must not enqueue")
	}
}

func TestDispatchCommand_EmitsResponseParts(t *testing.T) {
	h := newTestHub()
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "ping",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Parts: []string{"one", "two"}}, nil
		},
	})
	conn := &appConn{hub: h, agentID: "ag", commands: reg}
	b := &convBinding{convID: "c1", sessionKey: "ag/c1/9", client: fakeClientFor(h)}
	h.dispatchCommand(conn, b, command.Request{Name: "ping"})

	got := drain(t, b.client)
	if len(got) != 2 || got[0].d["text"] != "one" || got[1].d["text"] != "two" {
		t.Fatalf("command parts = %v", types(got))
	}
	if got[0].d["role"] != "system" {
		t.Errorf("command output role = %v, want system", got[0].d["role"])
	}
}

func TestDispatchCommand_UnknownCommand(t *testing.T) {
	h := newTestHub()
	conn := &appConn{hub: h, agentID: "ag", commands: command.NewRegistry()}
	b := &convBinding{convID: "c1", client: fakeClientFor(h)}
	h.dispatchCommand(conn, b, command.Request{Name: "nope"})
	got := drain(t, b.client)
	if len(got) != 1 || !strings.Contains(strings.ToLower(got[0].d["text"].(string)), "unknown command") {
		t.Fatalf("want unknown-command message, got %v", got)
	}
}

func fakeClientFor(h *Hub) *wsClient {
	c := fakeClient()
	c.hub = h
	return c
}

func TestClientHello_RepliesRosterRegistersTokenAndResumes(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag")
	c := fakeClientFor(h)

	// Pre-existing durable conversation with buffered frames seq 1..3.
	b := &convBinding{convID: "c1", agentID: "ag", sessionKey: "ag/c1/9", client: c, seen: map[string]struct{}{}}
	h.convs["c1"] = b
	for i := 0; i < 3; i++ {
		b.send(fap.Typing{ConversationID: "c1", On: true})
	}
	drainEnv(t, c) // clear the live sends

	hello := `{"t":"hello","id":"x","d":{"client":{"deviceId":"d1"},"pushToken":"ptok","resume":[{"conversationId":"c1","ack":2}]}}`
	h.dispatchInbound(c, []byte(hello))

	// Token registered.
	if got := h.tokens.all(); len(got) != 1 || got[0] != "ptok" {
		t.Errorf("push token not registered on hello: %v", got)
	}
	// First reply is the hello/roster; then seq 3 is replayed (seq > ack 2).
	frames := drainEnv(t, c)
	if len(frames) == 0 || frames[0].t != fap.TypeHello {
		t.Fatalf("first frame = %v, want hello", frames)
	}
	var replayed bool
	for _, f := range frames {
		if f.t == fap.TypeTyping && f.seq == 3 {
			replayed = true
		}
	}
	if !replayed {
		t.Errorf("seq 3 not replayed after resume ack=2; frames=%+v", frames)
	}
}

func TestCheckOrigin(t *testing.T) {
	cases := []struct {
		origin string
		want   bool
	}{
		{"", true}, // native client sends no Origin
		{"http://localhost", true},
		{"https://evil.example.com", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "http://localhost/app/ws", nil)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if got := checkOrigin(r); got != tc.want {
			t.Errorf("checkOrigin(%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}
}

func TestEnqueueOverflow_DropsWithoutBlocking(t *testing.T) {
	c := &wsClient{send: make(chan []byte, 1), done: make(chan struct{})}
	// Fill the buffer, then overflow — enqueue must not block or panic.
	c.enqueue("a")
	c.enqueue("b") // dropped (buffer full)
	if len(c.send) != 1 {
		t.Errorf("send depth = %d, want 1 (overflow dropped)", len(c.send))
	}
}

func TestFCMSend_PostsDataMessage(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &fcmPusher{
		projectID: "proj",
		ts:        oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "tok"}),
		http:      srv.Client(),
		baseURL:   srv.URL,
		ctx:       context.Background(),
	}
	p.send("device-token", "conv-1", "a preview")

	msg, _ := gotBody["message"].(map[string]any)
	if msg == nil || msg["token"] != "device-token" {
		t.Fatalf("fcm message = %v", gotBody)
	}
	data, _ := msg["data"].(map[string]any)
	if data["conversationId"] != "conv-1" || data["preview"] != "a preview" {
		t.Errorf("fcm data = %v", data)
	}
}

func TestUpdateChatSessionKey_RepointsAndEmits(t *testing.T) {
	h := newTestHub()
	c := fakeClientFor(h)
	b := &convBinding{convID: "c1", agentID: "ag", chatID: 7, sessionKey: "ag/7/old", client: c, seen: map[string]struct{}{}}
	h.bySession["ag/7/old"] = b

	conn := &appConn{hub: h, agentID: "ag"}
	conn.UpdateChatSessionKey(7, "ag/7/new")

	h.mu.RLock()
	_, oldGone := h.bySession["ag/7/old"]
	nb, newThere := h.bySession["ag/7/new"]
	h.mu.RUnlock()
	if oldGone || !newThere || nb != b {
		t.Fatalf("bySession not repointed old→new")
	}
	got := drain(t, c)
	if len(got) != 1 || got[0].t != fap.TypeSessionUpdate || got[0].d["reason"] != "rotated" {
		t.Errorf("want session.update{reason:rotated}, got %v", got)
	}
}

func TestBlobReaper_EvictsExpired(t *testing.T) {
	s := newBlobStore()
	s.ttl = time.Hour
	meta, err := s.putBytes([]byte("data"), "document", "f.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	// Backdate the blob past the TTL, then reap.
	s.mu.Lock()
	s.blobs[meta.id].created = time.Now().Add(-2 * time.Hour)
	s.mu.Unlock()

	s.reap()

	if _, ok := s.get(meta.id); ok {
		t.Error("expired blob should be evicted from metadata")
	}
}

func TestServeDevices_ListsWithoutTokens(t *testing.T) {
	h := newTestHub()
	d := h.devices.pair("dev-1", "Phone")

	req := httptest.NewRequest(http.MethodGet, "/app/devices", nil)
	req.Header.Set("Authorization", "Bearer "+d.Token)
	w := httptest.NewRecorder()
	h.ServeDevices(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("devices code = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "dev-1") || !strings.Contains(body, "Phone") {
		t.Errorf("devices listing missing entry: %s", body)
	}
	if strings.Contains(body, "\"token\"") {
		t.Errorf("devices listing must omit tokens: %s", body)
	}
}
