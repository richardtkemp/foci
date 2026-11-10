package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"foci/internal/config"
	"foci/internal/log"
)

// Deepgram Flux: streaming STT with built-in end-of-turn detection (#3018).
//
// The protocol is Deepgram's v2 listen WebSocket, as documented at
//   - https://developers.deepgram.com/reference/speech-to-text/listen-flux
//     (handshake, query parameters, every message shape)
//   - https://developers.deepgram.com/docs/flux/quickstart (raw linear16 at
//     16 kHz, 80 ms chunks recommended)
//   - https://developers.deepgram.com/docs/flux/state (TurnInfo events)
//   - https://developers.deepgram.com/docs/flux/close-stream (CloseStream)
//   - https://developers.deepgram.com/docs/flux/force-end-turn (ForceEndTurn)
//   - https://developers.deepgram.com/docs/flux/configuration (eot_threshold
//     0.5-1.0, default 0.7; eot_timeout_ms 500-60000, default 5000)
//
// In short: dial wss://api.deepgram.com/v2/listen?model=flux-general-en&
// encoding=linear16&sample_rate=16000 with "Authorization: Token <key>"; send
// audio as binary frames; receive JSON text frames: Connected, TurnInfo
// (event Update | StartOfTurn | EagerEndOfTurn | TurnResumed | EndOfTurn, with
// the turn's whole transcript so far), Warning (the connection stays open) and
// Error (fatal). {"type":"CloseStream"} ends the stream.

// Flux defaults, used when the [[stt]] entry leaves the field empty.
const (
	FluxDefaultEndpoint = config.STTFluxDefaultEndpoint
	FluxDefaultModel    = "flux-general-en"
)

// fluxDialTimeout bounds the WebSocket handshake.
const fluxDialTimeout = 15 * time.Second

// fluxEventBuffer is the depth of a stream's event channel. Partial events
// arrive a few times a second; the relay reads them at once.
const fluxEventBuffer = 32

// FluxSTT opens Deepgram Flux streams. Endpoint and Model fall back to the
// Flux defaults when empty.
type FluxSTT struct {
	Endpoint string
	APIKey   string
	Model    string
}

// fluxURL is the stream URL: the endpoint with the model, the audio shape and
// any end-of-turn settings as query parameters (listen-flux reference,
// "Query parameters"). A zero setting is left out, so Flux applies its own
// default.
func (f *FluxSTT) fluxURL(opts StreamOptions) (string, error) {
	endpoint := f.Endpoint
	if endpoint == "" {
		endpoint = FluxDefaultEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("flux endpoint: %w", err)
	}
	model := f.Model
	if model == "" {
		model = FluxDefaultModel
	}
	q := u.Query()
	q.Set("model", model)
	q.Set("encoding", StreamEncoding)
	q.Set("sample_rate", strconv.Itoa(StreamSampleRate))
	if opts.EOTThreshold != 0 {
		q.Set("eot_threshold", strconv.FormatFloat(opts.EOTThreshold, 'f', -1, 64))
	}
	if opts.EOTTimeoutMs != 0 {
		q.Set("eot_timeout_ms", strconv.Itoa(opts.EOTTimeoutMs))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// OpenStream dials Flux and starts reading its reports. The stream is open
// (and billed) until Close, a fatal provider error, or ctx ending.
func (f *FluxSTT) OpenStream(ctx context.Context, opts StreamOptions) (STTStream, error) {
	target, err := f.fluxURL(opts)
	if err != nil {
		return nil, err
	}
	hdr := http.Header{}
	if f.APIKey != "" {
		hdr.Set("Authorization", "Token "+f.APIKey)
	}
	dialCtx, cancel := context.WithTimeout(ctx, fluxDialTimeout)
	defer cancel()
	dialer := websocket.Dialer{HandshakeTimeout: fluxDialTimeout, Proxy: http.ProxyFromEnvironment}
	voiceLog.Debugf("stt stream open: model=%s key=%s", f.Model, log.FormatKeySuffix(f.APIKey))
	conn, resp, err := dialer.DialContext(dialCtx, target, hdr)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("flux handshake: HTTP %d: %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("flux dial: %w", err)
	}
	s := &fluxStream{
		conn:   conn,
		events: make(chan StreamEvent, fluxEventBuffer),
		done:   make(chan struct{}),
	}
	go s.readLoop()
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.done:
		}
	}()
	return s, nil
}

