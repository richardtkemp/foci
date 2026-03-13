package voice

import (
	"encoding/json"
	"testing"
)

func TestConnectedMsg_JSON(t *testing.T) {
	// Proves that ConnectedMsg round-trips through JSON correctly, preserving the
	// type field and all AgentListItem fields including Unicode emoji.
	msg := ConnectedMsg{
		Type: "connected",
		Agents: []AgentListItem{
			{ID: "clutch", Name: "Clutch", Emoji: "🥔"},
			{ID: "fotini", Name: "Φωτεινή", Emoji: "🕯️"},
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ConnectedMsg
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Type != "connected" {
		t.Errorf("type = %q, want %q", decoded.Type, "connected")
	}
	if len(decoded.Agents) != 2 {
		t.Fatalf("agents len = %d, want 2", len(decoded.Agents))
	}
	if decoded.Agents[0].ID != "clutch" {
		t.Errorf("agents[0].id = %q, want %q", decoded.Agents[0].ID, "clutch")
	}
	if decoded.Agents[1].Emoji != "🕯️" {
		t.Errorf("agents[1].emoji = %q, want %q", decoded.Agents[1].Emoji, "🕯️")
	}
}

func TestSessionReadyMsg_JSON(t *testing.T) {
	// Proves that SessionReadyMsg round-trips through JSON with agent_id and
	// session_key fields intact.
	msg := SessionReadyMsg{
		Type:       "session_ready",
		AgentID:    "clutch",
		SessionKey: "agent:clutch:voice:abc123",
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded SessionReadyMsg
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.AgentID != "clutch" {
		t.Errorf("agent_id = %q, want %q", decoded.AgentID, "clutch")
	}
	if decoded.SessionKey != "agent:clutch:voice:abc123" {
		t.Errorf("session_key = %q, want %q", decoded.SessionKey, "agent:clutch:voice:abc123")
	}
}

func TestResponseTextMsg_JSON(t *testing.T) {
	// Proves that ResponseTextMsg round-trips through JSON, preserving the
	// content string and the boolean final flag.
	msg := ResponseTextMsg{
		Type:    "response_text",
		Content: "Hello there!",
		Final:   true,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ResponseTextMsg
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Content != "Hello there!" {
		t.Errorf("content = %q, want %q", decoded.Content, "Hello there!")
	}
	if !decoded.Final {
		t.Error("final should be true")
	}
}

func TestSelectAgentMsg_JSON(t *testing.T) {
	// Proves that SelectAgentMsg round-trips through JSON with the type
	// discriminator and agent_id fields correctly serialised.
	msg := SelectAgentMsg{
		Type:    "select_agent",
		AgentID: "fotini",
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded SelectAgentMsg
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Type != "select_agent" {
		t.Errorf("type = %q, want %q", decoded.Type, "select_agent")
	}
	if decoded.AgentID != "fotini" {
		t.Errorf("agent_id = %q, want %q", decoded.AgentID, "fotini")
	}
}

func TestErrorMsg_JSON(t *testing.T) {
	// Proves that ErrorMsg round-trips through JSON with the message field intact.
	msg := ErrorMsg{
		Type:    "error",
		Message: "something broke",
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ErrorMsg
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Message != "something broke" {
		t.Errorf("message = %q, want %q", decoded.Message, "something broke")
	}
}

func TestTranscriptionMsg_JSON(t *testing.T) {
	// Proves that TranscriptionMsg round-trips through JSON with the text
	// field correctly preserved.
	msg := TranscriptionMsg{
		Type: "transcription",
		Text: "what is the weather",
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded TranscriptionMsg
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Text != "what is the weather" {
		t.Errorf("text = %q, want %q", decoded.Text, "what is the weather")
	}
}

func TestClientMessage_TypeExtraction(t *testing.T) {
	// Proves that the ClientMessage type can extract the "type" discriminator
	// field from all known client message shapes without error.
	tests := []struct {
		json string
		want string
	}{
		{`{"type":"ping"}`, "ping"},
		{`{"type":"select_agent","agent_id":"x"}`, "select_agent"},
		{`{"type":"audio_start","format":"opus","sample_rate":48000}`, "audio_start"},
		{`{"type":"audio_end"}`, "audio_end"},
		{`{"type":"text","content":"hello"}`, "text"},
	}

	for _, tt := range tests {
		var msg ClientMessage
		if err := json.Unmarshal([]byte(tt.json), &msg); err != nil {
			t.Errorf("unmarshal %q: %v", tt.json, err)
			continue
		}
		if msg.Type != tt.want {
			t.Errorf("type = %q, want %q for %s", msg.Type, tt.want, tt.json)
		}
	}
}
