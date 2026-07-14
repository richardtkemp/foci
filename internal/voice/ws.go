package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"foci/internal/log"
	"foci/internal/session"

	"github.com/gorilla/websocket"
)

const (
	// audioChunkSize is the max size of binary frames for TTS audio.
	audioChunkSize = 4096

	// writeWait is the time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// pongWait is the time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// pingPeriod sends pings at this interval. Must be less than pongWait.
	pingPeriod = 50 * time.Second
)

// AgentInfo describes an agent for the connected message.
type AgentInfo struct {
	ID    string
	Name  string
	Emoji string
}

// HandlerConfig holds all dependencies for the WebSocket handler.
// Callbacks avoid importing agent/session packages.
type HandlerConfig struct {
	// ListAgents returns the available agents in display order.
	ListAgents func() []AgentInfo

	// HandleMessage sends text to an agent session and returns the response.
	// agentID identifies the agent, sessionKey the voice session, text the input.
	HandleMessage func(ctx context.Context, agentID, sessionKey, text string) (string, error)

	// STT provider for speech-to-text transcription.
	STT STT

	// AgentTTS returns a TTS provider for the given agent (with per-agent rate).
	AgentTTS func(agentID string) TTS

	// SessionExists returns true if a session with the given key already exists.
	// Used to allow clients to reattach to existing sessions.
	SessionExists func(key string) bool

	// MaxFrameBytes caps a single inbound WebSocket frame (applied via
	// SetReadLimit). MaxAudioBytes caps the total accumulated audio buffer for
	// one recording. Both guard against a client streaming volume to OOM the
	// gateway (P1-10). A non-positive value disables that specific limit;
	// production always supplies both from [voice] config defaults.
	MaxFrameBytes int64
	MaxAudioBytes int

	// MaxConcurrentTurns bounds the number of in-flight STT→agent→TTS
	// goroutines per connection. Turns serialise on turnMu, so without a cap a
	// client flooding audio_end/text frames would pile up goroutines all
	// blocked on the lock. A non-positive value disables the cap; production
	// supplies it from [voice] config.
	MaxConcurrentTurns int
}

var upgrader = websocket.Upgrader{
	CheckOrigin: sameOriginOrNone,
}

// sameOriginOrNone is the WebSocket origin policy: allow native clients (which
// send no Origin header) and same-origin browser connections (Origin host
// matches the request Host), and reject everything else. This blocks
// cross-site WebSocket hijacking from an attacker page without needing an
// origin allowlist; the bundled browser client is served same-origin.
func sameOriginOrNone(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser client (e.g. the voice CLI)
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// conn is per-connection state.
type conn struct {
	ws  *websocket.Conn
	cfg *HandlerConfig

	writeMu sync.Mutex // serializes all WebSocket writes (text + binary)
	turnMu  sync.Mutex // serializes agent turns (prevents concurrent STT→agent→TTS)
	audioMu sync.Mutex // protects recording state and buffer

	turnSem chan struct{} // bounds in-flight turn goroutines; nil = unbounded

	agentID    string // selected agent (empty until select_agent)
	sessionKey string // voice session key

	recording  bool   // true between audio_start and audio_end
	audioBuf   []byte // accumulated audio data during recording
	sampleRate int    // sample rate from audio_start (default 24000)
}

// acquireTurn reserves a turn-processing slot without blocking. Returns false
// when the per-connection limit is saturated (caller should reject the frame).
// A nil turnSem means the cap is disabled — always succeeds.
func (c *conn) acquireTurn() bool {
	if c.turnSem == nil {
		return true
	}
	select {
	case c.turnSem <- struct{}{}:
		return true
	default:
		return false
	}
}

// releaseTurn frees a slot reserved by acquireTurn.
func (c *conn) releaseTurn() {
	if c.turnSem == nil {
		return
	}
	<-c.turnSem
}

// Handler returns an http.HandlerFunc that handles /voice WebSocket connections.
func Handler(cfg HandlerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Auth is handled by the HTTP auth middleware (http.api_key).
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Errorf("voice-ws", "upgrade: %v", err)
			return
		}
		defer func() { _ = ws.Close() }()

		connID := fmt.Sprintf("%d", time.Now().UnixNano())
		log.Infof("voice-ws", "connected (conn=%s)", connID)

		c := &conn{
			ws:  ws,
			cfg: &cfg,
		}
		if cfg.MaxConcurrentTurns > 0 {
			c.turnSem = make(chan struct{}, cfg.MaxConcurrentTurns)
		}

		// Send connected message with agent list.
		agents := cfg.ListAgents()
		var items []AgentListItem
		for _, a := range agents {
			items = append(items, AgentListItem{
				ID:    a.ID,
				Name:  a.Name,
				Emoji: a.Emoji,
			})
		}
		if err := c.sendJSON(ConnectedMsg{Type: "connected", Agents: items}); err != nil {
			log.Errorf("voice-ws", "send connected (conn=%s): %v", connID, err)
			return
		}

		// Cap the size of any single inbound frame so an oversized text or
		// binary message can't be buffered whole into memory before we look at
		// it (P1-10). gorilla closes the connection and the read loop exits
		// with ErrReadLimit when a frame exceeds this.
		if cfg.MaxFrameBytes > 0 {
			ws.SetReadLimit(cfg.MaxFrameBytes)
		}

		// Configure WebSocket timeouts.
		_ = ws.SetReadDeadline(time.Now().Add(pongWait))
		ws.SetPongHandler(func(string) error {
			_ = ws.SetReadDeadline(time.Now().Add(pongWait))
			return nil
		})

		// Start ping ticker in background.
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		go c.pingLoop(ctx)

		// Read loop.
		for {
			msgType, data, err := ws.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
					log.Warnf("voice-ws", "read error (conn=%s): %v", connID, err)
				}
				break
			}

			switch msgType {
			case websocket.TextMessage:
				c.handleTextMessage(ctx, connID, data)
			case websocket.BinaryMessage:
				c.handleBinaryMessage(data)
			}
		}

		log.Infof("voice-ws", "disconnected (conn=%s)", connID)
	}
}

