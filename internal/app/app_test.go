package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/app/fap"
	"foci/internal/command"
	"foci/internal/platform"
	"foci/internal/session"
)

// fakeClient is a wsClient whose send channel we drain in tests.
func fakeClient() *wsClient {
	return &wsClient{
		send:     make(chan []byte, 64),
		done:     make(chan struct{}),
		convByID: make(map[string]*convBinding),
	}
}

// drain decodes every queued wire frame into (t, payload-map) pairs.
func drain(t *testing.T, c *wsClient) []decoded {
	t.Helper()
	var out []decoded
	for {
		select {
		case b := <-c.send:
			var env struct {
				T string          `json:"t"`
				D json.RawMessage `json:"d"`
			}
			if err := json.Unmarshal(b, &env); err != nil {
				t.Fatalf("bad wire frame: %v", err)
			}
			d := map[string]any{}
			if len(env.D) > 0 {
				_ = json.Unmarshal(env.D, &d)
			}
			out = append(out, decoded{t: env.T, d: d})
		default:
			return out
		}
	}
}

type decoded struct {
	t string
	d map[string]any
}

func types(ds []decoded) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.t
	}
	return out
}

func TestChatIDForConv_DeterministicAndPositive(t *testing.T) {
	a := chatIDForConv("conv-abc")
	b := chatIDForConv("conv-abc")
	c := chatIDForConv("conv-xyz")
	if a != b {
		t.Errorf("not deterministic: %d != %d", a, b)
	}
	if a == c {
		t.Errorf("distinct conversations collided: %d", a)
	}
	if a <= 0 {
		t.Errorf("chatID must be positive, got %d", a)
	}
}

func TestAppSink_StreamingTranslation(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", client: c}
	s := newAppSink(b)
	ctx := context.Background()

	s.Emit(ctx, turnevent.TurnStart{})
	s.Emit(ctx, turnevent.TextDelta{Delta: "Hel"})
	s.Emit(ctx, turnevent.TextDelta{Delta: "lo"})
	s.Emit(ctx, turnevent.TurnComplete{FinalText: "Hello"})

	got := drain(t, c)
	want := []string{
		fap.TypeTyping,    // TurnStart → typing on
		fap.TypeTurnStart, // first delta lazily opens the turn
		fap.TypeTextDelta,
		fap.TypeTextDelta,
		fap.TypeTextEnd, // TurnComplete finalizes the streamed message
		fap.TypeTyping,  // typing off
	}
	if g := types(got); !equal(g, want) {
		t.Fatalf("frame sequence = %v, want %v", g, want)
	}
	// turn.start and text.end must share the turnId.
	if got[1].d["turnId"] != got[4].d["turnId"] {
		t.Errorf("turnId mismatch between turn.start and text.end")
	}
	if got[4].d["finalText"] != "Hello" {
		t.Errorf("text.end finalText = %v, want Hello", got[4].d["finalText"])
	}
}

func TestAppSink_NonStreamedFinalText(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", client: c}
	s := newAppSink(b)
	// No deltas — a single complete reply.
	s.Emit(context.Background(), turnevent.TurnComplete{FinalText: "done"})

	got := drain(t, c)
	if w := types(got); !equal(w, []string{fap.TypeMessage, fap.TypeTyping}) {
		t.Fatalf("frames = %v, want [message typing]", w)
	}
	if got[0].d["text"] != "done" || got[0].d["role"] != "agent" {
		t.Errorf("message payload wrong: %v", got[0].d)
	}
}

func TestAppSink_SilentFinalSuppressed(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", client: c}
	s := newAppSink(b)
	s.Emit(context.Background(), turnevent.TurnComplete{FinalText: "[[NO_RESPONSE]]"})

	got := drain(t, c)
	// Only the typing-off frame; no message for a silent turn.
	if w := types(got); !equal(w, []string{fap.TypeTyping}) {
		t.Fatalf("frames = %v, want just [typing] (silent suppressed)", w)
	}
}

func newTestHub() *Hub {
	return &Hub{
		deps:      platform.ProviderDeps{},
		blobs:     newBlobStore(),
		tokens:    newPushTokens(),
		agents:    make(map[string]*appConn),
		convs:     make(map[string]*convBinding),
		bySession: make(map[string]*convBinding),
		clients:   make(map[*wsClient]struct{}),
		prompts:   make(map[string]*convBinding),
	}
}

