package opencode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// SSE parser (Subscriber.Run) — pure byte-stream → event tests.
// ---------------------------------------------------------------------------

func TestSubscriber_ParsesEventFrame(t *testing.T) {
	// Verifies a single-line data: frame decodes to a rawEvent with the
	// expected Type and Properties. This is the happy path that every
	// other subscriber test depends on.
	input := "data: {\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"sess-x\"}}\n\n"
	var got []rawEvent
	sub := NewSubscriber(strings.NewReader(input), func(ev rawEvent) {
		got = append(got, ev)
	}, nil)
	if err := sub.Run(context.Background()); err != nil && err != io.EOF {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if got[0].Type != "session.idle" {
		t.Errorf("Type = %q, want session.idle", got[0].Type)
	}
	if sid := extractSessionID(got[0].Properties); sid != "sess-x" {
		t.Errorf("sessionID = %q, want sess-x", sid)
	}
}

func TestReadLine_TrimsAndResyncsPastOversized(t *testing.T) {
	// An over-ceiling line must be dropped (errLineTooLong) and the reader
	// resynced so the NEXT line reads cleanly — the property that keeps one
	// giant SSE frame from killing the subscriber (#972). Also checks CRLF
	// trimming. Ceiling is passed directly, so no 16 MiB allocation needed.
	const max = 64
	input := "small line\r\n" + strings.Repeat("x", max+50) + "\nafter\n"
	r := bufio.NewReaderSize(strings.NewReader(input), 16)

	line, err := readLine(r, max)
	if err != nil {
		t.Fatalf("line 1: %v", err)
	}
	if string(line) != "small line" {
		t.Errorf("line 1 = %q, want %q (CR must be trimmed)", line, "small line")
	}

	if _, err := readLine(r, max); !errors.Is(err, errLineTooLong) {
		t.Fatalf("oversized line: err = %v, want errLineTooLong", err)
	}

	line, err = readLine(r, max)
	if err != nil {
		t.Fatalf("line after oversized: %v", err)
	}
	if string(line) != "after" {
		t.Errorf("resync failed: got %q, want %q", line, "after")
	}
}

func TestSubscriber_MultipleDataLines(t *testing.T) {
	// Verifies the WHATWG concatenation rule: multiple `data:` lines in
	// one frame are joined with `\n` before being parsed as JSON. The
	// opencode server doesn't emit multi-line data today, but the parser
	// must honour the spec in case that changes.
	input := "data: {\"type\":\"x\",\"properties\":\ndata: {\"sessionID\":\"sess-y\"}}\n\n"
	var got []rawEvent
	sub := NewSubscriber(strings.NewReader(input), func(ev rawEvent) {
		got = append(got, ev)
	}, nil)
	if err := sub.Run(context.Background()); err != nil && err != io.EOF {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 (data: lines must concatenate)", len(got))
	}
	if got[0].Type != "x" {
		t.Errorf("Type = %q, want x", got[0].Type)
	}
}

func TestSubscriber_HeartbeatUpdatesActivity(t *testing.T) {
	// Verifies comment lines (leading `:`) invoke onHeartbeat — which
	// in production updates s.lastActivity. We assert onHeartbeat fired.
	input := ":heartbeat 1\n\n:heartbeat 2\n\n"
	var beats int
	sub := NewSubscriber(strings.NewReader(input), nil, func() {
		beats++
	})
	if err := sub.Run(context.Background()); err != nil && err != io.EOF {
		t.Fatalf("Run: %v", err)
	}
	if beats != 2 {
		t.Errorf("onHeartbeat fired %d times, want 2", beats)
	}
}

func TestSubscriber_DataLineWithNoSpaceAfterColon(t *testing.T) {
	// Verifies the WHATWG "single optional space" rule: `data:value`
	// (no space) and `data: value` (one space) decode to the same value.
	input := "data:{\"type\":\"nospace\",\"properties\":{}}\n\n"
	var got []rawEvent
	sub := NewSubscriber(strings.NewReader(input), func(ev rawEvent) {
		got = append(got, ev)
	}, nil)
	_ = sub.Run(context.Background())
	if len(got) != 1 || got[0].Type != "nospace" {
		t.Errorf("data:no-space parse failed; got %+v", got)
	}
}

func TestSubscriber_EmptyDataDropped(t *testing.T) {
	// Verifies an empty data: payload (no JSON) is silently dropped
	// rather than firing onEvent with a zero-value rawEvent. opencode
	// occasionally emits keepalive-ish empty frames; the subscriber must
	// not tear down on them.
	input := "data:\n\n"
	var got int
	sub := NewSubscriber(strings.NewReader(input), func(rawEvent) {
		got++
	}, nil)
	_ = sub.Run(context.Background())
	if got != 0 {
		t.Errorf("empty data: fired %d events, want 0", got)
	}
}

func TestSubscriber_MalformedJSONDropped(t *testing.T) {
	// Verifies a data: payload that isn't valid JSON is dropped rather
	// than killing the subscription. Defensive — opencode is well-
	// behaved but a parser that crashes on a bad frame would lose the
	// whole stream.
	input := "data: {not json\n\n"
	var got int
	sub := NewSubscriber(strings.NewReader(input), func(rawEvent) {
		got++
	}, nil)
	_ = sub.Run(context.Background())
	if got != 0 {
		t.Errorf("malformed JSON fired %d events, want 0", got)
	}
}

func TestSubscriber_UnknownFieldIgnored(t *testing.T) {
	// Verifies SSE fields other than "data:" (event, id, retry) are
	// ignored rather than confusing the parser. opencode doesn't emit
	// them today but the spec allows them.
	input := "event: foo\ndata: {\"type\":\"only-data\",\"properties\":{}}\nid: 42\n\n"
	var got []rawEvent
	sub := NewSubscriber(strings.NewReader(input), func(ev rawEvent) {
		got = append(got, ev)
	}, nil)
	_ = sub.Run(context.Background())
	if len(got) != 1 || got[0].Type != "only-data" {
		t.Errorf("unknown-field handling failed; got %+v", got)
	}
}

func TestSubscriber_StopsOnEOF(t *testing.T) {
	// Verifies Run returns io.EOF when the reader closes cleanly. This
	// is the path that fires when the subprocess shuts down.
	sub := NewSubscriber(strings.NewReader(""), nil, nil)
	if err := sub.Run(context.Background()); err != io.EOF {
		t.Errorf("Run returned %v, want io.EOF", err)
	}
}

func TestSubscriber_StopsOnCtxCancel(t *testing.T) {
	// Verifies Run returns ctx.Err() when the context is cancelled mid-
	// stream. Server.Close cancels its subscriber ctx to shut down the
	// subscriber goroutine.
	r, w := io.Pipe()
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	sub := NewSubscriber(r, nil, nil)
	done := make(chan error, 1)
	go func() { done <- sub.Run(ctx) }()

	// Cancel without writing anything — Run should return promptly.
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// ---------------------------------------------------------------------------
// runSubscriber — initial-connect retry through the subprocess startup
// window (the bug fix). The subscriber goroutine is launched before the
// subprocess binds its port, so the first GET /event gets "connection
// refused"; runSubscriber must retry rather than fire OnSubscriberStopped
// and kill the stream permanently.
// ---------------------------------------------------------------------------

// newRetryTestServer builds a minimal Server suitable for driving
// runSubscriber directly: a sessions registry plus the done channel that
// runSubscriber selects on for subprocess-death. It is NOT Start()ed — we
// only exercise runSubscriber's connect loop and routing.
func newRetryTestServer() *Server {
	return &Server{
		sessions: map[string]*Backend{},
		done:     make(chan struct{}),
	}
}

func TestRunSubscriber_RetriesInitialConnectUntilReady(t *testing.T) {
	// The core regression test. Target is initially unreachable (the port
	// is reserved then closed, so connects are refused). runSubscriber must
	// NOT immediately call OnSubscriberStopped/finalizeExit; it must retry,
	// and once a 200 SSE endpoint comes up on that port, connect and route
	// an event to the registered Backend.

	// Reserve a port, then close the listener so the address is free but
	// nothing is listening — connects refuse until we bind below.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	srv := newRetryTestServer()
	srv.baseURL = "http://" + addr

	// Register a Backend so a routed event is observable.
	be := &Backend{sessionID: "sess-retry", events: make(chan rawEvent, 1)}
	srv.sessions["sess-retry"] = be

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	subDone := make(chan struct{})
	go func() {
		srv.runSubscriber(ctx)
		close(subDone)
	}()

	// runSubscriber must still be retrying (not exited) while the target is
	// down. Give it several retry intervals.
	select {
	case <-subDone:
		t.Fatal("runSubscriber returned while target was unreachable — it must retry the initial connect")
	case <-time.After(5 * subscriberConnectRetryInterval):
		// expected — still looping
	}

	// Bring up a 200 SSE endpoint on the SAME port that emits one event.
	mux := http.NewServeMux()
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"sess-retry\"}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// Hold the stream open until the request ctx is cancelled so the
		// subscriber's Run blocks on the (now-established) stream rather
		// than hitting EOF immediately.
		<-r.Context().Done()
	})
	httpSrv := &http.Server{Addr: addr, Handler: mux}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("late listen on %s: %v", addr, err)
	}
	go func() { _ = httpSrv.Serve(ln) }()
	defer func() { _ = httpSrv.Close() }()

	// The event must arrive once the retry loop connects.
	select {
	case ev := <-be.events:
		if ev.Type != "session.idle" {
			t.Errorf("routed event = %q, want session.idle", ev.Type)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no event routed after the SSE endpoint came up — retry loop did not connect")
	}

	// runSubscriber should still be alive on the established stream (it
	// only stops on EOF/ctx/done). Cancel to wind it down.
	select {
	case <-subDone:
		t.Fatal("runSubscriber returned while the established stream was still open")
	default:
	}
	cancel()
	select {
	case <-subDone:
	case <-time.After(3 * time.Second):
		t.Fatal("runSubscriber did not return after ctx cancel")
	}
}