// fluxMessage is the part of a Flux server message foci reads. Flux sends
// more fields (sequence_id, words, audio_window_*, end_of_turn_confidence,
// trigger); none decide anything here.
type fluxMessage struct {
	Type        string `json:"type"`
	Event       string `json:"event"`
	Transcript  string `json:"transcript"`
	Code        string `json:"code"`
	Description string `json:"description"`
}

// fluxStream is one open Flux connection.
type fluxStream struct {
	conn    *websocket.Conn
	writeMu sync.Mutex // gorilla allows one concurrent writer

	events chan StreamEvent
	done   chan struct{} // closed by Close

	closeOnce sync.Once
	errMu     sync.Mutex
	err       error
}

func (s *fluxStream) Events() <-chan StreamEvent { return s.events }

func (s *fluxStream) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *fluxStream) setErr(err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

func (s *fluxStream) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *fluxStream) Send(pcm []byte) error {
	if s.closed() {
		return ErrStreamClosed
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return s.conn.WriteMessage(websocket.BinaryMessage, pcm)
}

// Finish sends ForceEndTurn (https://developers.deepgram.com/docs/flux/force-end-turn):
// Flux ends the current turn at once with an EndOfTurn (trigger "manual").
// With no turn active it answers with a Warning and nothing else.
func (s *fluxStream) Finish() error {
	if s.closed() {
		return ErrStreamClosed
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return s.conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"ForceEndTurn"}`))
}

// Close sends CloseStream (close-stream doc: Flux finishes the audio it has,
// then closes) and closes the connection without waiting for it: foci has
// already acted on the last end of turn, and the connection is billed while
// it stays open.
func (s *fluxStream) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.writeMu.Lock()
		_ = s.conn.SetWriteDeadline(time.Now().Add(time.Second))
		_ = s.conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"CloseStream"}`))
		s.writeMu.Unlock()
		_ = s.conn.Close()
	})
	return nil
}

// readLoop turns Flux messages into StreamEvents until the connection ends.
func (s *fluxStream) readLoop() {
	defer close(s.events)
	defer func() { _ = s.Close() }()
	for {
		mt, data, err := s.conn.ReadMessage()
		if err != nil {
			if !s.closed() {
				s.setErr(fmt.Errorf("flux read: %w", err))
			}
			return
		}
		if mt != websocket.TextMessage {
			continue
		}
		var m fluxMessage
		if err := json.Unmarshal(data, &m); err != nil {
			voiceLog.Warnf("stt stream: unreadable flux message (%d bytes): %v", len(data), err)
			continue
		}
		switch m.Type {
		case "TurnInfo":
			ev := StreamEvent{Kind: StreamPartial, Text: m.Transcript}
			if m.Event == "EndOfTurn" {
				ev.Kind = StreamEndOfTurn
			}
			select {
			case s.events <- ev:
			case <-s.done:
				return
			}
		case "Error":
			s.setErr(fmt.Errorf("flux error %s: %s", m.Code, m.Description))
			return
		case "Warning":
			voiceLog.Warnf("stt stream: flux warning %s: %s", m.Code, m.Description)
		default:
			// Connected, ConfigureSuccess, ConfigureFailure, and any type
			// Flux adds later: nothing to report.
		}
	}
}

// errFluxNoKey is returned when a Flux entry has no API key.
var errFluxNoKey = errors.New("no API key")

// NewStreamingSTT creates a streaming STT provider from an [[stt]] entry's
// values, or ok=false when format is not a streaming format.
func NewStreamingSTT(format, endpoint, apiKey, model string) (StreamingSTT, bool, error) {
	switch format {
	case config.STTFormatDeepgramFlux:
		if apiKey == "" {
			return nil, true, errFluxNoKey
		}
		return &FluxSTT{Endpoint: endpoint, APIKey: apiKey, Model: model}, true, nil
	default:
		return nil, false, nil
	}
}
