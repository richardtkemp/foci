package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"foci/internal/agent"
	"foci/internal/app/fap"
	"foci/internal/command"
	"foci/internal/platform"
)

// fakeAgent is a minimal agentCore that captures the enqueued envelope.
type fakeAgent struct {
	env *agent.Envelope
}

func (f *fakeAgent) Enqueue(e agent.Envelope) bool                { f.env = &e; return true }
func (f *fakeAgent) MetaStatus(string) string                     { return "" }
func (f *fakeAgent) CacheExpiryMs(_ string, at time.Time) int64 {
	return at.Add(5 * time.Minute).UnixMilli()
}
func (f *fakeAgent) TransformMessage(t string) string             { return t }

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

	h.routeUserTurn(c, "conv-1", "ag", "hello world", nil, "env-1", 1, agent.SteerDefault, false)

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
	if fa.env.Voice {
		t.Error("env.Voice = true for a normal typed message, want false")
	}
}

// A voice attachment that STT actually transcribes must tag the enqueued
// envelope Voice=true, so RunTurn tags the turn's trigger "voice" instead of
// the app's default platform trigger — the [meta] via= header then reads
// "voice" instead of "app" (#1436).
func TestRouteUserTurn_VoiceTranscribedEnvelopeTaggedVoice(t *testing.T) {
	h := newTestHub()
	fa := registerFakeAgent(h, "ag")
	h.agents["ag"].stt = fakeSTT{text: "hello world"}
	c := fakeClientFor(h)
	c.deviceID = "dev-1"

	voiceAtt := []platform.Attachment{{Type: fap.MediaVoice, Data: []byte("audio"), MimeType: "audio/mp4"}}
	h.routeUserTurn(c, "conv-1", "ag", "", voiceAtt, "env-1", 1, agent.SteerDefault, false)

	if fa.env == nil {
		t.Fatal("no envelope enqueued")
	}
	if !fa.env.Voice {
		t.Error("env.Voice = false, want true (text came from a transcribed voice attachment)")
	}
	if fa.env.Text != "hello world" {
		t.Errorf("env.Text = %q, want the transcript", fa.env.Text)
	}
}

// A failed transcription falls back to keeping the raw audio attachment — the
// turn's content did NOT come from a transcript, so it must not be tagged voice.
func TestRouteUserTurn_FailedTranscriptionNotTaggedVoice(t *testing.T) {
	h := newTestHub()
	fa := registerFakeAgent(h, "ag")
	h.agents["ag"].stt = fakeSTT{err: errors.New("boom")}
	c := fakeClientFor(h)
	c.deviceID = "dev-1"

	voiceAtt := []platform.Attachment{{Type: fap.MediaVoice, Data: []byte("audio"), MimeType: "audio/mp4"}}
	h.routeUserTurn(c, "conv-1", "ag", "", voiceAtt, "env-1", 1, agent.SteerDefault, false)

	if fa.env == nil {
		t.Fatal("no envelope enqueued")
	}
	if fa.env.Voice {
		t.Error("env.Voice = true, want false (transcription failed, audio attachment kept as-is)")
	}
}

// TranscribeOnly returns the voice transcript to the app (as a Transcript frame)
// and does NOT enqueue a turn — the user edits the text before sending (#1029).
func TestRouteUserTurn_TranscribeOnlyReturnsTranscript(t *testing.T) {
	h := newTestHub()
	fa := registerFakeAgent(h, "ag")
	h.agents["ag"].stt = fakeSTT{text: "hello world"}
	c := fakeClientFor(h)
	c.deviceID = "dev-1"

	voice := []platform.Attachment{{Type: fap.MediaVoice, Data: []byte("audio"), MimeType: "audio/mp4"}}
	h.routeUserTurn(c, "conv-1", "ag", "", voice, "env-1", 1, agent.SteerDefault, true)

	if fa.env != nil {
		t.Fatalf("transcribe-only must not enqueue a turn (got text=%q)", fa.env.Text)
	}
	got := drain(t, c)
	if len(got) != 1 || got[0].t != fap.TypeTranscript {
		t.Fatalf("want one transcript frame, got %v", got)
	}
	if got[0].d["text"] != "hello world" {
		t.Errorf("transcript text = %v, want \"hello world\"", got[0].d["text"])
	}
}

