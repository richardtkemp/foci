package agent

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/turn"
	"foci/internal/turnevent"
)

// deliveryLog records every terminal delivery a renderer makes — the user-
// visible messages of a StreamingSink-backed chat.
type deliveryLog struct {
	mu    sync.Mutex
	texts []string
}

func (l *deliveryLog) add(s string) {
	l.mu.Lock()
	l.texts = append(l.texts, s)
	l.mu.Unlock()
}

func (l *deliveryLog) got() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.texts...)
}

// recordingPlatform is a turn.Platform whose terminal deliveries land in a
// deliveryLog. Streaming never surfaces (the buffer is non-live), so every
// delivery is a fresh message.
type recordingPlatform struct{ log *deliveryLog }

type nopStream struct{}

func (nopStream) Update(string)    {}
func (nopStream) Close() bool      { return false }
func (nopStream) MsgIDs() []string { return nil }

func (p recordingPlatform) OpenStream() turn.StreamSink { return nopStream{} }
func (p recordingPlatform) Deliver(pl turn.Payload, _ turn.StreamSink) (turn.DeliveryResult, error) {
	p.log.add(pl.Text)
	return turn.DeliveryResult{}, nil
}
func (p recordingPlatform) EditInPlace(string, turn.Payload) error { return errors.New("no preview") }
func (p recordingPlatform) SendTyping()                            {}
func (p recordingPlatform) Logger() *log.ComponentLogger {
	return log.NewComponentLogger("fold-test")
}

type nopTracker struct{}

func (nopTracker) LastMsgID() string { return "" }
func (nopTracker) ResetMsgID()       {}
func (nopTracker) CleanupPreview()   {}

// newRecordingStreamingSink builds the real platform sink (StreamingSink over a
// TurnRenderer) the app and Telegram use, delivering into dl.
func newRecordingStreamingSink(dl *deliveryLog) *turn.StreamingSink {
	p := recordingPlatform{log: dl}
	d := turn.TurnDisplay{ShowThinking: "off", ShowToolCalls: "off"}
	newSB := func() *turn.StreamBuffer { return turn.NewStreamBuffer(p.OpenStream(), time.Hour, false) }
	return turn.NewStreamingSink(turn.NewTurnRenderer(p, nopTracker{}, d, newSB), nil, nil)
}

// streamingDriverConn is the chat's connection as ResolveLateConn returns it:
// a Driver, so an adopted autonomous run gets the platform's streaming sink
// (as the app's appConn does), and a plain Connection for the late-delivery
// fallback's SessionSink.
type streamingDriverConn struct {
	*leakConn
	dl *deliveryLog
}

func (c *streamingDriverConn) WrapTurn(_ context.Context, fn func() error) error { return fn() }
func (c *streamingDriverConn) NewTurnSink(Envelope) (turnevent.Sink, func()) {
	return newRecordingStreamingSink(c.dl), nil
}
func (c *streamingDriverConn) Connection() platform.Connection { return c }

// foldTestPlatform is the connection's platform name, registered as an
// interactive trigger for the test (a name of its own, so tests that register
// and delete "telegram" cannot race this one).
const foldTestPlatform = "fold-test-plat"

func (c *streamingDriverConn) PlatformName() string { return foldTestPlatform }

// openOnFirstCheckBackend models a CC process that resumes with a turn already
// open (#2366): the first in-flight probe — the one RunInference makes after
// acquiring the backend, AFTER RunTurn's own in-flight check found nothing —
// is when the backend reports the open run, and foci adopts it.
type openOnFirstCheckBackend struct {
	*adoptingBackend
	opened atomic.Bool
	onOpen func()
}

// IsTurnInFlight opens the run on the first probe. Not a sync.Once: adoption
// itself probes the backend, and a re-entrant Once.Do deadlocks.
func (b *openOnFirstCheckBackend) IsTurnInFlight() bool {
	if b.opened.CompareAndSwap(false, true) {
		b.setTurnInFlight(true)
		b.onOpen()
	}
	return b.adoptingBackend.IsTurnInFlight()
}

// platformSinkDriver is a sinkDriver on a real chat connection, so the turn is
// interactive user input (its trigger is the platform's name) and may fold.
type platformSinkDriver struct {
	sinkDriver
	conn platform.Connection
}

func (d *platformSinkDriver) Connection() platform.Connection { return d.conn }

