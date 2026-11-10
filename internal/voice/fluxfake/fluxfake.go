// Package fluxfake is a fake Deepgram Flux server for tests (#3018). No
// Deepgram key existed when the Flux client was written, so its tests run
// against this fake, built from Deepgram's documented v2 listen protocol:
//
//   - https://developers.deepgram.com/reference/speech-to-text/listen-flux
//     handshake (Authorization: Token <key>), query parameters and their valid
//     ranges, and the shape of every message (Connected, TurnInfo, Warning,
//     Error, CloseStream)
//   - https://developers.deepgram.com/docs/flux/state (StartOfTurn, Update,
//     EndOfTurn and its trigger field)
//   - https://developers.deepgram.com/docs/flux/close-stream (CloseStream: the
//     server finishes the audio it has, then closes the connection)
//   - https://developers.deepgram.com/docs/flux/configuration (eot_threshold
//     0.5-1.0, eot_timeout_ms 500-60000)
//   - https://developers.deepgram.com/docs/flux/force-end-turn (ForceEndTurn:
//     EndOfTurn with trigger "manual", or a Warning when no turn is active)
//
// The fake rejects what the reference says Flux rejects (a bad key, a missing
// or unknown model, raw audio without sample_rate, a threshold out of range),
// so a client that builds a wrong URL fails here first.
package fluxfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Server is one fake Flux endpoint. Set the fields before the first dial.
type Server struct {
	*httptest.Server

	// Key is the API key the fake accepts.
	Key string
	// Words is the turn the fake hears. After BytesPerWord bytes of audio
	// it hears the next word: the first word sends StartOfTurn, each later
	// word an Update, and after the last word comes EndOfTurn (trigger
	// "model"). Empty: the fake hears nothing (silence).
	Words        []string
	BytesPerWord int
	// FatalAfterBytes, when > 0, sends a fatal Error message once this much
	// audio has arrived, then closes the connection.
	FatalAfterBytes int

	mu          sync.Mutex
	queries     []url.Values
	audioBytes  int
	closeStream bool
	conns       int
	closed      chan struct{} // closed when a connection ends
}

// New starts a fake Flux server that accepts key.
func New(key string) *Server {
	s := &Server{Key: key, BytesPerWord: 3200, closed: make(chan struct{}, 16)}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// URL is the ws:// endpoint to configure a Flux client with.
func (s *Server) URL() string { return "ws" + strings.TrimPrefix(s.Server.URL, "http") + "/v2/listen" }

// Queries is every handshake's query, in order.
func (s *Server) Queries() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.queries...)
}

// AudioBytes is the total audio received over every connection.
func (s *Server) AudioBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audioBytes
}

// GotCloseStream reports whether a client sent CloseStream.
func (s *Server) GotCloseStream() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeStream
}

// Conns is the number of accepted connections.
func (s *Server) Conns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

// WaitConnEnd waits for one connection to end, or d to pass. It reports
// whether a connection ended.
func (s *Server) WaitConnEnd(d time.Duration) bool {
	select {
	case <-s.closed:
		return true
	case <-time.After(d):
		return false
	}
}

// validate applies the reference's handshake rules. It returns the HTTP
// status and reason for a rejected handshake, or 0.
func (s *Server) validate(r *http.Request) (int, string) {
	if r.Header.Get("Authorization") != "Token "+s.Key {
		return http.StatusUnauthorized, "invalid credentials"
	}
	q := r.URL.Query()
	switch q.Get("model") {
	case "flux-general-en", "flux-general-multi":
	default:
		return http.StatusBadRequest, "model must be flux-general-en or flux-general-multi"
	}
	enc := q.Get("encoding")
	switch enc {
	case "", "linear16", "linear32", "mulaw", "alaw", "opus", "ogg-opus":
	default:
		return http.StatusBadRequest, "unknown encoding"
	}
	if enc != "" && q.Get("sample_rate") == "" {
		return http.StatusBadRequest, "raw audio needs sample_rate"
	}
	if v := q.Get("eot_threshold"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0.5 || f > 1.0 {
			return http.StatusBadRequest, "eot_threshold must be between 0.5 and 1"
		}
	}
	if v := q.Get("eot_timeout_ms"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 500 || n > 60000 {
			return http.StatusBadRequest, "eot_timeout_ms must be between 500 and 60000"
		}
	}
	return 0, ""
}