// A plain user message must be echoed back as a durable user-role frame so it
// persists to the replay store and a freshly-paired device can restore it — with
// MessageID = the inbound envelope id so the sending device reconciles its
// optimistic copy instead of double-rendering.
func TestRouteUserTurn_EchoesUserMessage(t *testing.T) {
	h := newTestHub()
	registerFakeAgent(h, "ag")
	c := fakeClientFor(h)
	c.deviceID = "dev-1"

	h.routeUserTurn(c, "conv-1", "ag", "hello world", nil, "env-1", 1, agent.SteerDefault, false)

	got := drain(t, c)
	if len(got) == 0 {
		t.Fatal("user message was not echoed as a server frame")
	}
	if got[0].d["role"] != "user" || got[0].d["text"] != "hello world" {
		t.Errorf("echo = %v, want role=user text=\"hello world\"", got[0])
	}
	if got[0].d["messageId"] != "env-1" {
		t.Errorf("echo messageId = %v, want the inbound envelope id env-1", got[0].d["messageId"])
	}
}

// Slash commands typed as messages ("/ping") must be intercepted and routed
// to the command dispatch path — not enqueued as an agent turn (which would
// fire a typing indicator and send the text to the LLM). Also covers dot-alias
// commands (".ping") and the file-path guard ("/home/foci/x" must NOT match).
func TestRouteUserTurn_SlashCommandIntercepted(t *testing.T) {
	makeHub := func() (*Hub, *fakeAgent) {
		h := newTestHub()
		fa := registerFakeAgent(h, "ag")
		reg := command.NewRegistry()
		reg.Register(&command.Command{
			Name: "ping",
			Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
				return command.Response{Text: "pong"}, nil
			},
		})
		h.agents["ag"].commands = reg
		return h, fa
	}

	t.Run("slash prefix", func(t *testing.T) {
		h, fa := makeHub()
		c := fakeClient()
		c.hub = h
		c.deviceID = "dev-1"
		h.routeUserTurn(c, "conv-1", "ag", "/ping", nil, "env-1", 1, agent.SteerDefault, false)
		if fa.env != nil {
			t.Fatalf("slash command was enqueued as agent turn (text=%q) — should have been intercepted", fa.env.Text)
		}
	})

	t.Run("dot prefix", func(t *testing.T) {
		h, fa := makeHub()
		c := fakeClient()
		c.hub = h
		c.deviceID = "dev-1"
		h.routeUserTurn(c, "conv-1", "ag", ".ping", nil, "env-1", 1, agent.SteerDefault, false)
		if fa.env != nil {
			t.Fatalf("dot command was enqueued as agent turn (text=%q) — should have been intercepted", fa.env.Text)
		}
	})

	t.Run("slash path not intercepted", func(t *testing.T) {
		h, fa := makeHub()
		c := fakeClient()
		c.hub = h
		c.deviceID = "dev-1"
		h.routeUserTurn(c, "conv-1", "ag", "/home/foci/x is broken", nil, "env-1", 1, agent.SteerDefault, false)
		if fa.env == nil {
			t.Fatal("file path was intercepted as a command — should reach the agent as normal text")
		}
		if fa.env.Text != "/home/foci/x is broken" {
			t.Errorf("env.Text = %q, want original path text", fa.env.Text)
		}
	})

	t.Run("unknown dot not intercepted", func(t *testing.T) {
		h, fa := makeHub()
		c := fakeClient()
		c.hub = h
		c.deviceID = "dev-1"
		h.routeUserTurn(c, "conv-1", "ag", ".sigh", nil, "env-1", 1, agent.SteerDefault, false)
		if fa.env == nil {
			t.Fatal("unknown .cmd was intercepted — should reach the agent as normal text")
		}
		if fa.env.Text != ".sigh" {
			t.Errorf("env.Text = %q, want .sigh", fa.env.Text)
		}
	})
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
	h.routeUserTurn(c, "conv-dup", "ag", "hello", nil, "env-A", 1, agent.SteerDefault, false)

	b := h.convForReliability("conv-dup")
	if b == nil {
		t.Fatal("binding not created by first message")
	}
	// A replayed copy carries the same envelope id; the gate must now drop it.
	if b.acceptInbound(c, "env-A", 1) {
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
	h.routeUserTurn(c, "conv-x", "clutch", "first", nil, "env-1", 1, agent.SteerDefault, false)
	if clutch.env == nil {
		t.Fatal("clutch did not receive the first turn")
	}
	ownerKey := clutch.env.SessionKey

	// 2) a later turn for conv-x wrongly carries agentId=helen. It must still land
	// on clutch (the binding owner), not helen, and must not be dropped.
	clutch.env, helen.env = nil, nil
	h.routeUserTurn(c, "conv-x", "helen", "second", nil, "env-2", 2, agent.SteerDefault, false)

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

	h.routeUserTurn(c, "conv-1", "ghost", "hi", nil, "env-1", 1, agent.SteerDefault, false) // agent not registered

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
	h.routeUserTurn(c, "conv-1", "ag", "", nil, "env-1", 1, agent.SteerDefault, false)
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
	b := &convBinding{convID: "c1", sessionKey: "ag/c1", clients: map[*wsClient]struct{}{fakeClientFor(h): {}}}
	h.dispatchCommand(nil, conn, b, command.Request{Name: "ping"})

	got := drain(t, b.snapshotClient())
	// 3 frames: user echo "/ping" + two system response parts.
	if len(got) != 3 {
		t.Fatalf("command parts = %v", types(got))
	}
	if got[0].d["text"] != "/ping" || got[0].d["role"] != "user" {
		t.Errorf("first frame = %v, want user echo \"/ping\"", got[0])
	}
	if got[1].d["text"] != "one" || got[2].d["text"] != "two" {
		t.Errorf("response parts = %v %v, want one two", got[1], got[2])
	}
	if got[1].d["role"] != "system" {
		t.Errorf("command output role = %v, want system", got[1].d["role"])
	}
}