func TestRunSubscriber_StopsOnSubprocessDeath(t *testing.T) {
	// While retrying against a dead target, closing s.done (the waiter
	// goroutine's signal that the subprocess actually exited) must stop the
	// loop promptly — it does not spin forever against a server that will
	// never come up.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	srv := newRetryTestServer()
	srv.baseURL = "http://" + addr

	subDone := make(chan struct{})
	go func() {
		srv.runSubscriber(context.Background())
		close(subDone)
	}()

	// Confirm it's retrying, not already exited.
	select {
	case <-subDone:
		t.Fatal("runSubscriber returned before subprocess death was signalled")
	case <-time.After(3 * subscriberConnectRetryInterval):
	}

	// Simulate subprocess death.
	close(srv.done)
	select {
	case <-subDone:
		// expected — loop observed s.done and returned
	case <-time.After(2 * time.Second):
		t.Fatal("runSubscriber did not stop after s.done closed (spinning against dead server)")
	}
}

func TestRunSubscriber_StopsOnCtxCancelDuringRetry(t *testing.T) {
	// While retrying against a dead target, ctx cancellation (Close cancels
	// the subscriber ctx when the health probe fails) must stop the loop
	// promptly.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	srv := newRetryTestServer()
	srv.baseURL = "http://" + addr

	ctx, cancel := context.WithCancel(context.Background())
	subDone := make(chan struct{})
	go func() {
		srv.runSubscriber(ctx)
		close(subDone)
	}()

	select {
	case <-subDone:
		t.Fatal("runSubscriber returned before ctx cancel")
	case <-time.After(3 * subscriberConnectRetryInterval):
	}

	cancel()
	select {
	case <-subDone:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("runSubscriber did not stop after ctx cancel during retry")
	}
}

