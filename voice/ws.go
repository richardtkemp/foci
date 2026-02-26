package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"clod/log"

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
	// APIKey for authentication (from secrets.toml voice.api_key).
	APIKey string

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
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// conn is per-connection state.
type conn struct {
	ws  *websocket.Conn
	cfg *HandlerConfig

	writeMu sync.Mutex // serializes all WebSocket writes (text + binary)
	turnMu  sync.Mutex // serializes agent turns (prevents concurrent STT→agent→TTS)
	audioMu sync.Mutex // protects recording state and buffer

	agentID    string // selected agent (empty until select_agent)
	sessionKey string // voice session key

	recording bool   // true between audio_start and audio_end
	audioBuf  []byte // accumulated audio data during recording
}

// Handler returns an http.HandlerFunc that handles /voice WebSocket connections.
func Handler(cfg HandlerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Authenticate via query parameter.
		apiKey := r.URL.Query().Get("api_key")
		if apiKey == "" {
			http.Error(w, "missing api_key", http.StatusUnauthorized)
			return
		}
		if apiKey != cfg.APIKey {
			http.Error(w, "invalid api_key", http.StatusForbidden)
			return
		}

		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Errorf("voice-ws", "upgrade: %v", err)
			return
		}
		defer ws.Close()

		connID := fmt.Sprintf("%d", time.Now().UnixNano())
		log.Infof("voice-ws", "connected (conn=%s)", connID)

		c := &conn{
			ws:  ws,
			cfg: &cfg,
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
			log.Errorf("voice-ws", "send connected: %v", err)
			return
		}

		// Configure WebSocket timeouts.
		ws.SetReadDeadline(time.Now().Add(pongWait))
		ws.SetPongHandler(func(string) error {
			ws.SetReadDeadline(time.Now().Add(pongWait))
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
		c.audioMu.Lock()
		c.recording = true
		c.audioBuf = nil
		c.audioMu.Unlock()

	case "audio_end":
		c.audioMu.Lock()
		c.recording = false
		audio := c.audioBuf
		c.audioBuf = nil
		c.audioMu.Unlock()

		if len(audio) == 0 {
			c.sendError("empty audio")
			return
		}

		// Process audio in a goroutine with turn lock.
		go c.processAudio(ctx, connID, audio)

	case "text":
		var txt TextMsg
		if err := json.Unmarshal(data, &txt); err != nil {
			c.sendError("invalid text message")
			return
		}
		if txt.Content == "" {
			return
		}

		go c.processText(ctx, connID, txt.Content)

	case "ping":
		c.sendJSON(PongMsg{Type: "pong"})

	default:
		c.sendError(fmt.Sprintf("unknown message type: %q", msg.Type))
	}
}

// handleBinaryMessage appends audio data to the buffer during recording.
func (c *conn) handleBinaryMessage(data []byte) {
	c.audioMu.Lock()
	defer c.audioMu.Unlock()

	if c.recording {
		c.audioBuf = append(c.audioBuf, data...)
	}
}

// handleSelectAgent picks an agent and creates a voice session.
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
		c.sessionKey = fmt.Sprintf("agent:%s:voice:%s", sel.AgentID, connID)
		log.Infof("voice-ws", "agent selected: %s (new session=%s, conn=%s)", c.agentID, c.sessionKey, connID)
	}

	c.sendJSON(SessionReadyMsg{
		Type:       "session_ready",
		AgentID:    c.agentID,
		SessionKey: c.sessionKey,
	})
}

// processAudio transcribes audio and runs the agent pipeline.
func (c *conn) processAudio(ctx context.Context, connID string, audio []byte) {
	c.turnMu.Lock()
	defer c.turnMu.Unlock()

	if c.agentID == "" {
		c.sendError("no agent selected")
		return
	}

	// STT
	log.Debugf("voice-ws", "transcribing %d bytes (conn=%s)", len(audio), connID)
	text, err := c.cfg.STT.Transcribe(ctx, audio, "voice.opus")
	if err != nil {
		log.Errorf("voice-ws", "STT error (conn=%s): %v", connID, err)
		c.sendError(fmt.Sprintf("transcription failed: %v", err))
		return
	}

	if text == "" {
		return
	}

	// Send transcription to client.
	c.sendJSON(TranscriptionMsg{Type: "transcription", Text: text})

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
	c.sendJSON(ResponseStartMsg{Type: "response_start"})

	// Call agent.
	resp, err := c.cfg.HandleMessage(ctx, c.agentID, c.sessionKey, text)
	if err != nil {
		log.Errorf("voice-ws", "agent error (conn=%s): %v", connID, err)
		c.sendError(fmt.Sprintf("agent error: %v", err))
		c.sendJSON(ResponseEndMsg{Type: "response_end"})
		return
	}

	// response_text (final=true)
	c.sendJSON(ResponseTextMsg{Type: "response_text", Content: resp, Final: true})

	// TTS — non-fatal if it fails.
	tts := c.cfg.AgentTTS(c.agentID)
	if tts != nil && resp != "" {
		audioData, err := tts.Synthesize(ctx, resp)
		if err != nil {
			log.Warnf("voice-ws", "TTS error (conn=%s): %v", connID, err)
		} else if len(audioData) > 0 {
			c.sendJSON(AudioStartOutMsg{Type: "audio_start", Format: "mp3"})
			c.sendAudioChunks(audioData)
			c.sendJSON(AudioEndOutMsg{Type: "audio_end"})
		}
	}

	// response_end
	c.sendJSON(ResponseEndMsg{Type: "response_end"})
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
		c.ws.SetWriteDeadline(time.Now().Add(writeWait))
		err := c.ws.WriteMessage(websocket.BinaryMessage, chunk)
		c.writeMu.Unlock()

		if err != nil {
			log.Warnf("voice-ws", "write binary: %v", err)
			return
		}
	}
}

// sendJSON marshals v as JSON and sends it as a text frame.
func (c *conn) sendJSON(v interface{}) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	return c.ws.WriteJSON(v)
}

// sendError sends an error message to the client.
func (c *conn) sendError(msg string) {
	c.sendJSON(ErrorMsg{Type: "error", Message: msg})
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
			c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			err := c.ws.WriteMessage(websocket.PingMessage, nil)
			c.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}