// countContaining returns how many of texts contain sub.
func countContaining(texts []string, sub string) int {
	n := 0
	for _, s := range texts {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

// TestFoldIntoOpenTurn_ReplyDeliveredOnce reproduces #2366: a user message
// arrives while CC has a turn open that foci adopts only once the backend is
// acquired (a restart resumed CC mid-run), so RunTurn registers the message's
// own sink and RunInference then folds the message into the open run. The run
// had already streamed part of its reply. The folded turn delivers nothing of
// its own, but its HandleMessage still ended with an empty TurnComplete — and
// that reached the adopted run's sink through the router, finalizing the run
// mid-stream: the streamed fragment went out as a message, and the full reply
// went out again when the run delivered it. Each reply must reach the chat
// exactly once, and the run's sink must see only the run's own TurnComplete.
func TestFoldIntoOpenTurn_ReplyDeliveredOnce(t *testing.T) {
	initTestConvo(t, "helen")
	RegisterPlatformTrigger(foldTestPlatform)
	t.Cleanup(func() { platformTriggers.Delete(foldTestPlatform) })
	const sk = "helen/c42"
	const reply = "You were right: the gate was not running."

	dl := &deliveryLog{}
	conn := &streamingDriverConn{leakConn: &leakConn{}, dl: dl}

	inner := &adoptingBackend{}
	inner.sessionFile = "/tmp/fold-open-turn.jsonl"
	be := &openOnFirstCheckBackend{adoptingBackend: inner}

	a := newBatchTestAgent(t, be)
	a.ResolveLateConn = func(string) platform.Connection { return conn }
	be.onOpen = func() {
		// CC's resumed run is adopted with the chat's streaming sink, and has
		// streamed the start of its reply by the time the message folds in.
		a.OpenAutonomousTurn(sk, be, nil)
		inner.events().OnTextDelta("You were right: ")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartInbox(ctx)

	userLog := &deliveryLog{}
	done := make(chan struct{}, 1)
	d := &platformSinkDriver{
		sinkDriver: sinkDriver{recordingDriver: recordingDriver{doneCh: done}, sink: newRecordingStreamingSink(userLog)},
		conn:       conn,
	}
	a.Enqueue(Envelope{SessionKey: sk, Text: "was #2361 running?", Driver: d})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		buf := make([]byte, 1<<20)
		t.Fatalf("the user turn never returned\n%s", buf[:runtime.Stack(buf, true)])
	}

	if injects := inner.InjectsRecorded(); len(injects) != 1 {
		t.Fatalf("injects = %+v, want the message folded into the open run once", injects)
	}

	// The run finishes its reply and ends.
	inner.events().OnTextDelta("the gate was not running.")
	inner.events().OnText(reply)
	inner.completeAdopted(reply)
	waitNotInFlight(t, a, sk)

	// Everything that reached the chat: the run's sink, the message's own sink,
	// and the late-delivery fallback (SendToSession on the connection).
	all := append(append(dl.got(), userLog.got()...), conn.got()...)
	if n := countContaining(all, "You were right"); n != 1 {
		t.Fatalf("the reply reached the chat %d times, want exactly once: %q", n, all)
	}
	if got := dl.got(); len(got) != 1 || got[0] != reply {
		t.Fatalf("the run's sink delivered %q, want exactly the full reply %q", got, reply)
	}
}

// lifecycleSink counts the turn-boundary events a sink receives.
type lifecycleSink struct {
	mu                sync.Mutex
	starts, completes int
}

func (s *lifecycleSink) Emit(_ context.Context, ev turnevent.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev.(type) {
	case turnevent.TurnStart:
		s.starts++
	case turnevent.TurnComplete:
		s.completes++
	}
}
func (s *lifecycleSink) DeliversToPlatform() bool { return true }

func (s *lifecycleSink) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts, s.completes
}

// TestFoldWithOwnSink_StillCompletesOwnSink is the other side of #2366's
// fix: the folded turn's TurnComplete is withheld only from the session
// router. A caller that wires its OWN sink — an ask answer's SessionSink or
// appSink (deliverToSessionChatThen), whose TurnStart turned typing on — still
// gets the TurnComplete that pairs it, and the adopted run's sink gets none.
func TestFoldWithOwnSink_StillCompletesOwnSink(t *testing.T) {
	initTestConvo(t, "helen")
	const sk = "helen/c42"
	be := &adoptingBackend{}
	be.sessionFile = "/tmp/fold-own-sink.jsonl"
	be.setTurnInFlight(true)

	a := newBatchTestAgent(t, be)
	runSink := &lifecycleSink{}
	a.ResolveLateConn = func(string) platform.Connection {
		return &lifecycleDriverConn{leakConn: &leakConn{}, sink: runSink}
	}
	a.OpenAutonomousTurn(sk, be, nil)
	if _, c := runSink.counts(); c != 0 {
		t.Fatalf("the adopted run's sink completed before the run ended (%d)", c)
	}

	own := &lifecycleSink{}
	ctx := WithTrigger(WithUserInput(turnevent.WithSink(context.Background(), own)), "ask_grader")
	if err := a.HandleMessage(ctx, sk, []string{"[SYSTEM: the user answered]"}, nil); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if injects := be.InjectsRecorded(); len(injects) != 1 {
		t.Fatalf("injects = %+v, want the answer folded into the run once", injects)
	}
	if s, c := own.counts(); s != 1 || c != 1 {
		t.Fatalf("own sink got %d TurnStart / %d TurnComplete, want 1/1 — its TurnStart is left unpaired", s, c)
	}
	if _, c := runSink.counts(); c != 0 {
		t.Fatalf("the fold completed the adopted run's sink (%d TurnComplete)", c)
	}

	be.completeAdopted("final")
	waitNotInFlight(t, a, sk)
	if _, c := runSink.counts(); c != 1 {
		t.Fatalf("the adopted run's sink got %d TurnComplete, want its own one", c)
	}
}

// lifecycleDriverConn hands the adopted run a given sink.
type lifecycleDriverConn struct {
	*leakConn
	sink turnevent.Sink
}

func (c *lifecycleDriverConn) WrapTurn(_ context.Context, fn func() error) error { return fn() }
func (c *lifecycleDriverConn) NewTurnSink(Envelope) (turnevent.Sink, func()) {
	return c.sink, nil
}
func (c *lifecycleDriverConn) Connection() platform.Connection { return c }