func TestRunSubscriber_RetriesOnNon200(t *testing.T) {
	// A reachable server that returns a non-200 (e.g. 503 during boot)
	// must also be retried, not treated as a fatal stop. We start a server
	// that 503s, confirm the loop is still retrying, then flip it to a 200
	// SSE endpoint and confirm it connects and routes.
	var ready atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"sess-503\"}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer ts.Close()

	srv := newRetryTestServer()
	srv.baseURL = ts.URL
	be := &Backend{sessionID: "sess-503", events: make(chan rawEvent, 1)}
	srv.sessions["sess-503"] = be

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subDone := make(chan struct{})
	go func() {
		srv.runSubscriber(ctx)
		close(subDone)
	}()

	select {
	case <-subDone:
		t.Fatal("runSubscriber returned on non-200 — it must retry")
	case <-time.After(5 * subscriberConnectRetryInterval):
	}

	ready.Store(true)
	select {
	case ev := <-be.events:
		if ev.Type != "session.idle" {
			t.Errorf("routed event = %q, want session.idle", ev.Type)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no event routed after the server returned 200 — retry-on-non-200 failed")
	}

	cancel()
	select {
	case <-subDone:
	case <-time.After(3 * time.Second):
		t.Fatal("runSubscriber did not return after ctx cancel")
	}
}

// ---------------------------------------------------------------------------
// Server.route — per-session dispatch logic.
// ---------------------------------------------------------------------------

func TestServer_Route_DispatchesBySessionID(t *testing.T) {
	// Verifies route delivers each event to the Backend registered under
	// the event's sessionID — pulling sessionID from each of the four
	// wire locations (top-level, .part, .info, .permission) since
	// extractSessionID handles all of them.
	srv := &Server{sessions: map[string]*Backend{}}

	be1 := &Backend{sessionID: "sess-1", events: make(chan rawEvent, 1)}
	be2 := &Backend{sessionID: "sess-2", events: make(chan rawEvent, 1)}
	srv.sessions["sess-1"] = be1
	srv.sessions["sess-2"] = be2

	tests := []struct {
		name string
		ev   rawEvent
		want string // sessionID whose channel should receive
	}{
		{
			name: "session.idle (top-level sessionID)",
			ev:   rawEvent{Type: "session.idle", Properties: []byte(`{"sessionID":"sess-1"}`)},
			want: "sess-1",
		},
		{
			name: "message.part.updated (nested .part)",
			ev:   rawEvent{Type: "message.part.updated", Properties: []byte(`{"part":{"sessionID":"sess-2"}}`)},
			want: "sess-2",
		},
		{
			name: "message.updated (nested .info)",
			ev:   rawEvent{Type: "message.updated", Properties: []byte(`{"info":{"sessionID":"sess-1"}}`)},
			want: "sess-1",
		},
		{
			name: "permission.updated (nested .permission)",
			ev:   rawEvent{Type: "permission.updated", Properties: []byte(`{"permission":{"sessionID":"sess-2"}}`)},
			want: "sess-2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv.route(tc.ev)
			select {
			case got := <-be1.events:
				if tc.want != "sess-1" {
					t.Errorf("be1 received %v, want nobody", got.Type)
				}
			default:
			}
			select {
			case got := <-be2.events:
				if tc.want != "sess-2" {
					t.Errorf("be2 received %v, want nobody", got.Type)
				}
			default:
			}
		})
	}
}