func TestSendToSession_EmitsMessageFrame(t *testing.T) {
	h := newTestHub()
	c := fakeClient()
	b := &convBinding{convID: "c1", sessionKey: "ag/capp1/9", client: c}
	h.bySession[b.sessionKey] = b

	conn := &appConn{hub: h, agentID: "ag"}
	if err := conn.SendToSession(b.sessionKey, "hi there"); err != nil {
		t.Fatal(err)
	}
	got := drain(t, c)
	if len(got) != 1 || got[0].t != fap.TypeMessage {
		t.Fatalf("frames = %v, want one message", types(got))
	}
	if got[0].d["text"] != "hi there" {
		t.Errorf("text = %v", got[0].d["text"])
	}
}

func TestSendToSession_SilentSuppressed(t *testing.T) {
	h := newTestHub()
	c := fakeClient()
	b := &convBinding{convID: "c1", sessionKey: "ag/capp1/9", client: c}
	h.bySession[b.sessionKey] = b

	conn := &appConn{hub: h, agentID: "ag"}
	_ = conn.SendToSession(b.sessionKey, "  [[NO_RESPONSE]]  ")
	if got := drain(t, c); len(got) != 0 {
		t.Fatalf("silent send must emit nothing, got %v", types(got))
	}
}

func TestDispatchPing_RepliesPong(t *testing.T) {
	h := newTestHub()
	c := fakeClient()
	c.hub = h
	ping := `{"t":"ping","id":"x"}`
	h.dispatchInbound(c, []byte(ping))
	got := drain(t, c)
	if len(got) != 1 || got[0].t != fap.TypePong {
		t.Fatalf("ping must reply pong, got %v", types(got))
	}
}

// --- slice 2: interactive prompts ---

// boundConn wires a hub + socket + binding for a conversation and returns an
// appConn whose default session is that conversation, plus the binding/socket.
func boundConn(t *testing.T) (*Hub, *wsClient, *convBinding, *appConn) {
	t.Helper()
	h := newTestHub()
	c := fakeClient()
	c.hub = h
	b := &convBinding{convID: "c1", sessionKey: "ag/capp1/9", client: c, chatID: 7}
	h.bySession[b.sessionKey] = b
	c.convByID["c1"] = b
	conn := &appConn{hub: h, agentID: "ag", defaultSession: b.sessionKey}
	return h, c, b, conn
}

func TestPromptIDFromButtons(t *testing.T) {
	id := promptIDFromButtons([]platform.ButtonChoice{{Label: "Allow", Data: "req-9:0"}, {Label: "Deny", Data: "req-9:1"}})
	if id != "req-9" {
		t.Errorf("promptID = %q, want req-9", id)
	}
	// No buttons → a fresh ULID (non-empty), never a panic.
	if got := promptIDFromButtons(nil); len(got) != 26 {
		t.Errorf("empty buttons should yield a ULID, got %q", got)
	}
}

func TestSendTextWithButtons_EmitsInteractive(t *testing.T) {
	h, c, _, conn := boundConn(t)
	msgID, err := conn.SendTextWithButtons("Allow Bash?",
		[]platform.ButtonChoice{{Label: "Allow", Data: "req-1:0"}, {Label: "Deny", Data: "req-1:1"}}, "im:")
	if err != nil {
		t.Fatal(err)
	}
	if msgID != "req-1" {
		t.Errorf("msgID = %q, want req-1 (the promptID)", msgID)
	}
	if h.bindingForPrompt("req-1") == nil {
		t.Errorf("prompt not registered")
	}
	ds := drain(t, c)
	if len(ds) != 1 || ds[0].t != fap.TypeInteractive {
		t.Fatalf("frames = %v, want [interactive]", types(ds))
	}
	if ds[0].d["promptId"] != "req-1" {
		t.Errorf("promptId = %v", ds[0].d["promptId"])
	}
	choices, _ := ds[0].d["choices"].([]any)
	if len(choices) != 2 {
		t.Fatalf("choices = %v, want 2", ds[0].d["choices"])
	}
	if first, _ := choices[0].(map[string]any); first["data"] != "req-1:0" || first["label"] != "Allow" {
		t.Errorf("choice[0] = %v", choices[0])
	}
}

