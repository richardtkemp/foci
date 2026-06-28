// subscriber.go — SSE subscriber for the opencode-server's /event stream.
//
// One Subscriber goroutine exists per Server, started by Server.Start
// after the health probe succeeds. It opens a long-lived HTTP connection
// to GET /event, parses Server-Sent Events frames, and dispatches each
// decoded rawEvent to Server.route — which looks up the target Backend
// by sessionID and pushes the event onto its per-session channel.
//
// SSE wire format (per the HTML spec / whatwg, what opencode implements):
//
//	<field>: <value>\n    one line per field; "data:" is the JSON payload
//	\n                   blank line terminates a frame
//	:data\n               comment line (heartbeat); ignore content
//
// Multiple "data:" lines in one frame have their values joined with "\n"
// before being emitted as a single event payload.
//
// No third-party SSE library — the format is small and a hand-rolled
// parser avoids a new dep + gives us direct control over the heartbeat /
// reconnect semantics that an off-the-shelf lib might not expose.

package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"foci/internal/log"
)

// eventBufferSize is the per-Backend channel capacity. 256 is generous
// (opencode events are small); if a Backend's dispatcher goroutine stalls
// and the channel fills, the subscriber drops new events for that session
// rather than blocking the whole SSE reader.
const eventBufferSize = 256

// Subscriber parses an SSE byte stream and invokes a callback per decoded
// event. It owns nothing besides the parser state; the caller owns the
// io.Reader (typically an HTTP response body) and is responsible for
// closing it after Run returns.
//
// Step 4 split: Run is the parsing loop. The HTTP GET /event wiring
// lives in Server.runSubscriber; the per-Backend channel push lives in
// Server.route. Subscriber stays focused on "bytes → events" so it
// can be tested against net.Pipe / strings.Reader without spinning up
// HTTP.
type Subscriber struct {
	r       io.Reader
	onEvent func(rawEvent)
	onHeartbeat func()
}

// NewSubscriber constructs a Subscriber that reads SSE frames from r,
// invoking onEvent for each decoded event and onHeartbeat for each
// comment line (heartbeat). Callbacks are invoked synchronously from
// Run's goroutine — callers that need async dispatch should push to a
// channel from onEvent (which is exactly what Server.route does).
func NewSubscriber(r io.Reader, onEvent func(rawEvent), onHeartbeat func()) *Subscriber {
	return &Subscriber{r: r, onEvent: onEvent, onHeartbeat: onHeartbeat}
}

// Run is the blocking parse loop. Returns when the reader reaches EOF,
// the context is cancelled, or a read error occurs. The caller receives
// the terminating error (io.EOF on clean shutdown, ctx.Err on cancel,
// or the underlying read error).
//
// Run does NOT call OnSubscriberStopped — that's Server.runSubscriber's
// job, so it can fire finalizeExit exactly once across all exit paths.
func (sub *Subscriber) Run(ctx context.Context) error {
	scanner := bufio.NewScanner(sub.r)
	// opencode's events are small JSON blobs, but a single frame may
	// carry a large tool_result snippet. Match the ccstream reader cap
	// (1 MiB) so a chatty server doesn't blow the default 64 KiB and
	// silently wedge the subscriber.
	const maxSize = 1 << 20
	scanner.Buffer(make([]byte, 0, 64*1024), maxSize)

	var dataLines []string // accumulates "data:" lines for the current frame

	for {
		// Check ctx before blocking on the next line. A cancelled
		// subscriber should exit promptly even if the server is silent.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if !scanner.Scan() {
			err := scanner.Err()
			if err == nil {
				return io.EOF
			}
			return err
		}
		line := scanner.Text()

		// Blank line terminates the current frame (if any).
		if line == "" {
			if len(dataLines) > 0 {
				payload := strings.Join(dataLines, "\n")
				dataLines = dataLines[:0]
				if ev, ok := decodeEvent(payload); ok {
					sub.onEvent(ev)
				}
			}
			continue
		}

		// Comment / heartbeat line — leading ':'.
		if line[0] == ':' {
			if sub.onHeartbeat != nil {
				sub.onHeartbeat()
			}
			continue
		}

		// Field line "<field>:<value>" or "<field>: <value>" (single
		// optional space after colon per the WHATWG spec). We only
		// consume "data"; "event"/"id"/"retry" are unused by opencode.
		field, value, ok := splitSSEField(line)
		if !ok {
			continue // malformed line; ignore (defensive — server is well-behaved)
		}
		if field == "data" {
			dataLines = append(dataLines, value)
		}
		// Other fields (event, id, retry) are ignored — opencode's
		// stream uses only data:.
	}
}

// splitSSEField splits "field:value" or "field: value" at the first
// colon, returning (field, value, ok). ok=false if there's no colon.
// Per WHATWG: a single leading space in value is stripped.
func splitSSEField(line string) (field, value string, ok bool) {
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return "", "", false
	}
	field = line[:idx]
	value = line[idx+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return field, value, true
}

// decodeEvent parses a frame's joined data: payload as JSON into rawEvent.
// Returns ok=false if the payload is empty or not valid JSON — the
// subscriber silently drops such frames rather than tearing down the
// whole stream (opencode occasionally emits empty data lines that
// shouldn't kill the subscription).
func decodeEvent(payload string) (rawEvent, bool) {
	if payload == "" {
		return rawEvent{}, false
	}
	var ev rawEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return rawEvent{}, false
	}
	if ev.Type == "" {
		return rawEvent{}, false
	}
	return ev, true
}

// ---------------------------------------------------------------------------
// Server-owned subscriber loop
// ---------------------------------------------------------------------------

