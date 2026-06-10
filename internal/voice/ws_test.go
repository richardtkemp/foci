package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// --- Mock STT/TTS ---

type mockSTT struct {
	result string
	err    error
}

func (m *mockSTT) Transcribe(_ context.Context, _ []byte, _ string) (string, error) {
	return m.result, m.err
}

type mockTTS struct {
	result []byte
	err    error
}

func (m *mockTTS) Synthesize(_ context.Context, _ string) ([]byte, error) {
	return m.result, m.err
}

// --- Helpers ---

func testConfig(overrides ...func(*HandlerConfig)) HandlerConfig {
	cfg := HandlerConfig{
		ListAgents: func() []AgentInfo {
			return []AgentInfo{
				{ID: "agent1", Name: "Agent One", Emoji: "🤖"},
				{ID: "agent2", Name: "Agent Two", Emoji: "🧠"},
			}
		},
		HandleMessage: func(_ context.Context, _, _, text string) (string, error) {
			return "response to: " + text, nil
		},
		STT:      &mockSTT{result: "hello world"},
		AgentTTS: func(_ string) TTS { return &mockTTS{result: []byte("fake-mp3")} },
	}
	for _, fn := range overrides {
		fn(&cfg)
	}
	return cfg
}

func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	dialer := websocket.Dialer{}
	ws, _, err := dialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return ws
}

func readJSON(t *testing.T, ws *websocket.Conn, v interface{}) {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %q: %v", string(data), err)
	}
}

func sendJSON(t *testing.T, ws *websocket.Conn, v interface{}) {
	t.Helper()
	if err := ws.WriteJSON(v); err != nil {
		t.Fatalf("write json: %v", err)
	}
}

// readRawMessage reads either text or binary and returns (messageType, data).
func readRawMessage(t *testing.T, ws *websocket.Conn) (int, []byte) {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	msgType, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return msgType, data
}

// --- Tests ---

func TestConnect(t *testing.T) {
	// Proves that opening a WebSocket connection to the handler immediately
	// receives a "connected" message as the first server response.
	srv := httptest.NewServer(Handler(testConfig()))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/voice"
	ws := dialWS(t, url)
	defer ws.Close()

	var msg ConnectedMsg
	readJSON(t, ws, &msg)
	if msg.Type != "connected" {
		t.Errorf("type = %q, want %q", msg.Type, "connected")
	}
}

func TestConnected_AgentList(t *testing.T) {
	// Proves that the connected message includes the full list of agents from
	// ListAgents, with IDs and emoji correctly populated.
	srv := httptest.NewServer(Handler(testConfig()))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/voice"
	ws := dialWS(t, url)
	defer ws.Close()

	var msg ConnectedMsg
	readJSON(t, ws, &msg)
	if len(msg.Agents) != 2 {
		t.Fatalf("agents len = %d, want 2", len(msg.Agents))
	}
	if msg.Agents[0].ID != "agent1" {
		t.Errorf("agents[0].id = %q, want %q", msg.Agents[0].ID, "agent1")
	}
	if msg.Agents[0].Emoji != "🤖" {
		t.Errorf("agents[0].emoji = %q, want %q", msg.Agents[0].Emoji, "🤖")
	}
}

func TestSelectAgent_SessionReady(t *testing.T) {
	// Proves that sending select_agent for a valid agent yields a session_ready
	// response carrying the correct agent_id and a well-formed session key.
	srv := httptest.NewServer(Handler(testConfig()))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/voice"
	ws := dialWS(t, url)
	defer ws.Close()

	// Read connected.
	var connected ConnectedMsg
	readJSON(t, ws, &connected)

	// Select agent.
	sendJSON(t, ws, SelectAgentMsg{Type: "select_agent", AgentID: "agent1"})

	var ready SessionReadyMsg
	readJSON(t, ws, &ready)
	if ready.Type != "session_ready" {
		t.Errorf("type = %q, want %q", ready.Type, "session_ready")
	}
	if ready.AgentID != "agent1" {
		t.Errorf("agent_id = %q, want %q", ready.AgentID, "agent1")
	}
	if !strings.HasPrefix(ready.SessionKey, "agent1/c") {
		t.Errorf("session_key = %q, want prefix %q", ready.SessionKey, "agent1/c")
	}
}