func TestServer_Route_ChildPermissionResolvedViaAPI(t *testing.T) {
	// When a subagent's permission arrives but its session.created was missed
	// (no childToParent link), route must fetch the parent via GET /session and
	// deliver the permission to the parent Backend rather than dropping it (#969).
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/child-sess" {
			_, _ = w.Write([]byte(`{"id":"child-sess","parentID":"parent-sess"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer hs.Close()

	srv := &Server{
		sessions:      map[string]*Backend{},
		childToParent: map[string]string{},
		baseURL:       hs.URL,
		http:          hs.Client(),
		agentID:       "perm-fetch-test",
	}
	parent := &Backend{sessionID: "parent-sess", events: make(chan rawEvent, 1)}
	srv.sessions["parent-sess"] = parent

	// Permission for an unregistered child with no known parent link.
	srv.route(rawEvent{Type: EventPermissionAsked, Properties: []byte(`{"permission":{"sessionID":"child-sess"}}`)})

	// route spawned the fetch off-goroutine; wait briefly for delivery.
	select {
	case ev := <-parent.events:
		if ev.Type != EventPermissionAsked {
			t.Errorf("parent received %q, want %q", ev.Type, EventPermissionAsked)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("permission was not routed to the parent Backend via GET /session")
	}

	// The link must now be cached so subsequent events resolve synchronously.
	if got := srv.childToParent["child-sess"]; got != "parent-sess" {
		t.Errorf("childToParent[child-sess] = %q, want parent-sess", got)
	}
}

func TestServer_Route_UnknownSessionIgnored(t *testing.T) {
	// Verifies route silently drops events for unknown sessionIDs rather
	// than blocking or panicking. This is the early-race case (event
	// arrives before Backend registers).
	srv := &Server{sessions: map[string]*Backend{}}
	be := &Backend{sessionID: "known", events: make(chan rawEvent, 1)}
	srv.sessions["known"] = be

	// Route for an unknown session — must not panic or block.
	srv.route(rawEvent{Type: "x", Properties: []byte(`{"sessionID":"unknown"}`)})

	// Known session's channel must be empty (event went elsewhere).
	select {
	case ev := <-be.events:
		t.Errorf("known Backend received event for unknown session: %v", ev)
	default:
	}
}

func TestServer_Route_GlobalEventDropped(t *testing.T) {
	// Verifies route drops events with no sessionID (server.connected,
	// tui.*, etc.) rather than broadcasting them to every Backend.
	srv := &Server{sessions: map[string]*Backend{}}
	be1 := &Backend{sessionID: "sess-1", events: make(chan rawEvent, 1)}
	be2 := &Backend{sessionID: "sess-2", events: make(chan rawEvent, 1)}
	srv.sessions["sess-1"] = be1
	srv.sessions["sess-2"] = be2

	srv.route(rawEvent{Type: "server.connected", Properties: []byte(`{}`)})
	srv.route(rawEvent{Type: "tui.toast.show", Properties: []byte(`{"message":"hi"}`)})

	if len(be1.events) != 0 || len(be2.events) != 0 {
		t.Errorf("global events must not be delivered; be1=%d be2=%d", len(be1.events), len(be2.events))
	}
}

func TestServer_Route_FullChannelDropsAndLogs(t *testing.T) {
	// Verifies route drops events when the Backend's channel is full
	// rather than blocking the SSE reader. Critical: a wedged dispatcher
	// in one session must not stall events for others.
	srv := &Server{sessions: map[string]*Backend{}}
	fullBE := &Backend{sessionID: "full", events: make(chan rawEvent, 1)}
	otherBE := &Backend{sessionID: "other", events: make(chan rawEvent, 1)}
	srv.sessions["full"] = fullBE
	srv.sessions["other"] = otherBE

	// Fill the first Backend's channel.
	fullBE.events <- rawEvent{Type: "first", Properties: []byte(`{"sessionID":"full"}`)}

	// Emit two more events — one for the full Backend, one for the other.
	// The full one must drop; the other must deliver.
	srv.route(rawEvent{Type: "second-full", Properties: []byte(`{"sessionID":"full"}`)})
	srv.route(rawEvent{Type: "other-event", Properties: []byte(`{"sessionID":"other"}`)})

	// Drain fullBE: only the first event should be present.
	got1 := <-fullBE.events
	if got1.Type != "first" {
		t.Errorf("fullBE first event = %q, want first", got1.Type)
	}
	select {
	case ev := <-fullBE.events:
		t.Errorf("fullBE second event must have dropped; got %v", ev.Type)
	default:
		// expected — channel was empty after the first drain
	}

	// otherBE must have its event.
	select {
	case got := <-otherBE.events:
		if got.Type != "other-event" {
			t.Errorf("otherBE event = %q, want other-event", got.Type)
		}
	default:
		t.Error("otherBE channel empty — the drop on fullBE stalled the route")
	}
}

// childCreated builds a session.created event linking child→parent.
func childCreated(childID, parentID string) rawEvent {
	return rawEvent{
		Type:       EventSessionCreated,
		Properties: []byte(`{"info":{"id":"` + childID + `","parentID":"` + parentID + `"}}`),
	}
}

// askedFor builds a permission.asked event for the given (child) session.
func askedFor(sessionID string) rawEvent {
	return rawEvent{
		Type:       EventPermissionAsked,
		Properties: []byte(`{"id":"per_x","sessionID":"` + sessionID + `","permission":"external_directory"}`),
	}
}

func TestServer_Route_ChildPermissionReroutedToParent(t *testing.T) {
	// A subagent (child) session is never registered as a Backend. Its
	// permission.asked must be rerouted to the parent's registered Backend
	// (learned from session.created) — else it's dropped and the subagent,
	// plus the parent turn waiting on it, blocks forever (#964).
	srv := &Server{sessions: map[string]*Backend{}}
	parent := &Backend{sessionID: "ses_parent", events: make(chan rawEvent, 2)}
	srv.sessions["ses_parent"] = parent

	srv.route(childCreated("ses_child", "ses_parent")) // learn the link
	srv.route(askedFor("ses_child"))                   // child's permission

	select {
	case got := <-parent.events:
		if got.Type != EventPermissionAsked {
			t.Errorf("parent got %q, want permission.asked", got.Type)
		}
	default:
		t.Fatal("parent Backend did not receive the child's permission")
	}
}

func TestServer_Route_ChildPermissionMultiLevel(t *testing.T) {
	// A grandchild's permission resolves up two levels to the registered root.
	srv := &Server{sessions: map[string]*Backend{}}
	root := &Backend{sessionID: "ses_root", events: make(chan rawEvent, 2)}
	srv.sessions["ses_root"] = root

	srv.route(childCreated("ses_child", "ses_root"))
	srv.route(childCreated("ses_grand", "ses_child"))
	srv.route(askedFor("ses_grand"))

	select {
	case got := <-root.events:
		if got.Type != EventPermissionAsked {
			t.Errorf("root got %q, want permission.asked", got.Type)
		}
	default:
		t.Fatal("root Backend did not receive the grandchild's permission")
	}
}

func TestServer_Route_ChildPermissionCorrectParent(t *testing.T) {
	// Two registered sessions A and B share the server. A child of A's
	// permission must reach A, never B — correct attribution, no cross-wiring.
	srv := &Server{sessions: map[string]*Backend{}}
	beA := &Backend{sessionID: "ses_A", events: make(chan rawEvent, 2)}
	beB := &Backend{sessionID: "ses_B", events: make(chan rawEvent, 2)}
	srv.sessions["ses_A"] = beA
	srv.sessions["ses_B"] = beB

	srv.route(childCreated("ses_childA", "ses_A"))
	srv.route(askedFor("ses_childA"))

	if len(beB.events) != 0 {
		t.Errorf("B wrongly received A's child permission (cross-wiring)")
	}
	select {
	case got := <-beA.events:
		if got.Type != EventPermissionAsked {
			t.Errorf("A got %q, want permission.asked", got.Type)
		}
	default:
		t.Fatal("A did not receive its child's permission")
	}
}

func TestServer_Route_ChildNonPermissionStillDropped(t *testing.T) {
	// Only permission events are rerouted. A child's text/idle events stay
	// dropped (we don't want subagent output surfacing on the parent session).
	srv := &Server{sessions: map[string]*Backend{}}
	parent := &Backend{sessionID: "ses_parent", events: make(chan rawEvent, 2)}
	srv.sessions["ses_parent"] = parent

	srv.route(childCreated("ses_child", "ses_parent"))
	srv.route(rawEvent{Type: EventSessionIdle, Properties: []byte(`{"sessionID":"ses_child"}`)})
	srv.route(rawEvent{Type: EventMessagePartUpdated, Properties: []byte(`{"part":{"sessionID":"ses_child"}}`)})

	if len(parent.events) != 0 {
		t.Errorf("parent wrongly received a non-permission child event; got %d", len(parent.events))
	}
}

func TestServer_Route_ChildPermissionNoParentDropped(t *testing.T) {
	// A child permission with no known parent link is dropped (not panic, not
	// misrouted) — the pre-fix behaviour for genuinely unknown sessions.
	srv := &Server{sessions: map[string]*Backend{}}
	other := &Backend{sessionID: "ses_other", events: make(chan rawEvent, 2)}
	srv.sessions["ses_other"] = other

	srv.route(askedFor("ses_orphan")) // no session.created recorded

	if len(other.events) != 0 {
		t.Errorf("orphan permission must drop, not land on an unrelated Backend")
	}
}

// ---------------------------------------------------------------------------
// Server.registerSession / unregisterSession
// ---------------------------------------------------------------------------

func TestServer_RegisterUnregisterSession(t *testing.T) {
	// Verifies the registry lifecycle: register inserts the Backend and
	// allocates its event channel; unregister removes it. Idempotent on
	// both sides (unregister unknown ID is a no-op).
	srv := &Server{sessions: map[string]*Backend{}}

	be := &Backend{sessionID: "sess-reg"}
	srv.registerSession(be)
	if be.events == nil {
		t.Fatal("registerSession did not allocate events channel")
	}
	if got, ok := srv.sessions["sess-reg"]; !ok || got != be {
		t.Error("Backend not registered under its sessionID")
	}

	srv.unregisterSession("sess-reg")
	if _, ok := srv.sessions["sess-reg"]; ok {
		t.Error("unregisterSession did not remove the Backend")
	}

	// Idempotent unregister (must not panic).
	srv.unregisterSession("nonexistent")

	// Empty sessionID unregister is a no-op (defensive — Backend.Close
	// might fire before Backend.Start assigned a sessionID).
	srv.unregisterSession("")
}

func TestServer_RegisterSession_ConcurrentWithRoute(t *testing.T) {
	// Verifies register and route can race safely — route's RLock and
	// register's Lock serialise correctly. Also exercises the
	// dispatcher-launch side-effect under concurrency.
	srv := &Server{sessions: map[string]*Backend{}}
	var wg sync.WaitGroup
	const goroutines = 16
	for i := 0; i < goroutines; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			be := &Backend{sessionID: "sess-race"}
			srv.registerSession(be)
		}(i)
		go func(i int) {
			defer wg.Done()
			srv.route(rawEvent{Type: "x", Properties: []byte(`{"sessionID":"sess-race"}`)})
		}(i)
	}
	wg.Wait()
	// Clean up any dispatcher that got started.
	if be, ok := srv.sessions["sess-race"]; ok && be.stopDispatcher != nil {
		be.stopDispatcher()
		be.dispatchWg.Wait()
	}
}

// ---------------------------------------------------------------------------
// Backend dispatcher (drain) — Step 4
// ---------------------------------------------------------------------------

func TestBackend_Dispatcher_DrainsChannel(t *testing.T) {
	// Verifies the dispatcher goroutine drains be.events as fast as
	// events arrive, so the channel doesn't fill to its 256-event
	// buffer and stay there.
	//
	// We push events in batches smaller than the buffer, waiting for
	// each batch to drain before sending the next. Total events
	// exceed the buffer (proving the dispatcher is actually running
	// across multiple batch boundaries) without relying on tight-loop
	// timing between producer and consumer — which is fundamentally
	// racy in Go's cooperative scheduler.
	srv := &Server{sessions: map[string]*Backend{}}
	be := &Backend{sessionID: "sess-drain"}

	var seen atomic.Int32
	be.SetDispatchHandler(func(rawEvent) {
		seen.Add(1)
	})
	srv.registerSession(be)
	defer srv.unregisterSession(be.sessionID)

	// 3 batches of 100, each < buffer size (256). Total 300 > buffer.
	const batchSize = 100
	const batches = 3
	for b := 0; b < batches; b++ {
		start := seen.Load()
		for i := 0; i < batchSize; i++ {
			srv.route(rawEvent{
				Type:       "test",
				Properties: []byte(`{"sessionID":"sess-drain"}`),
			})
		}
		// Wait for this batch to drain before sending the next.
		target := start + batchSize
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && seen.Load() < target {
			time.Sleep(time.Millisecond)
		}
		if got := seen.Load(); got < target {
			t.Fatalf("batch %d: processed %d/%d events — dispatcher not draining", b, got, target)
		}
	}

	total := int32(batchSize * batches)
	if got := seen.Load(); got != total {
		t.Errorf("dispatcher processed %d/%d events", got, total)
	}
}

func TestBackend_Dispatcher_HandsEventsToHandler(t *testing.T) {
	// Verifies the dispatcher invokes the registered handler with each
	// event's content (not just the count). Pins the call shape Step 7
	// will rely on: handler(rawEvent).
	srv := &Server{sessions: map[string]*Backend{}}
	be := &Backend{sessionID: "sess-handler"}

	var (
		mu     sync.Mutex
		got    []string
	)
	be.SetDispatchHandler(func(ev rawEvent) {
		mu.Lock()
		got = append(got, ev.Type)
		mu.Unlock()
	})
	srv.registerSession(be)
	defer srv.unregisterSession(be.sessionID)

	srv.route(rawEvent{Type: "session.idle", Properties: []byte(`{"sessionID":"sess-handler"}`)})
	srv.route(rawEvent{Type: "session.status", Properties: []byte(`{"sessionID":"sess-handler","status":{"type":"busy"}}`)})
	srv.route(rawEvent{Type: "message.updated", Properties: []byte(`{"info":{"sessionID":"sess-handler"}}`)})

	// Poll for completion. Take the lock only to read the count, never
	// hold it across the time.Sleep — the dispatcher goroutine also
	// needs to acquire it.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 3 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("handler saw %d events, want 3: %v", len(got), got)
	}
	want := []string{"session.idle", "session.status", "message.updated"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBackend_Dispatcher_StopOnUnregister(t *testing.T) {
	// Verifies unregisterSession stops the dispatcher goroutine and
	// waits for it to exit before returning — specifically, that it
	// BLOCKS while a handler invocation is in flight and only returns
	// after the handler completes. Without this contract, a Backend
	// teardown would race against in-flight handler calls.
	//
	// Uses explicit channels rather than time-based assertions so the
	// test is deterministic across scheduler timing.
	srv := &Server{sessions: map[string]*Backend{}}
	be := &Backend{sessionID: "sess-stop"}

	handlerStarted := make(chan struct{})
	handlerRelease := make(chan struct{})
	be.SetDispatchHandler(func(rawEvent) {
		close(handlerStarted)
		<-handlerRelease // block until the test releases us
	})
	srv.registerSession(be)

	// Push an event so the dispatcher has work to do.
	srv.route(rawEvent{Type: "slow", Properties: []byte(`{"sessionID":"sess-stop"}`)})

	// Wait for the handler to start.
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("handler did not start within 1s")
	}

	// unregisterSession should block until we release the handler.
	unregisterDone := make(chan struct{})
	go func() {
		srv.unregisterSession(be.sessionID)
		close(unregisterDone)
	}()
	select {
	case <-unregisterDone:
		t.Fatal("unregisterSession returned while handler was still running")
	case <-time.After(50 * time.Millisecond):
		// expected — unregister is blocked behind the in-flight handler
	}

	// Release the handler. unregister should now complete.
	close(handlerRelease)
	select {
	case <-unregisterDone:
	case <-time.After(time.Second):
		t.Fatal("unregisterSession did not return after handler released")
	}

	if be.stopDispatcher != nil {
		t.Error("be.stopDispatcher should be nil after unregister")
	}
}

func TestBackend_Dispatcher_DefaultHandlerIsNoOp(t *testing.T) {
	// Verifies a Backend with no explicit handler (Step 4 production
	// default — Step 7 sets the real one) still drains without panicking.
	// The default logs at DEBUG; we just verify the channel empties.
	srv := &Server{sessions: map[string]*Backend{}}
	be := &Backend{sessionID: "sess-default"}
	// No SetDispatchHandler — defaultDispatchHandler is used.
	srv.registerSession(be)
	defer srv.unregisterSession(be.sessionID)

	for i := 0; i < 10; i++ {
		srv.route(rawEvent{Type: "default", Properties: []byte(`{"sessionID":"sess-default"}`)})
	}

	// Wait briefly for the dispatcher to drain.
	time.Sleep(50 * time.Millisecond)

	// Channel should be empty (drained).
	if len(be.events) != 0 {
		t.Errorf("events channel has %d undrained events with default handler", len(be.events))
	}
}