func TestDispatchCommand_UnknownCommand(t *testing.T) {
	h := newTestHub()
	conn := &appConn{hub: h, agentID: "ag", commands: command.NewRegistry()}
	b := &convBinding{convID: "c1", clients: map[*wsClient]struct{}{fakeClientFor(h): {}}}
	h.dispatchCommand(nil, conn, b, command.Request{Name: "nope"})
	got := drain(t, b.snapshotClient())
	// 2 frames: user echo "/nope" + the unknown-command system message.
	if len(got) != 2 {
		t.Fatalf("want 2 frames (user echo + unknown), got %v", types(got))
	}
	if got[0].d["text"] != "/nope" || got[0].d["role"] != "user" {
		t.Errorf("first frame = %v, want user echo \"/nope\"", got[0])
	}
	if !strings.Contains(strings.ToLower(got[1].d["text"].(string)), "unknown command") {
		t.Errorf("second frame = %v, want unknown-command message", got[1])
	}
}

func TestDispatchCommand_EchoesUserCommandWithArgs(t *testing.T) {
	h := newTestHub()
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "plan",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{Text: "Planning…"}, nil
		},
	})
	conn := &appConn{hub: h, agentID: "ag", commands: reg}
	b := &convBinding{convID: "c1", sessionKey: "ag/c1", clients: map[*wsClient]struct{}{fakeClientFor(h): {}}}
	h.dispatchCommand(nil, conn, b, command.Request{Name: "plan", Args: "paint a room"})

	got := drain(t, b.snapshotClient())
	if len(got) != 2 {
		t.Fatalf("want 2 frames (user echo + response), got %v", types(got))
	}
	if got[0].d["role"] != "user" || got[0].d["text"] != "/plan paint a room" {
		t.Errorf("user echo = %v, want role=user text=\"/plan paint a room\"", got[0])
	}
	if got[1].d["role"] != "system" || got[1].d["text"] != "Planning…" {
		t.Errorf("response = %v, want role=system text=\"Planning…\"", got[1])
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
	b := &convBinding{convID: "c1", agentID: "ag", sessionKey: "ag/c1", clients: map[*wsClient]struct{}{c: {}}, seen: map[string]struct{}{}}
	h.convs["c1"] = b
	for i := 0; i < 3; i++ {
		b.send(fap.Activity{ConversationID: "c1", Kind: "typing"})
	}
	drainEnv(t, c) // clear the live sends

	hello := `{"t":"hello","id":"x","d":{"client":{"deviceId":"d1"},"pushToken":"ptok","resume":[{"conversationId":"c1","ack":2}]}}`
	h.dispatchInbound(c, []byte(hello))

	// Token registered.
	if got := h.tokens.tokensExcluding(nil); len(got) != 1 || got[0] != "ptok" {
		t.Errorf("push token not registered on hello: %v", got)
	}
	// First reply is the hello/roster; then seq 3 is replayed (seq > ack 2).
	frames := drainEnv(t, c)
	if len(frames) == 0 || frames[0].t != fap.TypeHello {
		t.Fatalf("first frame = %v, want hello", frames)
	}
	var replayed bool
	for _, f := range frames {
		if f.t == fap.TypeActivity && f.seq == 3 {
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
	p.send("device-token", pushPayload{ConvID: "conv-1", Preview: "a preview"})

	msg, _ := gotBody["message"].(map[string]any)
	if msg == nil || msg["token"] != "device-token" {
		t.Fatalf("fcm message = %v", gotBody)
	}
	data, _ := msg["data"].(map[string]any)
	if data["conversationId"] != "conv-1" || data["preview"] != "a preview" {
		t.Errorf("fcm data = %v", data)
	}
}

func TestFCMSend_RetriesTransientThenSucceeds(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable) // transient 5xx → retried
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &fcmPusher{
		projectID: "proj",
		ts:        oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "tok"}),
		http:      srv.Client(),
		baseURL:   srv.URL,
		ctx:       context.Background(),
		retryBase: time.Millisecond,
	}
	p.send("device-token", pushPayload{ConvID: "conv-1"})

	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3 (two 503s then success)", got)
	}
}

