package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"foci/internal/voice"
)

// Hands-free voice input (#3018): the app streams its microphone to
// GET /app/stt/stream?agentId=<id> (a WebSocket), and foci relays the audio to
// the agent's streaming STT provider (Deepgram Flux first). The provider key
// never leaves the server. The app shows the words as they come, and sends the
// final text as the user's message when the provider reports the end of the
// turn. The protocol, app ⇄ foci:
//
//	app → foci  binary frames: audio, little-endian signed 16-bit PCM, mono,
//	            16 kHz (voice.StreamEncoding / StreamSampleRate)
//	app → foci  {"type":"finish"}: send now (the user pressed stop): the
//	            provider ends the turn and foci answers with "final"; with no
//	            words heard yet, "closed" with reason "nothing_heard"
//	app → foci  {"type":"stop"}: end the stream (the user cancelled or left)
//	foci → app  {"type":"ready"}: the provider stream is open; send audio
//	foci → app  {"type":"partial","text":"…"}: the whole turn so far
//	foci → app  {"type":"final","text":"…"}: the end of the turn; foci then
//	            closes the stream
//	foci → app  {"type":"closed","reason":"silence"|"max_duration"|
//	            "nothing_heard"}: foci ended the stream with no turn
//	foci → app  {"type":"error","message":"…"}: the stream failed; the app
//	            falls back to recording and uploading
//
// Texts carry the agent's STT word replacements. The stream (billed per open
// minute) closes on the final text, after SilenceTimeout with no new words, at
// sttStreamMaxDuration, on "stop", and when the app's socket goes.

// featureSTTStream is the hello capability an app reads to offer hands-free
// voice input: at least one agent has a streaming STT provider.
const featureSTTStream = "sttStream"

const (
	// sttStreamMaxFrame caps one inbound audio frame. 80 ms of audio is
	// 2560 bytes; this allows far larger chunks and stops a flood.
	sttStreamMaxFrame = 256 << 10
	// sttStreamMaxDuration bounds one stream, whatever it hears: a stream
	// that never reaches an end of turn (noise heard as words) must still
	// stop being billed.
	sttStreamMaxDuration = 5 * time.Minute
	sttStreamWriteWait   = 10 * time.Second
	// sttStreamFinishWait bounds the wait for the provider's end of turn
	// after "finish": past it the words heard so far are the final text.
	sttStreamFinishWait = 5 * time.Second
)

// sttStreamMsg is one JSON message on the stream socket, either way.
type sttStreamMsg struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

var sttStreamUpgrader = websocket.Upgrader{
	ReadBufferSize:  8192,
	WriteBufferSize: 4096,
	CheckOrigin:     checkOrigin,
}

// streamSetup resolves the conn's streaming STT now; the zero value when it
// has none.
func (c *appConn) streamSetup() voice.StreamSetup {
	if c == nil || c.sttStream == nil {
		return voice.StreamSetup{}
	}
	return c.sttStream()
}

// ServeSTTStream handles GET /app/stt/stream?agentId=<id>: the hands-free
// voice relay (see the protocol above). Authenticated like /app/stt.
func (h *Hub) ServeSTTStream(w http.ResponseWriter, r *http.Request) {
	dev, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	agentID := r.URL.Query().Get("agentId")
	if agentID == "" {
		http.Error(w, "agentId required", http.StatusBadRequest)
		return
	}
	conn := h.PrimaryBot(agentID)
	if conn == nil {
		http.Error(w, "unknown agent", http.StatusNotFound)
		return
	}
	setup := conn.streamSetup()
	if setup.STT == nil {
		// Before the upgrade, so the app reads a plain 404 and falls back.
		http.Error(w, "no streaming speech-to-text configured for this agent", http.StatusNotFound)
		return
	}
	ws, err := sttStreamUpgrader.Upgrade(w, r, nil)
	if err != nil {
		appLog.Warnf("stt stream upgrade: %v", err)
		return
	}
	defer func() { _ = ws.Close() }()
	ws.SetReadLimit(sttStreamMaxFrame)

	ctx := h.deps.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	started := time.Now()
	st, err := setup.STT.OpenStream(ctx, setup.Options)
	if err != nil {
		appLog.Warnf("stt stream: device=%s agent=%s: open failed: %v", dev.DeviceID, agentID, err)
		writeSTTStream(ws, sttStreamMsg{Type: "error", Message: "speech service unavailable"})
		return
	}
	reason := relaySTTStream(ctx, ws, st, setup.SilenceTimeout, sttStreamMaxDuration)
	appLog.Infof("stt stream: device=%s agent=%s closed after %s: %s", dev.DeviceID, agentID, time.Since(started).Round(100*time.Millisecond), reason)
}

