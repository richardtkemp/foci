package voice

import

// Protocol message types for the /voice WebSocket endpoint.
// All JSON messages are text frames; audio is sent as binary frames.
"foci/internal/log"

var (
	voiceLog    = log.NewComponentLogger("voice")
	voice_wsLog = log.NewComponentLogger("voice-ws")
)

// --- Client → Server ---

// ClientMessage is the envelope for all client JSON messages.
// Decode "type" first, then switch on it.
type ClientMessage struct {
	Type string `json:"type"`
}

// SelectAgentMsg — client picks which agent to talk to.
type SelectAgentMsg struct {
	Type       string `json:"type"`                  // "select_agent"
	AgentID    string `json:"agent_id"`              // agent ID from connected message
	SessionKey string `json:"session_key,omitempty"` // optional: reuse existing session
}

// AudioStartMsg — client begins recording.
type AudioStartMsg struct {
	Type       string `json:"type"`        // "audio_start"
	Format     string `json:"format"`      // e.g. "opus"
	SampleRate int    `json:"sample_rate"` // e.g. 48000
}

// TextMsg — client sends typed text input.
type TextMsg struct {
	Type    string `json:"type"`    // "text"
	Content string `json:"content"` // message text
}

// --- Server → Client ---

// AgentListItem describes an agent in the connected message.
type AgentListItem struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Emoji string `json:"emoji"`
}

// ConnectedMsg — handshake sent immediately after WebSocket upgrade.
type ConnectedMsg struct {
	Type   string          `json:"type"`   // "connected"
	Agents []AgentListItem `json:"agents"` // available agents
}

// SessionReadyMsg — agent selected, session active.
type SessionReadyMsg struct {
	Type       string `json:"type"`        // "session_ready"
	AgentID    string `json:"agent_id"`    // selected agent
	SessionKey string `json:"session_key"` // e.g. "clutch/i1709596800/0"
}

// TranscriptionMsg — STT result for client audio.
type TranscriptionMsg struct {
	Type string `json:"type"` // "transcription"
	Text string `json:"text"` // transcribed text
}

// ResponseStartMsg — agent is generating a response.
type ResponseStartMsg struct {
	Type string `json:"type"` // "response_start"
}

// ResponseTextMsg — agent response text (possibly partial).
type ResponseTextMsg struct {
	Type    string `json:"type"`    // "response_text"
	Content string `json:"content"` // text content
	Final   bool   `json:"final"`   // true = last text chunk
}

// AudioStartOutMsg — server begins streaming TTS audio.
type AudioStartOutMsg struct {
	Type   string `json:"type"`   // "audio_start"
	Format string `json:"format"` // e.g. "mp3"
}

// AudioEndOutMsg — server finished streaming TTS audio.
type AudioEndOutMsg struct {
	Type string `json:"type"` // "audio_end"
}

// ResponseEndMsg — turn complete.
type ResponseEndMsg struct {
	Type string `json:"type"` // "response_end"
}

// ErrorMsg — server-side error.
type ErrorMsg struct {
	Type    string `json:"type"`    // "error"
	Message string `json:"message"` // human-readable error description
}

// PongMsg — response to client ping.
type PongMsg struct {
	Type string `json:"type"` // "pong"
}