// runSubscriber is the goroutine launched by Server.Start (Step 4) that
// owns the GET /event HTTP connection for the Server's lifetime. It
// connects once the subprocess is up (Start has health-probed already),
// parses the SSE stream, and routes each decoded event via Server.route.
//
// On exit (clean EOF, transport error, or ctx cancel from Close), it
// invokes OnSubscriberStopped — which calls finalizeExit exactly once,
// so this goroutine racing the subprocess-waiter goroutine is safe.
//
// Reconnect logic is intentionally absent for v1: per the plan's
// "Steer (mid-turn injection)" + "How it works" notes, a dropped
// subscriber means the Server is dead and the per-session turns need to
// surface that. Auto-reconnect is future work.
func (s *Server) runSubscriber(ctx context.Context) {
	component := s.logComponent()
	url := s.baseURL + "/event"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Warnf(component, "subscriber: build request: %v", err)
		s.OnSubscriberStopped(err)
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	// Long timeout on the client — the SSE response is unbounded; we
	// rely on ctx for cancellation, not the client timeout.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Warnf(component, "subscriber: connect: %v", err)
		s.OnSubscriberStopped(err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		log.Warnf(component, "subscriber: %s returned %d", url, resp.StatusCode)
		s.OnSubscriberStopped(fmt.Errorf("subscriber: HTTP %d", resp.StatusCode))
		return
	}

	onEvent := func(ev rawEvent) {
		s.lastActivity.Store(time.Now().UnixNano())
		s.route(ev)
	}
	onHeartbeat := func() {
		s.lastActivity.Store(time.Now().UnixNano())
	}

	sub := NewSubscriber(resp.Body, onEvent, onHeartbeat)
	err = sub.Run(ctx)
	if err == io.EOF {
		log.Infof(component, "subscriber: end of stream")
	} else {
		log.Warnf(component, "subscriber: stopped: %v", err)
	}
	s.OnSubscriberStopped(err)
}

// route delivers an event to the Backend registered under the event's
// sessionID. Events whose sessionID has no registered Backend (early
// race, global events, sessions we've already deregistered) are dropped
// silently.
//
// Events for a Backend whose channel is full are also dropped (with a
// loud WARN log) rather than blocking the SSE reader — a stuck Backend
// must not wedge the whole subscription. Tune eventBufferSize if drops
// show up in production.
func (s *Server) route(ev rawEvent) {
	sid := extractSessionID(ev.Properties)
	if sid == "" {
		return // global event (server.connected, tui.*, etc.) — no per-session target
	}

	s.sessionsMu.RLock()
	be := s.sessions[sid]
	s.sessionsMu.RUnlock()
	if be == nil {
		return
	}

	select {
	case be.events <- ev:
	default:
		// Channel full. Step 4.4 decision: drop rather than block. The
		// dispatcher goroutine is wedged; Step 7's handlers will surface
		// that via the missing-event gap in the session's state.
		log.Warnf(s.logComponent(), "event channel full for session %s; dropping %s", sid, ev.Type)
	}
}

// registerSession adds be to the per-Server session registry under
// be.sessionID. Called by Backend.Start (Step 5) after the session has
// been created via POST /session. Safe to call concurrently with route
// — the RWMutex is the synchronisation point.
//
// Returns the channel the Server will push events to; the Backend owns
// the receive side and drains it via its dispatcher goroutine (Step 7).
func (s *Server) registerSession(be *Backend) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if be.events == nil {
		be.events = make(chan rawEvent, eventBufferSize)
	}
	s.sessions[be.sessionID] = be
}

// unregisterSession removes the Backend registered under sessionID, if
// any. Called by Backend.Close (Step 5) before the Backend tears down.
// Safe to call when no session was registered (idempotent).
func (s *Server) unregisterSession(sessionID string) {
	if sessionID == "" {
		return
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	delete(s.sessions, sessionID)
}

// extractSessionID pulls the sessionID out of an event's Properties
// JSON. sessionID appears at one of three places depending on event type:
//
//   - top-level (session.idle, session.status, session.compacted,
//     session.error, message.part.removed, permission.replied)
//   - inside .part      (message.part.updated)
//   - inside .info      (message.updated)
//   - inside .permission (permission.updated)
//
// Events without any sessionID (server.connected, tui.*, installation.*,
// file.*, vcs.*) return "" — those are global and have no per-session
// routing target; route() drops them.
//
// Cost: two json.Unmarshal attempts per event (one if top-level matches).
// Events are small, so this is cheap relative to the round-trip.
func extractSessionID(props []byte) string {
	if len(props) == 0 {
		return ""
	}
	// Try top-level first — the common case (most session-scoped events).
	var top struct {
		SessionID string `json:"sessionID"`
	}
	if err := json.Unmarshal(props, &top); err == nil && top.SessionID != "" {
		return top.SessionID
	}
	// Nested — decode the three known wrapper keys in one pass.
	var nested struct {
		Part struct {
			SessionID string `json:"sessionID"`
		} `json:"part"`
		Info struct {
			SessionID string `json:"sessionID"`
		} `json:"info"`
		Permission struct {
			SessionID string `json:"sessionID"`
		} `json:"permission"`
	}
	if err := json.Unmarshal(props, &nested); err != nil {
		return ""
	}
	switch {
	case nested.Part.SessionID != "":
		return nested.Part.SessionID
	case nested.Info.SessionID != "":
		return nested.Info.SessionID
	case nested.Permission.SessionID != "":
		return nested.Permission.SessionID
	}
	return ""
}