func TestSendTextWithButtons_OfflineReturnsErr(t *testing.T) {
	h := newTestHub()
	conn := &appConn{hub: h, agentID: "ag", defaultSession: "ag/capp1/9"} // no binding
	if _, err := conn.SendTextWithButtons("x", []platform.ButtonChoice{{Label: "Allow", Data: "r:0"}}, "im:"); err == nil {
		t.Errorf("offline SendTextWithButtons must return an error")
	}
}

// TestInteractive_ButtonRoundTrip drives the real platform interactive
// machinery: present a permission prompt, simulate the app echoing the chosen
// Choice.Data, and assert the callback fired and a resolution edit was emitted.
func TestInteractive_ButtonRoundTrip(t *testing.T) {
	h, c, _, conn := boundConn(t)
	resolve := func() platform.Connection { return conn }

	var chosen string
	_, err := platform.SendInteractiveMessageWithID(resolve, "req-rt", "Allow Bash?",
		[]platform.ButtonChoice{{Label: "Allow", Data: "allow"}, {Label: "Deny", Data: "deny"}},
		func(choice platform.ButtonChoice) string { chosen = choice.Data; return "✅ Approved" }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ds := drain(t, c); len(ds) != 1 || ds[0].t != fap.TypeInteractive {
		t.Fatalf("present frames = %v, want [interactive]", types(ds))
	}

	// User taps Allow → the app echoes Choice.Data ("<promptID>:<index>").
	h.handleInteractiveResponse(c, fap.InteractiveResponse{ConversationID: "c1", PromptID: "req-rt", Data: "req-rt:0"})

	if chosen != "allow" {
		t.Errorf("callback choice = %q, want allow", chosen)
	}
	ds := drain(t, c)
	if len(ds) != 1 || ds[0].t != fap.TypeInteractiveEdit {
		t.Fatalf("after tap frames = %v, want [interactive.edit]", types(ds))
	}
	if ds[0].d["text"] != "✅ Approved" {
		t.Errorf("edit text = %v, want ✅ Approved", ds[0].d["text"])
	}
	if h.bindingForPrompt("req-rt") != nil {
		t.Errorf("registration should be cleared after resolution")
	}
}

// TestInteractive_NextQuestionSuppressesEdit verifies the seq-advance guard: a
// callback that re-renders the conversation (e.g. presenting the next question
// of a multi-question ask) must suppress the resolved-prompt edit so the new
// question is not clobbered.
func TestInteractive_NextQuestionSuppressesEdit(t *testing.T) {
	h, c, b, conn := boundConn(t)
	resolve := func() platform.Connection { return conn }

	_, err := platform.SendInteractiveMessageWithID(resolve, "req-mq", "Q1?",
		[]platform.ButtonChoice{{Label: "A", Data: "qa:0"}, {Label: "B", Data: "qa:1"}},
		func(choice platform.ButtonChoice) string {
			// Simulate presenting the next question: any frame advances seq.
			b.send(fap.Typing{ConversationID: "c1", On: true})
			return "✅ A"
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, c) // consume the present frame

	h.handleInteractiveResponse(c, fap.InteractiveResponse{ConversationID: "c1", PromptID: "req-mq", Data: "req-mq:0"})

	for _, d := range drain(t, c) {
		if d.t == fap.TypeInteractiveEdit {
			t.Errorf("resolved-prompt edit must be suppressed when seq advanced")
		}
	}
	if h.bindingForPrompt("req-mq") == nil {
		t.Errorf("registration must survive a seq-advancing resolution")
	}
}

func TestEditMessageText_EmitsEditAndClears(t *testing.T) {
	h, c, b, conn := boundConn(t)
	h.registerPrompt("p1", b)
	if err := conn.EditMessageText("p1", "❌ cancelled"); err != nil {
		t.Fatal(err)
	}
	ds := drain(t, c)
	if len(ds) != 1 || ds[0].t != fap.TypeInteractiveEdit || ds[0].d["text"] != "❌ cancelled" {
		t.Fatalf("frames = %v / text = %v", types(ds), ds[0].d["text"])
	}
	if h.bindingForPrompt("p1") != nil {
		t.Errorf("registration not cleared")
	}
	// Idempotent: an unknown promptID is a silent no-op.
	if err := conn.EditMessageText("nope", "x"); err != nil {
		t.Fatal(err)
	}
	if got := drain(t, c); len(got) != 0 {
		t.Fatalf("unknown edit must emit nothing, got %v", types(got))
	}
}

func TestRemoveClient_PurgesPrompts(t *testing.T) {
	h, c, b, _ := boundConn(t)
	h.addClient(c)
	h.registerPrompt("p1", b)
	h.removeClient(c)
	if h.bindingForPrompt("p1") != nil {
		t.Errorf("prompt registration should be purged on disconnect")
	}
}

// --- slice 3: reliability (seq / ack / replay / dedup) ---

type envFrame struct {
	t   string
	seq int64
	ack int64
}

// drainEnv decodes queued frames capturing envelope seq/ack (not just payload).
func drainEnv(t *testing.T, c *wsClient) []envFrame {
	t.Helper()
	var out []envFrame
	for {
		select {
		case b := <-c.send:
			var env struct {
				T   string `json:"t"`
				Seq int64  `json:"seq"`
				Ack int64  `json:"ack"`
			}
			if err := json.Unmarshal(b, &env); err != nil {
				t.Fatalf("bad wire frame: %v", err)
			}
			out = append(out, envFrame{t: env.T, seq: env.Seq, ack: env.Ack})
		default:
			return out
		}
	}
}

func TestReliability_SeqSurvivesReconnect(t *testing.T) {
	h := newTestHub()
	c1 := fakeClient()
	c1.hub = h
	b := h.ensureBinding(c1, "ag", "conv-1")
	b.send(fap.Typing{ConversationID: "conv-1", On: true})  // seq 1
	b.send(fap.Typing{ConversationID: "conv-1", On: false}) // seq 2
	drainEnv(t, c1)

	h.removeClient(c1) // disconnect; durable state survives
	c2 := fakeClient()
	c2.hub = h
	b2 := h.ensureBinding(c2, "ag", "conv-1")
	if b2 != b {
		t.Fatalf("reconnect must reuse the durable binding")
	}
	b2.send(fap.Typing{ConversationID: "conv-1", On: true}) // seq must continue at 3
	ds := drainEnv(t, c2)
	if len(ds) != 1 || ds[0].seq != 3 {
		t.Fatalf("post-reconnect frames = %v, want one frame at seq 3", ds)
	}
}

func TestReliability_ReplayOnResume(t *testing.T) {
	h := newTestHub()
	c1 := fakeClient()
	c1.hub = h
	b := h.ensureBinding(c1, "ag", "conv-1")
	for i := 0; i < 3; i++ {
		b.send(fap.Typing{ConversationID: "conv-1", On: true}) // seq 1,2,3
	}
	drainEnv(t, c1)
	h.removeClient(c1)

	// Reconnect: the client rendered up to seq 1, so resume replays 2 and 3.
	c2 := fakeClient()
	c2.hub = h
	h.resumeConversations(c2, []fap.ResumePoint{{ConversationID: "conv-1", Ack: 1}})
	ds := drainEnv(t, c2)
	if len(ds) != 2 || ds[0].seq != 2 || ds[1].seq != 3 {
		t.Fatalf("replay = %v, want frames at seq [2 3]", ds)
	}
}

func TestReliability_OfflineBuffersThenReplays(t *testing.T) {
	h := newTestHub()
	c1 := fakeClient()
	c1.hub = h
	b := h.ensureBinding(c1, "ag", "conv-1")
	h.removeClient(c1) // offline: binding detached but retained

	b.send(fap.ServerMessage{ConversationID: "conv-1", MessageID: "m1", Role: "agent", Text: "queued offline"})

	c2 := fakeClient()
	c2.hub = h
	h.resumeConversations(c2, []fap.ResumePoint{{ConversationID: "conv-1", Ack: 0}})
	ds := drain(t, c2)
	if len(ds) != 1 || ds[0].t != fap.TypeMessage || ds[0].d["text"] != "queued offline" {
		t.Fatalf("offline frame must replay on resume, got %v", types(ds))
	}
}

func TestReliability_InboundDedup(t *testing.T) {
	b := &convBinding{convID: "c1", seen: make(map[string]struct{})}
	if !b.acceptInbound("id-1", 1) {
		t.Fatal("first frame must be accepted")
	}
	if b.acceptInbound("id-1", 1) {
		t.Fatal("duplicate id must be rejected")
	}
	if !b.acceptInbound("id-2", 2) {
		t.Fatal("new id must be accepted")
	}
	if b.clientSeqHW != 2 {
		t.Errorf("clientSeqHW = %d, want 2", b.clientSeqHW)
	}
}

func TestReliability_OutboundAckStampsClientSeq(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", client: c, seen: make(map[string]struct{})}
	b.acceptInbound("u1", 7) // client's outbound seq high-water is 7
	b.send(fap.Typing{ConversationID: "c1", On: true})
	ds := drainEnv(t, c)
	if len(ds) != 1 || ds[0].ack != 7 {
		t.Fatalf("outbound ack = %v, want 7 (client seq high-water)", ds)
	}
}

func TestReliability_AckTrimsBuffer(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", client: c, seen: make(map[string]struct{})}
	for i := 0; i < 5; i++ {
		b.send(fap.Typing{ConversationID: "c1", On: true}) // seq 1..5
	}
	drainEnv(t, c)
	b.ackInbound(3)
	b.mu.Lock()
	n, first := len(b.buffer), b.buffer[0].seq
	b.mu.Unlock()
	if n != 2 || first != 4 {
		t.Errorf("after ack(3): %d frames starting seq %d, want 2 frames from seq 4", n, first)
	}
}

func TestReliability_BufferTrimsByDepth(t *testing.T) {
	c := fakeClient()
	b := &convBinding{convID: "c1", client: c, seen: make(map[string]struct{})}
	for i := 0; i < replayBufferDepth+50; i++ {
		b.send(fap.Typing{ConversationID: "c1", On: true})
	}
	drainEnv(t, c)
	b.mu.Lock()
	n, first := len(b.buffer), b.buffer[0].seq
	b.mu.Unlock()
	if n != replayBufferDepth {
		t.Errorf("buffer depth = %d, want %d", n, replayBufferDepth)
	}
	if first != 51 { // seq 1..50 dropped, 51..1050 retained
		t.Errorf("oldest retained seq = %d, want 51", first)
	}
}

// --- slice 4: media / blobs ---

func TestBlobStore_PutGetRoundTrip(t *testing.T) {
	s := newBlobStore()
	meta, err := s.putBytes([]byte("hello"), "document", "f.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if meta.size != 5 || meta.kind != "document" || meta.mime != "text/plain" {
		t.Errorf("meta = %+v", meta)
	}
	got, ok := s.get(meta.id)
	if !ok || got != meta {
		t.Errorf("get(%q) failed", meta.id)
	}
	data, err := os.ReadFile(meta.path)
	if err != nil || string(data) != "hello" {
		t.Errorf("blob file = %q (%v)", data, err)
	}
}

func TestBlobStore_SizeCap(t *testing.T) {
	s := newBlobStore()
	s.maxBytes = 4
	if _, err := s.put(bytes.NewReader([]byte("12345")), "document", "x", "text/plain"); !errors.Is(err, errBlobTooLarge) {
		t.Fatalf("over-cap put error = %v, want errBlobTooLarge", err)
	}
	if _, err := s.put(bytes.NewReader([]byte("1234")), "document", "x", "text/plain"); err != nil {
		t.Fatalf("at-cap put: %v", err)
	}
}

func TestSendPhoto_StoresBlobAndEmitsMedia(t *testing.T) {
	h, c, _, conn := boundConn(t)
	tmp := filepath.Join(t.TempDir(), "pic.png")
	if err := os.WriteFile(tmp, []byte("PNGDATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := conn.SendPhoto(tmp, "nice pic"); err != nil {
		t.Fatal(err)
	}
	ds := drain(t, c)
	if len(ds) != 1 || ds[0].t != fap.TypeMedia {
		t.Fatalf("frames = %v, want [media]", types(ds))
	}
	d := ds[0].d
	if d["kind"] != "photo" || d["caption"] != "nice pic" {
		t.Errorf("media payload = %v", d)
	}
	blobID, _ := d["blobId"].(string)
	meta, ok := h.blobs.get(blobID)
	if !ok || meta.size != 7 {
		t.Errorf("blob not stored: ok=%v meta=%+v", ok, meta)
	}
}

func TestResolveAttachments_ReadsBlob(t *testing.T) {
	h := newTestHub()
	meta, err := h.blobs.putBytes([]byte("hello"), "document", "f.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	atts := h.resolveAttachments([]fap.AttachmentRef{{BlobID: meta.id, Kind: "document", MIME: "text/plain", Name: "f.txt"}})
	if len(atts) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(atts))
	}
	if string(atts[0].Data) != "hello" || atts[0].MimeType != "text/plain" || atts[0].SavedPath != meta.path {
		t.Errorf("attachment = %+v", atts[0])
	}
	if got := h.resolveAttachments([]fap.AttachmentRef{{BlobID: "nope"}}); len(got) != 0 {
		t.Errorf("unknown blob must be skipped, got %v", got)
	}
}

func TestBlobHTTP_UploadThenDownload(t *testing.T) {
	h := newTestHub()
	h.apiKey = "secret-key"

	up := httptest.NewRequest(http.MethodPost, "/app/blob", strings.NewReader("filedata"))
	up.Header.Set("Authorization", "Bearer secret-key")
	up.Header.Set("Content-Type", "image/png")
	up.Header.Set("X-Filename", "p.png")
	uw := httptest.NewRecorder()
	h.ServeBlobPost(uw, up)
	if uw.Code != http.StatusOK {
		t.Fatalf("upload code = %d", uw.Code)
	}
	var res struct {
		BlobID string `json:"blobId"`
		Size   int64  `json:"size"`
		Mime   string `json:"mime"`
	}
	if err := json.Unmarshal(uw.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.BlobID == "" || res.Size != 8 || res.Mime != "image/png" {
		t.Fatalf("upload result = %+v", res)
	}

	dn := httptest.NewRequest(http.MethodGet, "/app/blob/"+res.BlobID, nil)
	dn.Header.Set("Authorization", "Bearer secret-key")
	dw := httptest.NewRecorder()
	h.ServeBlobGet(dw, dn)
	if dw.Code != http.StatusOK || dw.Body.String() != "filedata" {
		t.Fatalf("download code=%d body=%q", dw.Code, dw.Body.String())
	}
	if ct := dw.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("content-type = %q, want image/png", ct)
	}
}

func TestBlobHTTP_Auth(t *testing.T) {
	h := newTestHub()
	h.apiKey = "secret-key"

	noAuth := httptest.NewRequest(http.MethodPost, "/app/blob", strings.NewReader("x"))
	nw := httptest.NewRecorder()
	h.ServeBlobPost(nw, noAuth)
	if nw.Code != http.StatusUnauthorized {
		t.Fatalf("no-bearer code = %d, want 401", nw.Code)
	}

	wrong := httptest.NewRequest(http.MethodGet, "/app/blob/abc", nil)
	wrong.Header.Set("Authorization", "Bearer nope")
	ww := httptest.NewRecorder()
	h.ServeBlobGet(ww, wrong)
	if ww.Code != http.StatusForbidden {
		t.Fatalf("wrong-key code = %d, want 403", ww.Code)
	}
}

// --- slice 5: push (offline wake) ---

func TestPushTokens_SetAndAll(t *testing.T) {
	p := newPushTokens()
	p.set("dev1", "tokA")
	p.set("dev2", "tokB")
	p.set("", "ignored")
	p.set("dev3", "")
	if all := p.all(); len(all) != 2 {
		t.Fatalf("tokens = %v, want 2 (empty deviceId/token ignored)", all)
	}
}

func TestPushPreview_Classification(t *testing.T) {
	final := "the answer"
	cases := []struct {
		f      fap.ServerFrame
		wantOK bool
		want   string
	}{
		{fap.ServerMessage{Text: "hello"}, true, "hello"},
		{fap.TextEnd{FinalText: &final}, true, "the answer"},
		{fap.TextEnd{}, true, "New message"},
		{fap.Media{Kind: "photo"}, true, "Sent photo"},
		{fap.Media{Kind: "photo", Caption: "cap"}, true, "cap"},
		{fap.Notification{Text: "note"}, true, "note"},
		{fap.Interactive{Text: "approve?"}, true, "approve?"},
		{fap.Typing{On: true}, false, ""},
		{fap.Meta{}, false, ""},
		{fap.TextDelta{Text: "x"}, false, ""},
	}
	for _, c := range cases {
		got, ok := pushPreview(c.f)
		if ok != c.wantOK || got != c.want {
			t.Errorf("pushPreview(%T) = (%q,%v), want (%q,%v)", c.f, got, ok, c.want, c.wantOK)
		}
	}
}

func TestTruncatePreview(t *testing.T) {
	if got := truncatePreview("  hi  "); got != "hi" {
		t.Errorf("trim = %q, want hi", got)
	}
	got := truncatePreview(strings.Repeat("a", 100))
	if !strings.HasSuffix(got, "…") || len(got) != pushPreviewMax+len("…") {
		t.Errorf("truncated len = %d, want %d with ellipsis", len(got), pushPreviewMax+len("…"))
	}
}

func TestOfflineSend_FiresPushForVisibleFramesOnly(t *testing.T) {
	var got []string
	b := &convBinding{
		convID:        "conv-1",
		seen:          make(map[string]struct{}),
		notifyOffline: func(_, preview string) { got = append(got, preview) },
	} // client nil → offline
	b.send(fap.ServerMessage{ConversationID: "conv-1", MessageID: "m", Role: "agent", Text: "hello there"})
	b.send(fap.Typing{ConversationID: "conv-1", On: true}) // control frame → no push
	if len(got) != 1 || got[0] != "hello there" {
		t.Fatalf("offline previews = %v, want [hello there]", got)
	}
}

func TestOnlineSend_NoPush(t *testing.T) {
	c := fakeClient()
	var got []string
	b := &convBinding{
		convID:        "c1",
		client:        c,
		seen:          make(map[string]struct{}),
		notifyOffline: func(_, preview string) { got = append(got, preview) },
	}
	b.send(fap.ServerMessage{ConversationID: "c1", MessageID: "m", Role: "agent", Text: "hi"})
	drain(t, c)
	if len(got) != 0 {
		t.Fatalf("online send must not push, got %v", got)
	}
}

func TestPusher_Coalesces(t *testing.T) {
	p := &fcmPusher{tokens: newPushTokens(), lastPush: make(map[string]time.Time)}
	p.notify("conv-1", "a") // no tokens → no network; updates lastPush
	first := p.lastPush["conv-1"]
	p.notify("conv-1", "b") // within window → coalesced, lastPush unchanged
	if !p.lastPush["conv-1"].Equal(first) {
		t.Errorf("second notify within window must be coalesced")
	}
}

// --- slice 6: multi-agent / session ---

// registerBareAgent registers an appConn without a real *agent.Agent — enough
// for PrimaryBot/roster/binding tests that don't drive a turn.
func registerBareAgent(h *Hub, agentID string) {
	h.agents[agentID] = &appConn{hub: h, agentID: agentID}
	h.agentOrder = append(h.agentOrder, agentID)
}

func TestConversationOpen_MintsAndAdvertises(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag")
	c := fakeClient()
	c.hub = h

	h.dispatchInbound(c, []byte(`{"t":"conversation.open","id":"x","d":{"agentId":"ag"}}`))

	ds := drain(t, c)
	if len(ds) != 1 || ds[0].t != fap.TypeHello {
		t.Fatalf("want [hello] roster reply, got %v", types(ds))
	}
	agents, _ := ds[0].d["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("roster agents = %v", ds[0].d["agents"])
	}
	convs, _ := agents[0].(map[string]any)["conversations"].([]any)
	if len(convs) != 1 {
		t.Fatalf("agent conversations = %v", agents[0])
	}
	conv := convs[0].(map[string]any)
	if conv["id"] == "" || conv["sessionKey"] == "" {
		t.Errorf("conversation info incomplete: %v", conv)
	}
	if len(h.convs) != 1 {
		t.Errorf("expected 1 durable conversation, got %d", len(h.convs))
	}
}

func TestConversationOpen_AdoptsNamedSession(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag")
	c := fakeClient()
	c.hub = h
	sk, err := session.NamedIndependentSessionKey("ag", "work")
	if err != nil {
		t.Fatal(err)
	}

	h.dispatchInbound(c, []byte(`{"t":"conversation.open","id":"x","d":{"agentId":"ag","sessionKey":"`+sk+`"}}`))

	var b *convBinding
	for _, bb := range h.convs {
		b = bb
	}
	if b == nil || b.sessionKey != sk {
		t.Fatalf("adopted sessionKey = %v, want %q", b, sk)
	}
	if h.bySession[sk] != b {
		t.Errorf("bySession not repointed to the named key")
	}
}

func TestAdoptSession_RejectsForeignAgentKey(t *testing.T) {
	h := newTestHub()
	b := &convBinding{convID: "c1", agentID: "ag", sessionKey: "ag/c1/9", seen: map[string]struct{}{}}
	h.convs["c1"] = b
	h.bySession["ag/c1/9"] = b
	h.adoptSession(b, "other/iwork/0") // belongs to a different agent
	if b.sessionKey != "ag/c1/9" {
		t.Errorf("foreign-agent key must be rejected, sessionKey = %q", b.sessionKey)
	}
}

func TestCommandParts(t *testing.T) {
	if got := commandParts(command.Response{Text: "hi"}); len(got) != 1 || got[0] != "hi" {
		t.Errorf("text-only = %v", got)
	}
	if got := commandParts(command.Response{Parts: []string{"a", "b"}}); len(got) != 2 {
		t.Errorf("parts = %v", got)
	}
	if got := commandParts(command.Response{Text: "   "}); got != nil {
		t.Errorf("blank text must yield nil, got %v", got)
	}
}

func TestRouteCommand_NoCommandsErrors(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag") // bare appConn → commands registry nil
	c := fakeClient()
	c.hub = h
	c.agentID = "ag"

	h.routeCommand(c, fap.Command{ConversationID: "c1", Name: "help"})

	ds := drain(t, c)
	if len(ds) != 1 || ds[0].t != fap.TypeError || ds[0].d["code"] != "no_commands" {
		t.Fatalf("want no_commands error, got %v / %v", types(ds), ds)
	}
}

// --- slice 8: voice (inbound STT) ---

type fakeSTT struct {
	text string
	err  error
}

func (f fakeSTT) Transcribe(_ context.Context, _ []byte, _ string) (string, error) {
	return f.text, f.err
}

func TestTranscribeVoice_MergesAndDropsVoiceAttachment(t *testing.T) {
	h := newTestHub()
	conn := &appConn{hub: h, agentID: "ag", stt: fakeSTT{text: "hello world"}}
	atts := []platform.Attachment{
		{Type: "voice", Data: []byte("audio"), MimeType: "audio/ogg"},
		{Type: "document", Data: []byte("doc"), MimeType: "text/plain"},
	}
	text, kept := h.transcribeVoice(conn, "", atts)
	if text != "hello world" {
		t.Errorf("text = %q, want transcript", text)
	}
	if len(kept) != 1 || kept[0].Type != "document" {
		t.Errorf("voice attachment should be dropped, kept = %v", kept)
	}
}

func TestTranscribeVoice_AppendsToTypedText(t *testing.T) {
	h := newTestHub()
	conn := &appConn{hub: h, stt: fakeSTT{text: "spoken"}}
	text, _ := h.transcribeVoice(conn, "typed", []platform.Attachment{{Type: "voice", Data: []byte("a"), MimeType: "audio/ogg"}})
	if text != "typed\nspoken" {
		t.Errorf("text = %q, want typed+spoken", text)
	}
}

func TestTranscribeVoice_NoSTTUnchanged(t *testing.T) {
	h := newTestHub()
	conn := &appConn{hub: h} // no transcriber
	atts := []platform.Attachment{{Type: "voice", Data: []byte("a"), MimeType: "audio/ogg"}}
	text, kept := h.transcribeVoice(conn, "x", atts)
	if text != "x" || len(kept) != 1 {
		t.Errorf("without STT must pass through: %q %v", text, kept)
	}
}

func TestTranscribeVoice_ErrorKeepsAttachment(t *testing.T) {
	h := newTestHub()
	conn := &appConn{hub: h, stt: fakeSTT{err: errors.New("boom")}}
	atts := []platform.Attachment{{Type: "voice", Data: []byte("a"), MimeType: "audio/ogg"}}
	text, kept := h.transcribeVoice(conn, "", atts)
	if text != "" || len(kept) != 1 {
		t.Errorf("transcription error must keep the audio: %q %v", text, kept)
	}
}

func TestVoiceFilename(t *testing.T) {
	cases := map[string]string{
		"audio/ogg":                "voice.ogg",
		"audio/mp4":                "voice.m4a",
		"audio/mpeg":               "voice.mp3",
		"audio/wav":                "voice.wav",
		"application/octet-stream": "voice.ogg",
	}
	for mime, want := range cases {
		if got := voiceFilename(platform.Attachment{MimeType: mime}); got != want {
			t.Errorf("voiceFilename(%q) = %q, want %q", mime, got, want)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
