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
	"errors"
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

// subscriberConnectRetryInterval is the tick between initial-connect
// attempts in runSubscriber. The subscriber goroutine is launched before
// the subprocess has bound its port (so we don't miss server.connected),
// so the first GET /event reliably gets "connection refused" for the ~1s
// the server takes to come up. We retry on this cadence until the stream
// establishes or the loop is cancelled (ctx / subprocess death).
const subscriberConnectRetryInterval = 100 * time.Millisecond

// subscriberHeaderTimeout bounds how long the initial GET /event connect waits
// for the server's response headers. It is deliberately a ResponseHeaderTimeout
// (Transport-level), NOT an http.Client.Timeout: the latter caps the whole
// request including the SSE body read, which would sever an established stream;
// ResponseHeaderTimeout covers ONLY the header wait and leaves the long-lived
// body stream unbounded. Without it, a connect that reaches a booting opencode
// which accepts the TCP connection but never sends headers wedges forever
// (client.Do has no deadline, only unblocks on ctx-cancel) — the retry loop
// never gets a chance to retry, no SSE stream establishes, no session.idle ever
// arrives, and the delegated turn hangs until a restart (foci bug #1051's
// trigger). 10s is pure margin: readiness is already gated by a 1-2s health
// probe, so once opencode is up it sends headers within milliseconds; a wedged
// connect instead errors after 10s and the loop retries. A var (not const) so
// tests can shorten it, mirroring sigtermGrace.
var subscriberHeaderTimeout = 10 * time.Second

// Subscriber parses an SSE byte stream and invokes a callback per decoded
// event. It owns nothing besides the parser state; the caller owns the
// io.Reader (typically an HTTP response body) and is responsible for
// closing it after Run returns.
//
// Run is the parsing loop. The HTTP GET /event wiring lives in
// Server.runSubscriber; the per-Backend channel push lives in
// Server.route. Subscriber stays focused on "bytes → events" so it
// can be tested against net.Pipe / strings.Reader without spinning up
// HTTP.
type Subscriber struct {
	r           io.Reader
	onEvent     func(rawEvent)
	onHeartbeat func()
	component   string // log tag; set by Server.runSubscriber (defaults to "opencode")
}

// NewSubscriber constructs a Subscriber that reads SSE frames from r,
// invoking onEvent for each decoded event and onHeartbeat for each
// comment line (heartbeat). Callbacks are invoked synchronously from
// Run's goroutine — callers that need async dispatch should push to a
// channel from onEvent (which is exactly what Server.route does).
func NewSubscriber(r io.Reader, onEvent func(rawEvent), onHeartbeat func()) *Subscriber {
	return &Subscriber{r: r, onEvent: onEvent, onHeartbeat: onHeartbeat, component: "opencode"}
}

// Run is the blocking parse loop. Returns when the reader reaches EOF,
// the context is cancelled, or a read error occurs. The caller receives
// the terminating error (io.EOF on clean shutdown, ctx.Err on cancel,
// or the underlying read error).
//
// Run does NOT call OnSubscriberStopped — that's Server.runSubscriber's
// job, so it can fire finalizeExit exactly once across all exit paths.
func (sub *Subscriber) Run(ctx context.Context) error {
	// opencode 1.17.11 can emit a single SSE frame well over 1 MiB (large
	// tool_result / message.part payloads). A bufio.Scanner errors terminally
	// once a token exceeds its cap, and here a dropped stream tears down the
	// whole Server and errors every one of the agent's sessions (#972). Read
	// with a growable bufio.Reader under a hard ceiling instead: an oversized
	// line is logged and skipped (resyncing to the next frame) so one giant
	// event can't kill the stream.
	const maxLine = 16 << 20 // 16 MiB hard ceiling per SSE line
	reader := bufio.NewReaderSize(sub.r, 64*1024)

	var dataLines []string // accumulates "data:" lines for the current frame

	for {
		// Check ctx before blocking on the next line. A cancelled
		// subscriber should exit promptly even if the server is silent.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		lineBytes, err := readLine(reader, maxLine)
		if errors.Is(err, errLineTooLong) {
			log.Warnf(sub.component, "SSE line exceeded %d bytes — dropping oversized frame and resyncing to next event", maxLine)
			dataLines = dataLines[:0] // partial frame is untrustworthy; restart at the next boundary
			continue
		}
		if err != nil {
			return err // io.EOF on clean close, or the underlying read error
		}
		line := string(lineBytes)

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

var errLineTooLong = errors.New("opencode: SSE line exceeds ceiling")

// readLine reads one '\n'-terminated line from r under a hard byte ceiling,
// returning it with the trailing newline (and optional CR) stripped. If the
// line would exceed max, readLine drains the rest of the line so the reader
// resyncs to the next frame, and returns errLineTooLong. A final line at EOF
// without a trailing newline is dropped (an unterminated SSE frame is never
// flushed anyway), surfacing io.EOF. Read errors propagate as-is.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(buf)+len(chunk) > max {
				if derr := drainLine(r); derr != nil {
					return nil, derr
				}
				return nil, errLineTooLong
			}
			buf = append(buf, chunk...) // copy: chunk aliases r's internal buffer
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(buf)+len(chunk) > max {
			return nil, errLineTooLong // full line already consumed through '\n'
		}
		buf = append(buf, chunk...)
		return trimEOL(buf), nil
	}
}