func TestSelectAgent_UnknownAgent(t *testing.T) {
	// Proves that selecting a nonexistent agent returns an error message
	// mentioning the unknown agent ID instead of crashing or hanging.
	srv := httptest.NewServer(Handler(testConfig()))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/voice"
	ws := dialWS(t, url)
	defer ws.Close()

	var connected ConnectedMsg
	readJSON(t, ws, &connected)

	sendJSON(t, ws, SelectAgentMsg{Type: "select_agent", AgentID: "nonexistent"})

	var errMsg ErrorMsg
	readJSON(t, ws, &errMsg)
	if errMsg.Type != "error" {
		t.Errorf("type = %q, want %q", errMsg.Type, "error")
	}
	if !strings.Contains(errMsg.Message, "nonexistent") {
		t.Errorf("error should mention agent name: %q", errMsg.Message)
	}
}

func TestPingPong(t *testing.T) {
	// Proves that sending a ping message results in a pong response, confirming
	// the keepalive protocol is handled by the server.
	srv := httptest.NewServer(Handler(testConfig()))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/voice"
	ws := dialWS(t, url)
	defer ws.Close()

	var connected ConnectedMsg
	readJSON(t, ws, &connected)

	sendJSON(t, ws, map[string]string{"type": "ping"})

	var pong PongMsg
	readJSON(t, ws, &pong)
	if pong.Type != "pong" {
		t.Errorf("type = %q, want %q", pong.Type, "pong")
	}
}

func TestTextInput_FullPipeline(t *testing.T) {
	// Proves that sending a text message through an established agent session
	// produces the complete response sequence: response_start, response_text
	// (with the agent's reply), audio_start, binary audio chunk, audio_end,
	// and response_end — in that exact order.
	srv := httptest.NewServer(Handler(testConfig()))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/voice"
	ws := dialWS(t, url)
	defer ws.Close()

	// Connected.
	var connected ConnectedMsg
	readJSON(t, ws, &connected)

	// Select agent.
	sendJSON(t, ws, SelectAgentMsg{Type: "select_agent", AgentID: "agent1"})
	var ready SessionReadyMsg
	readJSON(t, ws, &ready)

	// Send text.
	sendJSON(t, ws, TextMsg{Type: "text", Content: "test input"})

	// response_start
	var start ResponseStartMsg
	readJSON(t, ws, &start)
	if start.Type != "response_start" {
		t.Errorf("type = %q, want %q", start.Type, "response_start")
	}

	// response_text
	var text ResponseTextMsg
	readJSON(t, ws, &text)
	if text.Type != "response_text" {
		t.Errorf("type = %q, want %q", text.Type, "response_text")
	}
	if text.Content != "response to: test input" {
		t.Errorf("content = %q, want %q", text.Content, "response to: test input")
	}
	if !text.Final {
		t.Error("final should be true")
	}

	// audio_start
	var audioStart AudioStartOutMsg
	readJSON(t, ws, &audioStart)
	if audioStart.Format != "mp3" {
		t.Errorf("format = %q, want %q", audioStart.Format, "mp3")
	}

	// Binary audio data.
	msgType, audioData := readRawMessage(t, ws)
	if msgType != websocket.BinaryMessage {
		t.Errorf("message type = %d, want binary (%d)", msgType, websocket.BinaryMessage)
	}
	if string(audioData) != "fake-mp3" {
		t.Errorf("audio = %q, want %q", string(audioData), "fake-mp3")
	}

	// audio_end
	var audioEnd AudioEndOutMsg
	readJSON(t, ws, &audioEnd)
	if audioEnd.Type != "audio_end" {
		t.Errorf("type = %q, want %q", audioEnd.Type, "audio_end")
	}

	// response_end
	var end ResponseEndMsg
	readJSON(t, ws, &end)
	if end.Type != "response_end" {
		t.Errorf("type = %q, want %q", end.Type, "response_end")
	}
}