func TestFCMSend_DeadTokenPrunedNotRetried(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusNotFound) // dead token → permanent, prune, no retry
	}))
	defer srv.Close()

	tokens := newPushTokens()
	tokens.set("dev-1", "dead-token")
	p := &fcmPusher{
		projectID: "proj",
		ts:        oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "tok"}),
		http:      srv.Client(),
		baseURL:   srv.URL,
		ctx:       context.Background(),
		tokens:    tokens,
		retryBase: time.Millisecond,
	}
	p.send("dead-token", pushPayload{ConvID: "conv-1"})

	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (404 is permanent)", got)
	}
	if len(tokens.tokensExcluding(nil)) != 0 {
		t.Errorf("dead token not pruned: %v", tokens.tokensExcluding(nil))
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

// --- lifecycle callbacks (regression for the "reflection never fires on app
// sessions" bug: the app provider's SetLifecycleCallback used to be a no-op, so
// the periodic runner's lastInteraction stayed frozen at boot and reflection /
// consolidation perpetually skipped with "idle > interval"). ---

// TestRouteUserTurn_FiresOnUserMessage asserts an inbound user message records
// interaction via the OnUserMessage hook — the only signal the periodic runner
// has that an app-delivered agent is active.
func TestRouteUserTurn_FiresOnUserMessage(t *testing.T) {
	h := newTestHub()
	registerFakeAgent(h, "ag")
	c := fakeClient()
	c.hub = h
	c.deviceID = "dev-1"

	conn := h.PrimaryBot("ag")
	fired := false
	conn.OnUserMessage = func() { fired = true }

	h.routeUserTurn(c, "conv-1", "ag", "hello world", nil, "env-1", 1, agent.SteerDefault, false)
	if !fired {
		t.Fatal("OnUserMessage did not fire on inbound user message")
	}
}

// TestRouteUserTurn_EmptyMessageDoesNotFire guards against recording
// interaction for a no-op turn (empty text + no attachments, or a voice note
// that transcribes to nothing).
func TestRouteUserTurn_EmptyMessageDoesNotFire(t *testing.T) {
	h := newTestHub()
	registerFakeAgent(h, "ag")
	c := fakeClient()
	c.hub = h

	conn := h.PrimaryBot("ag")
	fired := false
	conn.OnUserMessage = func() { fired = true }

	h.routeUserTurn(c, "conv-1", "ag", "", nil, "env-1", 1, agent.SteerDefault, false)
	if fired {
		t.Fatal("OnUserMessage must not fire for an empty message")
	}
}

// TestRouteCommand_FiresOnUserMessage asserts a slash-command also records
// interaction — a command is user activity, and the reset-idle-guard must see
// the user as active right after a command, not "idle since boot".
func TestRouteCommand_FiresOnUserMessage(t *testing.T) {
	h := newTestHub()
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "ping",
		Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
			return command.Response{Parts: []string{"pong"}}, nil
		},
	})
	h.agents["ag"] = &appConn{hub: h, agentID: "ag", commands: reg}
	c := fakeClient()
	c.hub = h

	conn := h.PrimaryBot("ag")
	fired := false
	conn.OnUserMessage = func() { fired = true }

	h.routeCommand(c, fap.Command{ConversationID: "c1", AgentID: "ag", Name: "ping"})
	if !fired {
		t.Fatal("OnUserMessage did not fire on inbound command")
	}
}

