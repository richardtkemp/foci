package opencode

import (
	"context"
	"io"
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
	// events arrive, so route doesn't hit the "channel full" drop path
	// during steady-state operation. We push more events than the
	// channel buffer (256) and assert all are received by the handler.
	srv := &Server{sessions: map[string]*Backend{}}
	be := &Backend{sessionID: "sess-drain"}

	var seen atomic.Int32
	be.SetDispatchHandler(func(rawEvent) {
		seen.Add(1)
	})
	srv.registerSession(be)
	defer srv.unregisterSession(be.sessionID)

	// Push 300 events — more than the 256 buffer.
	for i := 0; i < 300; i++ {
		srv.route(rawEvent{
			Type:       "test",
			Properties: []byte(`{"sessionID":"sess-drain"}`),
		})
	}

	// Wait for the dispatcher to process them all.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && seen.Load() < 300 {
		time.Sleep(time.Millisecond)
	}
	if got := seen.Load(); got != 300 {
		t.Errorf("dispatcher processed %d/300 events — channel would fill without a drain goroutine", got)
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
	// waits for it to exit before returning. Without this contract, a
	// Backend teardown would race against in-flight handler calls.
	srv := &Server{sessions: map[string]*Backend{}}
	be := &Backend{sessionID: "sess-stop"}

	var stopped atomic.Bool
	be.SetDispatchHandler(func(rawEvent) {
		// Slow handler so we can prove unregister waits for it.
		time.Sleep(50 * time.Millisecond)
	})
	srv.registerSession(be)

	// Push an event so the dispatcher has work in flight when we
	// unregister.
	srv.route(rawEvent{Type: "slow", Properties: []byte(`{"sessionID":"sess-stop"}`)})
	time.Sleep(10 * time.Millisecond) // let the handler start

	// unregisterSession must wait for the handler to finish.
	start := time.Now()
	srv.unregisterSession(be.sessionID)
	elapsed := time.Since(start)
	if elapsed < 40*time.Millisecond {
		t.Errorf("unregister returned in %s; should have waited for the slow handler", elapsed)
	}
	stopped.Store(true)

	// Pushing another event must not panic or block — the Backend is
	// no longer registered so route drops silently.
	srv.route(rawEvent{Type: "after-stop", Properties: []byte(`{"sessionID":"sess-stop"}`)})

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