func TestAudioFlow_FullPipeline(t *testing.T) {
	// Proves the end-to-end voice pipeline: audio_start + binary frames +
	// audio_end triggers STT transcription, produces a transcription message,
	// then drives the full response sequence including TTS audio output.
	srv := httptest.NewServer(Handler(testConfig()))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/voice"
	ws := dialWS(t, url)
	defer ws.Close()

	var connected ConnectedMsg
	readJSON(t, ws, &connected)

	sendJSON(t, ws, SelectAgentMsg{Type: "select_agent", AgentID: "agent1"})
	var ready SessionReadyMsg
	readJSON(t, ws, &ready)

	// Send audio_start + binary + audio_end.
	sendJSON(t, ws, AudioStartMsg{Type: "audio_start", Format: "opus", SampleRate: 48000})
	ws.WriteMessage(websocket.BinaryMessage, []byte("fake-opus-audio"))
	sendJSON(t, ws, map[string]string{"type": "audio_end"})

	// transcription
	var transcription TranscriptionMsg
	readJSON(t, ws, &transcription)
	if transcription.Text != "hello world" {
		t.Errorf("text = %q, want %q", transcription.Text, "hello world")
	}

	// response_start
	var start ResponseStartMsg
	readJSON(t, ws, &start)
	if start.Type != "response_start" {
		t.Errorf("type = %q, want %q", start.Type, "response_start")
	}

	// response_text
	var text ResponseTextMsg
	readJSON(t, ws, &text)
	if text.Content != "response to: hello world" {
		t.Errorf("content = %q, want %q", text.Content, "response to: hello world")
	}

	// audio_start + binary + audio_end
	var audioStart AudioStartOutMsg
	readJSON(t, ws, &audioStart)

	_, _ = readRawMessage(t, ws) // binary audio

	var audioEnd AudioEndOutMsg
	readJSON(t, ws, &audioEnd)

	// response_end
	var end ResponseEndMsg
	readJSON(t, ws, &end)
	if end.Type != "response_end" {
		t.Errorf("type = %q, want %q", end.Type, "response_end")
	}
}

func TestNoAgentSelected_Error(t *testing.T) {
	// Proves that sending a text message before selecting an agent returns an
	// error message rather than silently discarding the input or panicking.
	srv := httptest.NewServer(Handler(testConfig()))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/voice"
	ws := dialWS(t, url)
	defer ws.Close()

	var connected ConnectedMsg
	readJSON(t, ws, &connected)

	// Send text without selecting agent.
	sendJSON(t, ws, TextMsg{Type: "text", Content: "hello"})

	// Should get error.
	var errMsg ErrorMsg
	readJSON(t, ws, &errMsg)
	if !strings.Contains(errMsg.Message, "no agent selected") {
		t.Errorf("error = %q, want 'no agent selected'", errMsg.Message)
	}
}

func TestEmptyAudio_Error(t *testing.T) {
	// Proves that starting and immediately ending an audio session with no
	// binary frames results in an "empty audio" error rather than a transcription
	// attempt on zero bytes.
	srv := httptest.NewServer(Handler(testConfig()))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/voice"
	ws := dialWS(t, url)
	defer ws.Close()

	var connected ConnectedMsg
	readJSON(t, ws, &connected)

	sendJSON(t, ws, SelectAgentMsg{Type: "select_agent", AgentID: "agent1"})
	var ready SessionReadyMsg
	readJSON(t, ws, &ready)

	// Send audio_start + audio_end with no binary data.
	sendJSON(t, ws, AudioStartMsg{Type: "audio_start", Format: "opus", SampleRate: 48000})
	sendJSON(t, ws, map[string]string{"type": "audio_end"})

	var errMsg ErrorMsg
	readJSON(t, ws, &errMsg)
	if !strings.Contains(errMsg.Message, "empty audio") {
		t.Errorf("error = %q, want 'empty audio'", errMsg.Message)
	}
}

