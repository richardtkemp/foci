package app

// GET /app/stt/stream (#3018): the hands-free voice relay. The end-to-end
// tests run the real relay against the real Flux client and a fake Flux server
// built from Deepgram's documented v2 listen protocol (voice/fluxfake). They
// pin: words reach the app as they are heard, the end of the turn arrives
// once with the agent's word replacements, and the provider stream (billed
// per open minute) closes on the final text, on silence, on "stop", and when
// the app's socket goes.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"foci/internal/config"
	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/voice"
	"foci/internal/voice/fluxfake"
)

const fluxKey = "dg-test-key-91c2"

// streamTestHub is a hub with one paired device and one agent whose stream
// setup is the real Flux client against fake, with "foca" → "foci".
func streamTestHub(t *testing.T, fake *fluxfake.Server, silence time.Duration) (*Hub, string) {
	t.Helper()
	h := newTestHub()
	h.deps = platform.ProviderDeps{Config: &config.Config{}, Ctx: context.Background()}
	stt := voice.WrapStreamingSTT(&voice.FluxSTT{Endpoint: fake.URL(), APIKey: fluxKey}, map[string]string{"foca": "foci"})
	h.agents["clutch"] = &appConn{hub: h, agentID: "clutch", sttStream: func() voice.StreamSetup {
		return voice.StreamSetup{STT: stt, Options: voice.StreamOptions{EOTThreshold: 0.75}, SilenceTimeout: silence}
	}}
	h.agentOrder = append(h.agentOrder, "clutch")
	d := h.devices.pair("phone-1", "Phone")
	return h, d.Token
}

// dialStream serves the hub through the real handler registration and dials
// the stream as the app does: a WebSocket with the device token.
func dialStream(t *testing.T, h *Hub, tok, agent string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })
	mux := http.NewServeMux()
	mux.HandleFunc("/app/stt/stream", STTStreamHandler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/app/stt/stream?agentId=" + agent
	hdr := http.Header{}
	if tok != "" {
		hdr.Set("Authorization", "Bearer "+tok)
	}
	return websocket.DefaultDialer.Dial(url, hdr)
}

// next reads the next JSON message, failing on a hang or a closed socket.
func next(t *testing.T, ws *websocket.Conn) sttStreamMsg {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	mt, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Fatalf("message type %d, want text", mt)
	}
	var m sttStreamMsg
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return m
}

// audio sends n bytes of audio in 80 ms chunks.
func audio(t *testing.T, ws *websocket.Conn, n int) {
	t.Helper()
	chunk := make([]byte, 2560)
	for sent := 0; sent < n; sent += len(chunk) {
		if err := ws.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
			t.Fatalf("send audio: %v", err)
		}
	}
}

// socketCloses waits for the server to close the app's socket.
func socketCloses(t *testing.T, ws *websocket.Conn) {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
				t.Fatal("server did not close the stream socket")
			}
			return
		}
	}
}

func TestSTTStream_WordsThenFinalThenClosed(t *testing.T) {
	fake := fluxfake.New(fluxKey)
	defer fake.Close()
	fake.Words = []string{"ask", "foca", "now"}
	fake.BytesPerWord = 2560
	h, tok := streamTestHub(t, fake, time.Minute)

	ws, _, err := dialStream(t, h, tok, "clutch")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = ws.Close() }()
	if m := next(t, ws); m.Type != "ready" {
		t.Fatalf("first message = %+v, want ready", m)
	}
	audio(t, ws, 3*2560)

	want := []sttStreamMsg{
		{Type: "partial", Text: "ask"},
		{Type: "partial", Text: "ask foci"},
		{Type: "partial", Text: "ask foci now"},
		{Type: "final", Text: "ask foci now"},
	}
	for i, w := range want {
		if got := next(t, ws); got != w {
			t.Fatalf("message %d = %+v, want %+v", i, got, w)
		}
	}
	socketCloses(t, ws)
	if !fake.WaitConnEnd(5 * time.Second) {
		t.Fatal("Flux connection still open after the final text: billing continues")
	}
	if got := fake.AudioBytes(); got != 3*2560 {
		t.Errorf("Flux got %d audio bytes, want %d (every byte relayed)", got, 3*2560)
	}
	q := fake.Queries()[0]
	if q.Get("eot_threshold") != "0.75" || q.Get("encoding") != "linear16" || q.Get("sample_rate") != "16000" {
		t.Errorf("Flux query = %v, want the stream options and the PCM shape", q)
	}
}