func writeSTTStream(ws *websocket.Conn, m sttStreamMsg) {
	b, _ := json.Marshal(m)
	_ = ws.SetWriteDeadline(time.Now().Add(sttStreamWriteWait))
	_ = ws.WriteMessage(websocket.TextMessage, b)
}

// relaySTTStream runs one open stream to its end and returns why it ended. It
// always closes st. Only this goroutine writes to ws.
func relaySTTStream(ctx context.Context, ws *websocket.Conn, st voice.STTStream, silence, maxDur time.Duration) string {
	defer func() {
		_ = st.Close()
		// Drain, so the provider's reader is never left blocked on a send.
		for range st.Events() {
		}
	}()

	// The app's side: audio goes to the provider; "stop" or a dead socket
	// ends the stream.
	appGone := make(chan string, 1)
	finishReq := make(chan struct{}, 1)
	go func() {
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				appGone <- "app disconnected"
				return
			}
			switch mt {
			case websocket.BinaryMessage:
				if err := st.Send(data); err != nil {
					// The provider side has ended; the events loop reports why.
					continue
				}
			case websocket.TextMessage:
				var m sttStreamMsg
				if json.Unmarshal(data, &m) != nil {
					continue
				}
				switch m.Type {
				case "stop":
					appGone <- "stopped by app"
					return
				case "finish":
					select {
					case finishReq <- struct{}{}:
					default: // one is already pending
					}
				}
			}
		}
	}()

	writeSTTStream(ws, sttStreamMsg{Type: "ready"})

	var silenceC <-chan time.Time
	var silenceT *time.Timer
	if silence > 0 {
		silenceT = time.NewTimer(silence)
		defer silenceT.Stop()
		silenceC = silenceT.C
	}
	maxT := time.NewTimer(maxDur)
	defer maxT.Stop()

	last := ""
	var finishC <-chan time.Time
	for {
		select {
		case <-finishReq:
			if finishC != nil {
				continue // already finishing
			}
			if last == "" {
				writeSTTStream(ws, sttStreamMsg{Type: "closed", Reason: "nothing_heard"})
				return "finish with nothing heard"
			}
			if err := st.Finish(); err != nil {
				writeSTTStream(ws, sttStreamMsg{Type: "final", Text: last})
				return "finish (provider gone): words so far"
			}
			ft := time.NewTimer(sttStreamFinishWait)
			defer ft.Stop()
			finishC = ft.C
		case <-finishC:
			// The provider never answered the finish: the words so far are
			// the user's message.
			writeSTTStream(ws, sttStreamMsg{Type: "final", Text: last})
			return "finish timed out: words so far"
		case <-ctx.Done():
			return "server shutting down"
		case why := <-appGone:
			return why
		case <-silenceC:
			writeSTTStream(ws, sttStreamMsg{Type: "closed", Reason: "silence"})
			return "silence"
		case <-maxT.C:
			writeSTTStream(ws, sttStreamMsg{Type: "closed", Reason: "max_duration"})
			return "max duration"
		case ev, ok := <-st.Events():
			if !ok {
				if err := st.Err(); err != nil {
					appLog.Warnf("stt stream: provider ended: %v", err)
					writeSTTStream(ws, sttStreamMsg{Type: "error", Message: "speech service failed"})
					return "provider error"
				}
				writeSTTStream(ws, sttStreamMsg{Type: "error", Message: "speech service closed"})
				return "provider closed"
			}
			text := strings.TrimSpace(ev.Text)
			if ev.Kind == voice.StreamEndOfTurn {
				if text == "" {
					continue // a turn with no words: keep listening
				}
				writeSTTStream(ws, sttStreamMsg{Type: "final", Text: text})
				return "end of turn"
			}
			if text == "" || text == last {
				continue
			}
			last = text
			// New words: the user is talking, so the silence clock restarts.
			if silenceT != nil {
				if !silenceT.Stop() {
					select {
					case <-silenceT.C:
					default:
					}
				}
				silenceT.Reset(silence)
			}
			writeSTTStream(ws, sttStreamMsg{Type: "partial", Text: text})
		}
	}
}