func TestSTTError(t *testing.T) {
	// Proves that an STT failure during transcription surfaces as a
	// "transcription failed" error message to the client rather than
	// silently dropping the response.
	cfg := testConfig(func(c *HandlerConfig) {
		c.STT = &mockSTT{err: fmt.Errorf("whisper down")}
	})
	srv := httptest.NewServer(Handler(cfg))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/voice"
	ws := dialWS(t, url)
	defer ws.Close()

	var connected ConnectedMsg
	readJSON(t, ws, &connected)

	sendJSON(t, ws, SelectAgentMsg{Type: "select_agent", AgentID: "agent1"})
	var ready SessionReadyMsg
	readJSON(t, ws, &ready)

	sendJSON(t, ws, AudioStartMsg{Type: "audio_start", Format: "opus", SampleRate: 48000})
	ws.WriteMessage(websocket.BinaryMessage, []byte("audio-data"))
	sendJSON(t, ws, map[string]string{"type": "audio_end"})

	var errMsg ErrorMsg
	readJSON(t, ws, &errMsg)
	if !strings.Contains(errMsg.Message, "transcription failed") {
		t.Errorf("error = %q, want 'transcription failed'", errMsg.Message)
	}
}

func TestTTSError_NonFatal(t *testing.T) {
	// Proves that a TTS synthesis failure is non-fatal: the text response still
	// reaches the client and the session terminates cleanly with response_end,
	// with no audio_start/audio_end emitted.
	cfg := testConfig(func(c *HandlerConfig) {
		c.AgentTTS = func(_ string) TTS { return &mockTTS{err: fmt.Errorf("tts broken")} }
	})
	srv := httptest.NewServer(Handler(cfg))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/voice"
	ws := dialWS(t, url)
	defer ws.Close()

	var connected ConnectedMsg
	readJSON(t, ws, &connected)

	sendJSON(t, ws, SelectAgentMsg{Type: "select_agent", AgentID: "agent1"})
	var ready SessionReadyMsg
	readJSON(t, ws, &ready)

	sendJSON(t, ws, TextMsg{Type: "text", Content: "test"})

	// response_start
	var start ResponseStartMsg
	readJSON(t, ws, &start)

	// response_text — should still come through
	var text ResponseTextMsg
	readJSON(t, ws, &text)
	if text.Content != "response to: test" {
		t.Errorf("content = %q, want %q", text.Content, "response to: test")
	}

	// response_end — no audio_start/audio_end when TTS fails
	var end ResponseEndMsg
	readJSON(t, ws, &end)
	if end.Type != "response_end" {
		t.Errorf("type = %q, want %q", end.Type, "response_end")
	}
}