// handleTextMessage processes a JSON text frame.
func (c *conn) handleTextMessage(ctx context.Context, connID string, data []byte) {
	var msg ClientMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		c.sendError("invalid JSON")
		return
	}

	switch msg.Type {
	case "select_agent":
		var sel SelectAgentMsg
		if err := json.Unmarshal(data, &sel); err != nil {
			c.sendError("invalid select_agent message")
			return
		}
		c.handleSelectAgent(connID, sel)

	case "audio_start":
		var as AudioStartMsg
		if err := json.Unmarshal(data, &as); err != nil {
			c.sendError("invalid audio_start message")
			return
		}
		sampleRate := as.SampleRate
		if sampleRate <= 0 {
			sampleRate = 24000 // default
		}

		c.audioMu.Lock()
		c.recording = true
		c.audioBuf = nil
		c.sampleRate = sampleRate
		c.audioMu.Unlock()

	case "audio_end":
		c.audioMu.Lock()
		c.recording = false
		audio := c.audioBuf
		c.audioBuf = nil
		sampleRate := c.sampleRate
		c.audioMu.Unlock()

		if len(audio) == 0 {
			c.sendError("empty audio")
			return
		}

		// Process audio in a goroutine with turn lock, bounded by turnSem so a
		// frame flood can't spawn unbounded goroutines.
		if !c.acquireTurn() {
			c.sendError("busy: too many in-flight requests, slow down")
			return
		}
		go func() {
			defer c.releaseTurn()
			c.processAudio(ctx, connID, audio, sampleRate)
		}()

	case "text":
		var txt TextMsg
		if err := json.Unmarshal(data, &txt); err != nil {
			c.sendError("invalid text message")
			return
		}
		if txt.Content == "" {
			return
		}

		if !c.acquireTurn() {
			c.sendError("busy: too many in-flight requests, slow down")
			return
		}
		go func() {
			defer c.releaseTurn()
			c.processText(ctx, connID, txt.Content)
		}()

	case "ping":
		_ = c.sendJSON(PongMsg{Type: "pong"})

	default:
		c.sendError(fmt.Sprintf("unknown message type: %q", msg.Type))
	}
}

// handleBinaryMessage appends audio data to the buffer during recording,
// enforcing the total-buffer cap so a client cannot stream unbounded audio to
// exhaust gateway memory (P1-10). On overflow it stops recording, drops the
// buffer, and reports an error rather than growing without bound.
func (c *conn) handleBinaryMessage(data []byte) {
	c.audioMu.Lock()
	if !c.recording {
		c.audioMu.Unlock()
		return
	}
	maxBytes := c.cfg.MaxAudioBytes
	if maxBytes > 0 && len(c.audioBuf)+len(data) > maxBytes {
		c.recording = false
		c.audioBuf = nil
		c.audioMu.Unlock()
		c.sendError(fmt.Sprintf("audio exceeds maximum buffer size of %d bytes", maxBytes))
		return
	}
	c.audioBuf = append(c.audioBuf, data...)
	c.audioMu.Unlock()
}