// drainLine discards bytes until the end of the current line ('\n') or a read
// error, so the reader resyncs after an over-ceiling line.
func drainLine(r *bufio.Reader) error {
	for {
		if _, err := r.ReadSlice('\n'); !errors.Is(err, bufio.ErrBufferFull) {
			return err // nil once '\n' is consumed; otherwise the read error
		}
	}
}

// trimEOL strips a trailing '\n' and an optional preceding '\r'.
func trimEOL(b []byte) []byte {
	n := len(b)
	if n > 0 && b[n-1] == '\n' {
		n--
		if n > 0 && b[n-1] == '\r' {
			n--
		}
	}
	return b[:n]
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

// runSubscriber is the goroutine launched by Server.Start that
// owns the GET /event HTTP connection for the Server's lifetime. It is
// launched BEFORE the health probe completes (so we don't miss
// server.connected), parses the SSE stream, and routes each decoded event
// via Server.route.
//
// On exit (clean EOF, transport error, or ctx cancel from Close), it
// invokes OnSubscriberStopped — which calls finalizeExit exactly once,
// so this goroutine racing the subprocess-waiter goroutine is safe.
//
// The INITIAL connect retries through the subprocess startup window: the
// server binds its port ~1s after launch, so the first GET /event gets
// "connection refused" until it's ready. We retry on a fixed tick
// (subscriberConnectRetryInterval), bounded by ctx cancellation (Close
// cancels the subscriber ctx when the health probe fails) and s.done
// (subprocess death — the waiter goroutine owns finalizeExit). Once the
// SSE stream is established (HTTP 200), behaviour is unchanged.
//
// Mid-stream reconnect is intentionally absent: once established, a
// dropped stream means the Server is effectively dead and the per-session
// turns need to surface that. Auto-reconnect of an established stream is
// deferred future work.
func (s *Server) runSubscriber(ctx context.Context) {
	component := s.logComponent()
	url := s.baseURL + "/event"

	// Initial-connect retry loop. Retries on connection error OR non-200
	// until the stream establishes, or the loop is cancelled. Quiet by
	// design: one DEBUG on the first failure, not a WARN per attempt (the
	// per-attempt failures during the boot window would otherwise flood
	// the log).
	client := &http.Client{
		Transport: &http.Transport{ResponseHeaderTimeout: subscriberHeaderTimeout},
	}
	var resp *http.Response
	loggedRetry := false
	for {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if rerr != nil {
			log.Warnf(component, "subscriber: build request: %v", rerr)
			s.OnSubscriberStopped(rerr)
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")

		r, derr := client.Do(req)
		if derr == nil && r.StatusCode == http.StatusOK {
			resp = r
			break
		}
		if r != nil {
			_ = r.Body.Close()
		}
		if !loggedRetry {
			if derr != nil {
				log.Debugf(component, "subscriber: not ready (%v), retrying every %s", derr, subscriberConnectRetryInterval)
			} else {
				log.Debugf(component, "subscriber: %s returned %d, retrying every %s", url, r.StatusCode, subscriberConnectRetryInterval)
			}
			loggedRetry = true
		}

		select {
		case <-ctx.Done():
			// Close cancelled us (e.g. health probe failed) — surface the
			// ctx error and stop. Quiet exit; OnSubscriberStopped logs.
			s.OnSubscriberStopped(ctx.Err())
			return
		case <-s.done:
			// Subprocess actually died — the waiter goroutine owns
			// finalizeExit. Surface the last connect error (nil-safe).
			s.OnSubscriberStopped(derr)
			return
		case <-time.After(subscriberConnectRetryInterval):
			// Retry.
		}
	}
	defer func() { _ = resp.Body.Close() }()

	onEvent := func(ev rawEvent) {
		s.lastActivity.Store(time.Now().UnixNano())
		log.Debugf(component, "SSE event: %s session=%s", ev.Type, extractSessionID(ev.Properties))
		s.route(ev)
	}
	onHeartbeat := func() {
		s.lastActivity.Store(time.Now().UnixNano())
	}

	sub := NewSubscriber(resp.Body, onEvent, onHeartbeat)
	sub.component = component
	err := sub.Run(ctx)
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
// silently. Global events get a DEBUG log; server.connected gets INFO
// (plan §4.1: "first event is server.connected — log and ignore").
//
// Events for a Backend whose channel is full are also dropped (with a
// loud WARN log) rather than blocking the SSE reader — a stuck Backend
// must not wedge the whole subscription. Tune eventBufferSize if drops
// show up in production.
func (s *Server) route(ev rawEvent) {
	// Learn child→parent links from session.created so a subagent's
	// permission requests can be resolved to the owning Backend below.
	// session.created carries the new session in .info (with .parentID) and
	// has no top-level sessionID, so it falls through to the global-drop —
	// we capture the link first, then let it drop as before.
	if ev.Type == EventSessionCreated {
		s.recordSessionParent(ev.Properties)
	}

	// Track task tool lifecycle so recordSessionParent can assign child
	// sessions to the right tool call. Runs before the event reaches the
	// dispatcher, preserving the tool-start → session.created ordering.
	if ev.Type == EventMessagePartUpdated {
		s.trackTaskTool(ev.Properties)
	}

	sid := extractSessionID(ev.Properties)
	if sid == "" {
		// Global event (server.connected, tui.*, file.*, vcs.*) — no
		// per-session target. server.connected is the subscription's
		// first frame; log it at INFO so operators see "subscribed"
		// in the gateway log. Everything else logs at DEBUG only.
		if ev.Type == EventServerConnected {
			log.Infof(s.logComponent(), "subscriber: server.connected")
		} else {
			log.Debugf(s.logComponent(), "global event: %s", ev.Type)
		}
		return
	}

	s.sessionsMu.RLock()
	be := s.sessions[sid]
	s.sessionsMu.RUnlock()
	if be == nil {
		// No Backend for this session. opencode spawns child (subagent)
		// sessions that foci never registers; their PERMISSION requests would
		// otherwise be dropped here, blocking the subagent — and the parent
		// turn waiting on it — forever. Resolve the child up its parent chain
		// to the owning Backend and route the permission there so the human
		// can answer it. The permission's session ID is the child's, but the
		// reply endpoint (POST /permission/{id}/reply) is keyed by permission
		// ID, so the parent Backend can answer it. Child text events are
		// also routed to the parent — tagged with childCallID so handleEvent
		// can deliver them via OnSubagentText without polluting turn state.
		if isPermissionEvent(ev.Type) {
			if pbe := s.resolveParentBackend(sid); pbe != nil {
				log.Debugf(s.logComponent(), "routing child session %s %s to parent session %s", sid, ev.Type, pbe.sessionID)
				be = pbe
			} else if s.http != nil && s.baseURL != "" {
				// No childToParent link for this child — its session.created was
				// missed (fired before this subscriber connected, or dropped when
				// the event channel was full). Fetch the parent chain via GET
				// /session OFF the reader goroutine (HTTP must not block the SSE
				// stream) and re-route once resolved (#969).
				go s.resolveChildPermissionViaAPI(sid, ev)
				return
			}
		}
		if be == nil && isChildTextEvent(ev.Type) {
			if pbe := s.resolveParentBackend(sid); pbe != nil {
				if callID := s.callIDForChild(sid); callID != "" {
					ev.childCallID = callID
					be = pbe
				}
			}
		}
		if be == nil {
			return
		}
	}

	select {
	case be.events <- ev:
	default:
		// Channel full — drop rather than block. The dispatcher goroutine
		// is wedged; handlers.go will surface that via the missing-event
		log.Warnf(s.logComponent(), "event channel full for session %s; dropping %s", sid, ev.Type)
	}
}

// isPermissionEvent reports whether an event type is a permission lifecycle
// event — child-session events route() reroutes to a parent Backend.
func isPermissionEvent(t string) bool {
	return t == EventPermissionAsked || t == EventPermissionUpdated || t == EventPermissionReplied
}

// isChildTextEvent reports whether an event type carries text content from a
// subagent that should be surfaced on the parent session via OnSubagentText.
func isChildTextEvent(t string) bool {
	return t == EventMessagePartUpdated
}

// trackTaskTool inspects a message.part.updated event for task tool
// lifecycle, maintaining the pendingTaskCalls FIFO per parent session so
// recordSessionParent can assign child sessions to the right tool call.
// Runs synchronously in the subscriber goroutine, ahead of the dispatcher,
// so the ordering is preserved: tool-start → session.created → child text.
func (s *Server) trackTaskTool(props json.RawMessage) {
	var p eventMessagePartUpdated
	if err := json.Unmarshal(props, &p); err != nil {
		return
	}
	if p.Part.Type != PartTool || p.Part.Tool != taskTool {
		return
	}
	sid := p.Part.SessionID
	if sid == "" || p.Part.CallID == "" || p.Part.State == nil {
		return
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	switch p.Part.State.Status {
	case ToolStateRunning, ToolStatePending:
		s.pendingTaskCalls[sid] = append(s.pendingTaskCalls[sid], p.Part.CallID)
	case ToolStateCompleted, ToolStateError:
		calls := s.pendingTaskCalls[sid]
		for i, c := range calls {
			if c == p.Part.CallID {
				s.pendingTaskCalls[sid] = append(calls[:i], calls[i+1:]...)
				break
			}
		}
	}
}

// callIDForChild looks up the parent tool callID assigned to a child session.
func (s *Server) callIDForChild(childSID string) string {
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()
	return s.childToCallID[childSID]
}

// recordSessionParent records a child→parent session link from a
// session.created event's .info (a Session with .id and .parentID). No-op if
// the payload lacks either id or parentID (top-level/root sessions have no
// parent). Lazily allocates the map so Server literals in tests work.
//
// Also resolves the child→callID link: the parent session's oldest pending
// task tool call (FIFO from trackTaskTool) is assigned to this child, so
// child text events can be grouped with the matching OnSubagentStart/End.
func (s *Server) recordSessionParent(props []byte) {
	var p struct {
		Info struct {
			ID       string `json:"id"`
			ParentID string `json:"parentID"`
		} `json:"info"`
	}
	if err := json.Unmarshal(props, &p); err != nil || p.Info.ID == "" || p.Info.ParentID == "" {
		return
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if s.childToParent == nil {
		s.childToParent = make(map[string]string)
	}
	s.childToParent[p.Info.ID] = p.Info.ParentID

	// Assign the parent's oldest pending task call to this child session.
	if calls := s.pendingTaskCalls[p.Info.ParentID]; len(calls) > 0 {
		if s.childToCallID == nil {
			s.childToCallID = make(map[string]string)
		}
		s.childToCallID[p.Info.ID] = calls[0]
		s.pendingTaskCalls[p.Info.ParentID] = calls[1:]
	}
}

// resolveParentBackend walks a child session ID up its parent chain
// (childToParent) and returns the first ancestor that has a registered
// Backend, or nil if none is found. Bounded depth + a visited set guard
// against cycles or a pathologically deep chain.
func (s *Server) resolveParentBackend(sid string) *Backend {
	const maxParentDepth = 16
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()
	seen := make(map[string]bool, maxParentDepth)
	cur := sid
	for i := 0; i < maxParentDepth && cur != "" && !seen[cur]; i++ {
		seen[cur] = true
		parent, ok := s.childToParent[cur]
		if !ok {
			return nil
		}
		if be := s.sessions[parent]; be != nil {
			return be
		}
		cur = parent
	}
	return nil
}

// resolveChildPermissionViaAPI resolves a subagent's parent Backend by walking
// the child→parent chain, filling any missing links via GET /session, then
// delivers ev to the first registered ancestor. It runs OFF the SSE-reader
// goroutine (route spawns it) because the HTTP calls must not block the stream.
// Fired only when childToParent has no link for the child, i.e. the child's
// session.created was missed (#969). Bounded by depth + a visited set.
func (s *Server) resolveChildPermissionViaAPI(sid string, ev rawEvent) {
	const maxDepth = 16
	cur := sid
	seen := make(map[string]bool, maxDepth)
	for i := 0; i < maxDepth && cur != "" && !seen[cur]; i++ {
		seen[cur] = true

		s.sessionsMu.RLock()
		be := s.sessions[cur]
		parent, known := s.childToParent[cur]
		s.sessionsMu.RUnlock()

		if be != nil {
			log.Debugf(s.logComponent(), "routing child session %s %s to parent session %s (resolved via GET /session)", sid, ev.Type, be.sessionID)
			select {
			case be.events <- ev:
			default:
				log.Warnf(s.logComponent(), "event channel full for session %s; dropping %s", be.sessionID, ev.Type)
			}
			return
		}
		if !known {
			pid, ok := s.fetchParentID(cur)
			if !ok || pid == "" {
				// Fetch failed, or cur is a root session (no parent) — no
				// registered ancestor exists to answer this permission.
				log.Debugf(s.logComponent(), "no registered ancestor for child session %s; %s dropped", sid, ev.Type)
				return
			}
			s.sessionsMu.Lock()
			if s.childToParent == nil {
				s.childToParent = make(map[string]string)
			}
			s.childToParent[cur] = pid
			s.sessionsMu.Unlock()
			parent = pid
		}
		cur = parent
	}
	log.Debugf(s.logComponent(), "parent chain for child session %s exceeded depth/cycle; %s dropped", sid, ev.Type)
}

// fetchParentID reads a session's parentID via GET /session/{id}. Returns
// (parentID, true) on success — parentID is "" for a root session — and
// ("", false) on any non-200 or transport error.
func (s *Server) fetchParentID(sid string) (string, bool) {
	req, err := http.NewRequest(http.MethodGet, s.baseURL+"/session/"+sid, nil)
	if err != nil {
		return "", false
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var sess Session
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		return "", false
	}
	return sess.ParentID, true
}

// registerSession adds be to the per-Server session registry under
// be.sessionID. Called by Backend.Start after the session has
// been created via POST /session. Safe to call concurrently with route
// — the RWMutex is the synchronisation point.
//
// Side effects:
//   - allocates be.events if nil (buffered eventBufferSize)
//   - launches be.dispatchLoop if not already running (so events start
//     draining immediately — without this, the channel would fill to
//     256 and route would start dropping)
//
// The dispatcher's handler is whatever be.dispatchHandler is at the
// moment of registerSession. Start calls SetDispatchHandler before
// registerSession so the real handler is bound; if left nil,
// defaultDispatchHandler logs at DEBUG.
func (s *Server) registerSession(be *Backend) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if be.events == nil {
		be.events = make(chan rawEvent, eventBufferSize)
	}
	if be.stopDispatcher == nil {
		be.stopDispatcher = be.startDispatcher()
	}
	s.sessions[be.sessionID] = be
}

// unregisterSession removes the Backend registered under sessionID, if
// any. Called by Backend.Close before the Backend tears down.
// Safe to call when no session was registered (idempotent). Stops the
// dispatcher goroutine and waits for it to exit so any in-flight
// handler invocation completes before the caller frees the Backend.
func (s *Server) unregisterSession(sessionID string) {
	if sessionID == "" {
		return
	}
	s.sessionsMu.Lock()
	be := s.sessions[sessionID]
	delete(s.sessions, sessionID)
	s.sessionsMu.Unlock()
	if be == nil {
		return
	}
	if be.stopDispatcher != nil {
		be.stopDispatcher()
		be.dispatchWg.Wait()
		be.stopDispatcher = nil
	}
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