func TestAudioChunking(t *testing.T) {
	// Proves that large TTS audio is split into multiple binary WebSocket frames
	// each not exceeding audioChunkSize, and that all bytes are delivered with
	// no loss, followed by audio_end.
	// Create audio data larger than audioChunkSize (4096).
	bigAudio := bytes.Repeat([]byte("x"), 10000)
	cfg := testConfig(func(c *HandlerConfig) {
		c.AgentTTS = func(_ string) TTS { return &mockTTS{result: bigAudio} }
	})
	srv := httptest.NewServer(Handler(cfg))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/voice"
	ws := dialWS(t, url)
	defer ws.Close()

	var connected ConnectedMsg
	readJSON(t, ws, &connected)

	sendJSON(t, ws, SelectAgentMsg{Type: "select_agent", AgentID: "agent1"})
	var ready SessionReadyMsg
	readJSON(t, ws, &ready)

	sendJSON(t, ws, TextMsg{Type: "text", Content: "test"})

	// response_start
	var start ResponseStartMsg
	readJSON(t, ws, &start)

	// response_text
	var text ResponseTextMsg
	readJSON(t, ws, &text)

	// audio_start
	var audioStart AudioStartOutMsg
	readJSON(t, ws, &audioStart)

	// Read binary chunks until audio_end.
	var totalBytes int
	var chunkCount int
	for {
		ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		msgType, data, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read chunk: %v", err)
		}
		if msgType == websocket.BinaryMessage {
			if len(data) > audioChunkSize {
				t.Errorf("chunk size %d exceeds max %d", len(data), audioChunkSize)
			}
			totalBytes += len(data)
			chunkCount++
		} else if msgType == websocket.TextMessage {
			// Should be audio_end.
			var audioEnd AudioEndOutMsg
			if err := json.Unmarshal(data, &audioEnd); err != nil {
				t.Fatalf("unmarshal audio_end: %v", err)
			}
			if audioEnd.Type != "audio_end" {
				t.Errorf("expected audio_end, got type=%q", audioEnd.Type)
			}
			break
		}
	}

	if totalBytes != len(bigAudio) {
		t.Errorf("total bytes = %d, want %d", totalBytes, len(bigAudio))
	}
	// 10000 bytes / 4096 = 3 chunks (4096 + 4096 + 1808).
	if chunkCount != 3 {
		t.Errorf("chunk count = %d, want 3", chunkCount)
	}

	// response_end
	var end ResponseEndMsg
	readJSON(t, ws, &end)
	if end.Type != "response_end" {
		t.Errorf("type = %q, want %q", end.Type, "response_end")
	}
}

func TestNilTTS_NoAudio(t *testing.T) {
	// Proves that when AgentTTS returns nil (TTS disabled for an agent), the
	// response completes without any audio messages — just text and response_end.
	cfg := testConfig(func(c *HandlerConfig) {
		c.AgentTTS = func(_ string) TTS { return nil }
	})
	srv := httptest.NewServer(Handler(cfg))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/voice"
	ws := dialWS(t, url)
	defer ws.Close()

	var connected ConnectedMsg
	readJSON(t, ws, &connected)

	sendJSON(t, ws, SelectAgentMsg{Type: "select_agent", AgentID: "agent1"})
	var ready SessionReadyMsg
	readJSON(t, ws, &ready)

	sendJSON(t, ws, TextMsg{Type: "text", Content: "test"})

	// response_start
	var start ResponseStartMsg
	readJSON(t, ws, &start)

	// response_text
	var text ResponseTextMsg
	readJSON(t, ws, &text)

	// response_end — no audio when TTS is nil
	var end ResponseEndMsg
	readJSON(t, ws, &end)
	if end.Type != "response_end" {
		t.Errorf("type = %q, want %q", end.Type, "response_end")
	}
}

func TestAudioBuffer_CappedAtMaxAudioBytes(t *testing.T) {
	// Proves the accumulated audio buffer cannot exceed MaxAudioBytes (P1-10):
	// streaming binary frames past the cap stops recording and surfaces an error
	// instead of growing memory without bound (DoS). With a small cap, two
	// frames that together exceed it must trigger the limit error.
	cfg := testConfig(func(c *HandlerConfig) {
		c.MaxAudioBytes = 16 // tiny cap for the test
	})
	srv := httptest.NewServer(Handler(cfg))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/voice"
	ws := dialWS(t, url)
	defer ws.Close()

	var connected ConnectedMsg
	readJSON(t, ws, &connected)
	sendJSON(t, ws, SelectAgentMsg{Type: "select_agent", AgentID: "agent1"})
	var ready SessionReadyMsg
	readJSON(t, ws, &ready)

	sendJSON(t, ws, AudioStartMsg{Type: "audio_start", Format: "opus", SampleRate: 48000})
	// Each frame is 10 bytes; the second pushes the total (20) over the 16-byte cap.
	ws.WriteMessage(websocket.BinaryMessage, []byte("0123456789"))
	ws.WriteMessage(websocket.BinaryMessage, []byte("0123456789"))

	var errMsg ErrorMsg
	readJSON(t, ws, &errMsg)
	if errMsg.Type != "error" || !strings.Contains(errMsg.Message, "audio") {
		t.Errorf("got %+v, want an audio-limit error", errMsg)
	}
}