// TestWrapTurn_ErrorPassthrough asserts appConn.WrapTurn is a straight
// pass-through: it returns the turn body's error unchanged. The session/
// keepalive hooks that used to fire here moved to Agent.HandleMessage.
func TestWrapTurn_ErrorPassthrough(t *testing.T) {
	conn := &appConn{}
	turnErr := errors.New("boom")
	if got := conn.WrapTurn(context.Background(), func() error { return turnErr }); got != turnErr {
		t.Errorf("WrapTurn returned %v, want the original error passthrough", got)
	}
	if err := conn.WrapTurn(context.Background(), func() error { return nil }); err != nil {
		t.Fatalf("nil-error WrapTurn errored: %v", err)
	}
}

// TestSetLifecycleCallback_StoresOnConn verifies the provider stores the
// OnUserMessage callback on the per-agent appConn (the sole remaining platform
// lifecycle event — turn-boundary hooks moved to the Agent).
func TestSetLifecycleCallback_StoresOnConn(t *testing.T) {
	h := newTestHub()
	h.agents["ag"] = &appConn{hub: h, agentID: "ag"}
	p := &appProvider{hub: h}

	marker := false
	p.SetLifecycleCallback("ag", platform.OnUserMessage, func() { marker = true })
	conn := h.PrimaryBot("ag")
	if conn.OnUserMessage == nil {
		t.Fatal("OnUserMessage not stored")
	}
	conn.OnUserMessage()
	if !marker {
		t.Error("stored OnUserMessage callback did not fire")
	}
}

// TestSetLifecycleCallback_UnknownAgentNoOp ensures a missing agent connection
// is a silent no-op (not a panic) — mirrors telegram's PrimaryBot-nil guard.
func TestSetLifecycleCallback_UnknownAgentNoOp(t *testing.T) {
	h := newTestHub()
	p := &appProvider{hub: h}
	// Unknown agent, nil hub, and a known-but-callbackless path must all be safe.
	p.SetLifecycleCallback("ghost", platform.OnUserMessage, func() {})

	pNil := &appProvider{} // nil hub (Init panicked)
	pNil.SetLifecycleCallback("ag", platform.OnUserMessage, func() {})
}