func TestSTTStream_SilenceClosesStream(t *testing.T) {
	fake := fluxfake.New(fluxKey) // hears nothing
	defer fake.Close()
	h, tok := streamTestHub(t, fake, 300*time.Millisecond)

	ws, _, err := dialStream(t, h, tok, "clutch")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = ws.Close() }()
	if m := next(t, ws); m.Type != "ready" {
		t.Fatalf("first = %+v", m)
	}
	audio(t, ws, 2560)
	if m := next(t, ws); m.Type != "closed" || m.Reason != "silence" {
		t.Fatalf("got %+v, want closed/silence", m)
	}
	socketCloses(t, ws)
	if !fake.WaitConnEnd(5 * time.Second) {
		t.Fatal("Flux connection open after the silence close")
	}
}

func TestSTTStream_StopAndDisconnectCloseUpstream(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(ws *websocket.Conn)
	}{
		{"stop", func(ws *websocket.Conn) { _ = ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"stop"}`)) }},
		{"disconnect", func(ws *websocket.Conn) { _ = ws.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := fluxfake.New(fluxKey)
			defer fake.Close()
			h, tok := streamTestHub(t, fake, time.Minute)
			ws, _, err := dialStream(t, h, tok, "clutch")
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = ws.Close() }()
			if m := next(t, ws); m.Type != "ready" {
				t.Fatalf("first = %+v", m)
			}
			tc.end(ws)
			if !fake.WaitConnEnd(5 * time.Second) {
				t.Fatal("Flux connection still open after the app ended the stream")
			}
			if !fake.GotCloseStream() {
				t.Error("relay did not send CloseStream upstream")
			}
		})
	}
}

func TestSTTStream_ProviderFailureReportsError(t *testing.T) {
	fake := fluxfake.New(fluxKey)
	defer fake.Close()
	fake.FatalAfterBytes = 2560
	h, tok := streamTestHub(t, fake, time.Minute)
	ws, _, err := dialStream(t, h, tok, "clutch")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = ws.Close() }()
	if m := next(t, ws); m.Type != "ready" {
		t.Fatalf("first = %+v", m)
	}
	audio(t, ws, 2560)
	if m := next(t, ws); m.Type != "error" {
		t.Fatalf("got %+v, want error (the app falls back)", m)
	}
	socketCloses(t, ws)
}

func TestSTTStream_OpenFailureReportsErrorWithoutKey(t *testing.T) {
	fake := fluxfake.New("some-other-key") // rejects fluxKey: 401
	defer fake.Close()
	h, tok := streamTestHub(t, fake, time.Minute)
	ws, _, err := dialStream(t, h, tok, "clutch")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = ws.Close() }()
	m := next(t, ws)
	if m.Type != "error" {
		t.Fatalf("got %+v, want error", m)
	}
	if strings.Contains(m.Message, fluxKey) {
		t.Error("the error sent to the app carries the provider key")
	}
}

func TestSTTStream_RefusedBeforeUpgrade(t *testing.T) {
	fake := fluxfake.New(fluxKey)
	defer fake.Close()
	h, tok := streamTestHub(t, fake, time.Minute)
	h.agents["plain"] = &appConn{hub: h, agentID: "plain"} // no streaming STT
	h.agentOrder = append(h.agentOrder, "plain")

	for _, tc := range []struct {
		name, tok, agent string
		code             int
	}{
		{"no token", "", "clutch", http.StatusUnauthorized},
		{"bad token", "nope", "clutch", http.StatusForbidden},
		{"unknown agent", tok, "ghost", http.StatusNotFound},
		{"no streaming provider", tok, "plain", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, resp, err := dialStream(t, h, tc.tok, tc.agent)
			if err == nil {
				t.Fatal("dial succeeded, want a refused handshake")
			}
			if resp == nil || resp.StatusCode != tc.code {
				got := 0
				if resp != nil {
					got = resp.StatusCode
				}
				t.Fatalf("status = %d, want %d", got, tc.code)
			}
		})
	}
	if fake.Conns() != 0 {
		t.Errorf("a refused request opened %d Flux connections", fake.Conns())
	}
}

func TestSTTStream_CapsAdvertised(t *testing.T) {
	h := newTestHub()
	h.agents["a"] = &appConn{hub: h, agentID: "a"}
	if hasFeature(h.caps().Features, featureSTTStream) {
		t.Fatal("sttStream advertised with no streaming provider")
	}
	h.agents["b"] = &appConn{hub: h, agentID: "b", sttStream: func() voice.StreamSetup {
		return voice.StreamSetup{STT: voice.WrapStreamingSTT(&voice.FluxSTT{APIKey: "k"}, nil)}
	}}
	if !hasFeature(h.caps().Features, featureSTTStream) {
		t.Fatal("sttStream not advertised with a streaming provider")
	}
}

func hasFeature(fs []string, f string) bool {
	for _, x := range fs {
		if x == f {
			return true
		}
	}
	return false
}

// A spoken message (fap.ClientMessage.Voice) is a voice turn, and its text is
// never run as a "!cmd" or a command.
func TestRouteUserTurn_SpokenTaggedVoiceAndNeverACommand(t *testing.T) {
	for _, text := range []string{"hello there", "!ls", "/stop"} {
		t.Run(text, func(t *testing.T) {
			h := newTestHub()
			fa := registerFakeAgent(h, "ag")
			c := fakeClientFor(h)
			c.deviceID = "dev-1"
			h.handleVoiceModePut(c, fap.VoiceModePut{ConversationIDs: []string{"conv-1"}})
			h.routeUserTurn(c, "conv-1", "ag", text, nil, nil, "env-1", 1, platform.SteerDefault, false, true)
			if fa.env == nil {
				t.Fatal("no envelope enqueued: the spoken text was diverted")
			}
			if !fa.env.Voice {
				t.Error("env.Voice = false for a spoken message")
			}
			if fa.env.Text != text {
				t.Errorf("env.Text = %q, want %q", fa.env.Text, text)
			}
		})
	}
}

// "finish" (the user pressed stop) ends the turn now: Flux's ForceEndTurn
// answers with an EndOfTurn for the words so far, which is the final text.
func TestSTTStream_FinishSendsWordsSoFar(t *testing.T) {
	fake := fluxfake.New(fluxKey)
	defer fake.Close()
	fake.Words = []string{"turn", "foca", "off", "lights"}
	fake.BytesPerWord = 2560
	h, tok := streamTestHub(t, fake, time.Minute)
	ws, _, err := dialStream(t, h, tok, "clutch")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = ws.Close() }()
	if m := next(t, ws); m.Type != "ready" {
		t.Fatalf("first = %+v", m)
	}
	audio(t, ws, 2*2560)
	for _, w := range []string{"turn", "turn foci"} {
		if m := next(t, ws); m.Type != "partial" || m.Text != w {
			t.Fatalf("got %+v, want partial %q", m, w)
		}
	}
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"finish"}`)); err != nil {
		t.Fatal(err)
	}
	if m := next(t, ws); m.Type != "final" || m.Text != "turn foci" {
		t.Fatalf("got %+v, want final \"turn foci\"", m)
	}
	socketCloses(t, ws)
	if !fake.WaitConnEnd(5 * time.Second) {
		t.Fatal("Flux connection open after the final text")
	}
}

func TestSTTStream_FinishWithNothingHeard(t *testing.T) {
	fake := fluxfake.New(fluxKey)
	defer fake.Close()
	h, tok := streamTestHub(t, fake, time.Minute)
	ws, _, err := dialStream(t, h, tok, "clutch")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = ws.Close() }()
	if m := next(t, ws); m.Type != "ready" {
		t.Fatalf("first = %+v", m)
	}
	_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"finish"}`))
	if m := next(t, ws); m.Type != "closed" || m.Reason != "nothing_heard" {
		t.Fatalf("got %+v, want closed/nothing_heard", m)
	}
	socketCloses(t, ws)
	if !fake.WaitConnEnd(5 * time.Second) {
		t.Fatal("Flux connection open after an empty finish")
	}
}