var upgrader = websocket.Upgrader{}

const requestID = "550e8400-e29b-41d4-a716-446655440000"

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if code, why := s.validate(r); code != 0 {
		http.Error(w, why, code)
		return
	}
	s.mu.Lock()
	s.queries = append(s.queries, r.URL.Query())
	s.conns++
	words, perWord, fatalAt := s.Words, s.BytesPerWord, s.FatalAfterBytes
	s.mu.Unlock()
	if perWord <= 0 {
		perWord = 1
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() {
		_ = conn.Close()
		s.closed <- struct{}{}
	}()

	seq := 0
	send := func(m map[string]any) bool {
		m["request_id"] = requestID
		m["sequence_id"] = seq
		seq++
		b, _ := json.Marshal(m)
		return conn.WriteMessage(websocket.TextMessage, b) == nil
	}
	if !send(map[string]any{"type": "Connected"}) {
		return
	}

	heard, got := 0, 0
	turnInfo := func(event string, n int) bool {
		m := map[string]any{
			"type":                   "TurnInfo",
			"event":                  event,
			"turn_index":             0,
			"audio_window_start":     0,
			"audio_window_end":       float64(got) / 32000,
			"transcript":             strings.Join(words[:n], " "),
			"words":                  wordList(words[:n]),
			"end_of_turn_confidence": 0.1,
		}
		if event == "EndOfTurn" {
			m["end_of_turn_confidence"] = 0.9
			m["trigger"] = "model"
		}
		return send(m)
	}
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt == websocket.TextMessage {
			var m struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(data, &m)
			if m.Type == "ForceEndTurn" {
				// force-end-turn doc: the current turn ends at once with
				// EndOfTurn, trigger "manual"; with no turn active, a Warning.
				if heard == 0 || heard == len(words) {
					if !send(map[string]any{"type": "Warning", "code": "FORCE_END_TURN_NO_ACTIVE_TURN", "description": "Received ForceEndTurn while no turn was active; the request was ignored."}) {
						return
					}
					continue
				}
				m := map[string]any{
					"type": "TurnInfo", "event": "EndOfTurn", "turn_index": 0,
					"audio_window_start": 0, "audio_window_end": float64(got) / 32000,
					"transcript": strings.Join(words[:heard], " "), "words": wordList(words[:heard]),
					"end_of_turn_confidence": 0.3, "trigger": "manual",
				}
				if !send(m) {
					return
				}
				heard = len(words) // the turn is over: nothing more is heard
				continue
			}
			if m.Type == "CloseStream" {
				s.mu.Lock()
				s.closeStream = true
				s.mu.Unlock()
				// close-stream doc: the server finishes the audio it has and
				// closes, with no EndOfTurn.
				return
			}
			continue
		}
		got += len(data)
		s.mu.Lock()
		s.audioBytes += len(data)
		s.mu.Unlock()
		if fatalAt > 0 && got >= fatalAt {
			send(map[string]any{"type": "Error", "code": "INTERNAL_SERVER_ERROR", "description": "An internal server error occurred while processing the request"})
			return
		}
		for heard < len(words) && got >= (heard+1)*perWord {
			heard++
			event := "Update"
			if heard == 1 {
				event = "StartOfTurn"
			}
			if !turnInfo(event, heard) {
				return
			}
			if heard == len(words) && !turnInfo("EndOfTurn", heard) {
				return
			}
		}
	}
}

func wordList(ws []string) []map[string]any {
	out := make([]map[string]any, 0, len(ws))
	for i, w := range ws {
		out = append(out, map[string]any{"word": w, "confidence": 0.95, "start": float64(i) * 0.3, "end": float64(i)*0.3 + 0.25})
	}
	return out
}

// String names the fake in test logs.
func (s *Server) String() string { return fmt.Sprintf("fluxfake(%s)", s.URL()) }