// handleSelectAgent picks an agent and creates or reuses a voice session.
func (c *conn) handleSelectAgent(connID string, sel SelectAgentMsg) {
	// Validate agent exists.
	agents := c.cfg.ListAgents()
	found := false
	for _, a := range agents {
		if a.ID == sel.AgentID {
			found = true
			break
		}
	}
	if !found {
		c.sendError(fmt.Sprintf("unknown agent: %q", sel.AgentID))
		return
	}

	c.agentID = sel.AgentID
	if sel.SessionKey != "" && c.cfg.SessionExists != nil && c.cfg.SessionExists(sel.SessionKey) {
		c.sessionKey = sel.SessionKey
		log.Infof("voice-ws", "agent selected: %s (reused session=%s, conn=%s)", c.agentID, c.sessionKey, connID)
	} else {
		// Generate a timestamp-based chat ID for new voice sessions.
		voiceChatID := int64(time.Now().Unix())
		c.sessionKey = session.NewChatSessionKey(sel.AgentID, voiceChatID)
		log.Infof("voice-ws", "agent selected: %s (new session=%s, conn=%s)", c.agentID, c.sessionKey, connID)
	}

	_ = c.sendJSON(SessionReadyMsg{
		Type:       "session_ready",
		AgentID:    c.agentID,
		SessionKey: c.sessionKey,
	})
}

// processAudio transcribes audio and runs the agent pipeline.
func (c *conn) processAudio(ctx context.Context, connID string, audio []byte, sampleRate int) {
	c.turnMu.Lock()
	defer c.turnMu.Unlock()

	if c.agentID == "" {
		c.sendError("no agent selected")
		return
	}

	// Wrap raw PCM in WAV header (16-bit, mono, little-endian).
	wavAudio := wrapPCMInWAV(audio, sampleRate, 1, 16)

	// STT
	log.Debugf("voice-ws", "transcribing %d bytes (conn=%s)", len(wavAudio), connID)
	text, err := c.cfg.STT.Transcribe(ctx, wavAudio, "voice.wav")
	if err != nil {
		log.Errorf("voice-ws", "STT error (conn=%s): %v", connID, err)
		c.sendError(fmt.Sprintf("transcription failed: %v", err))
		return
	}

	if text == "" {
		return
	}

	// Send transcription to client.
	_ = c.sendJSON(TranscriptionMsg{Type: "transcription", Text: text})

	// Run agent pipeline with transcribed text.
	c.runAgentPipeline(ctx, connID, text)
}

// processText runs the agent pipeline with typed text input.
func (c *conn) processText(ctx context.Context, connID string, text string) {
	c.turnMu.Lock()
	defer c.turnMu.Unlock()

	if c.agentID == "" {
		c.sendError("no agent selected")
		return
	}

	c.runAgentPipeline(ctx, connID, text)
}

// runAgentPipeline handles the response_start → agent call → response_text → TTS → response_end flow.
// Must be called with turnMu held.
func (c *conn) runAgentPipeline(ctx context.Context, connID string, text string) {
	// response_start
	_ = c.sendJSON(ResponseStartMsg{Type: "response_start"})

	// Call agent.
	resp, err := c.cfg.HandleMessage(ctx, c.agentID, c.sessionKey, text)
	if err != nil {
		log.Errorf("voice-ws", "agent error (session=%s, conn=%s): %v", c.sessionKey, connID, err)
		c.sendError(fmt.Sprintf("agent error: %v", err))
		_ = c.sendJSON(ResponseEndMsg{Type: "response_end"})
		return
	}

	// response_text (final=true)
	_ = c.sendJSON(ResponseTextMsg{Type: "response_text", Content: resp, Final: true})

	// TTS — non-fatal if it fails.
	tts := c.cfg.AgentTTS(c.agentID)
	if tts != nil && resp != "" {
		audioData, err := tts.Synthesize(ctx, resp)
		if err != nil {
			log.Warnf("voice-ws", "TTS error (session=%s, conn=%s): %v", c.sessionKey, connID, err)
		} else if len(audioData) > 0 {
			_ = c.sendJSON(AudioStartOutMsg{Type: "audio_start", Format: "mp3"})
			c.sendAudioChunks(audioData)
			_ = c.sendJSON(AudioEndOutMsg{Type: "audio_end"})
		}
	}

	// response_end
	_ = c.sendJSON(ResponseEndMsg{Type: "response_end"})
}

// sendAudioChunks sends audio data as binary frames, chunked to audioChunkSize.
func (c *conn) sendAudioChunks(data []byte) {
	for len(data) > 0 {
		chunk := data
		if len(chunk) > audioChunkSize {
			chunk = data[:audioChunkSize]
		}
		data = data[len(chunk):]

		c.writeMu.Lock()
		_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
		err := c.ws.WriteMessage(websocket.BinaryMessage, chunk)
		c.writeMu.Unlock()

		if err != nil {
			log.Warnf("voice-ws", "write binary (session=%s): %v", c.sessionKey, err)
			return
		}
	}
}

// sendJSON marshals v as JSON and sends it as a text frame.
func (c *conn) sendJSON(v interface{}) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	return c.ws.WriteJSON(v)
}

// sendError sends an error message to the client.
func (c *conn) sendError(msg string) {
	_ = c.sendJSON(ErrorMsg{Type: "error", Message: msg})
}

// pingLoop sends periodic WebSocket pings.
func (c *conn) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.writeMu.Lock()
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			err := c.ws.WriteMessage(websocket.PingMessage, nil)
			c.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}
