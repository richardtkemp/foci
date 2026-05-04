package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/delegator"
	"foci/internal/platform"
)

// --- Test fixtures ---

// recordingDriver records all Drive calls for assertion.
type recordingDriver struct {
	mu     sync.Mutex
	calls  [][]Envelope
	delay  time.Duration // optional: hold each Drive open for this long
	hookCh chan struct{} // optional: signalled at start of each Drive
	doneCh chan struct{} // optional: signalled at end of each Drive
}

func (d *recordingDriver) Drive(ctx context.Context, sk string, batch []Envelope, _ turnevent.Steerer) error {
	if d.hookCh != nil {
		d.hookCh <- struct{}{}
	}
	if d.delay > 0 {
		select {
		case <-time.After(d.delay):
		case <-ctx.Done():
		}
	}
	d.mu.Lock()
	d.calls = append(d.calls, batch)
	d.mu.Unlock()
	if d.doneCh != nil {
		d.doneCh <- struct{}{}
	}
	return nil
}

func (d *recordingDriver) Calls() [][]Envelope {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([][]Envelope, len(d.calls))
	copy(out, d.calls)
	return out
}

func (d *recordingDriver) NumCalls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

// recordingBackend captures Inject calls for routing tests. Implements
// only what Enqueue needs — the full delegator.Delegator surface is
// satisfied by embedding mockBackendDT (defined in turn_delegated_test.go).
type recordingBackend struct {
	mockBackendDT
	mu      sync.Mutex
	injects []delegator.Inject
}

func (r *recordingBackend) Inject(ctx context.Context, inj delegator.Inject) error {
	r.mu.Lock()
	r.injects = append(r.injects, inj)
	r.mu.Unlock()
	return nil
}

func (r *recordingBackend) Injects() []delegator.Inject {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]delegator.Inject, len(r.injects))
	copy(out, r.injects)
	return out
}

// --- Helpers ---

func newTestAgent(t *testing.T) *Agent {
	t.Helper()
	return &Agent{}
}

// startedAgent returns an agent with StartInbox called, suitable for
// worker-driven tests. The returned cancel func must be called to stop
// any spawned worker goroutines.
func startedAgent(t *testing.T) (*Agent, context.CancelFunc) {
	t.Helper()
	a := newTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	a.StartInbox(ctx)
	return a, cancel
}

// waitFor polls cond every ~5ms until it returns true or timeout.
// Returns true on success, false on timeout.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// --- Routing tests ---

// TestInbox_Enqueue_Idle_PushesToChannel verifies that an idle session
// (no turn in flight) routes the envelope to the session's main channel,
// and the worker drives a turn for it.
func TestInbox_Enqueue_Idle_PushesToChannel(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	d := &recordingDriver{}
	a.Enqueue(Envelope{
		SessionKey: "test/s",
		Text:       "hello",
		Driver:     d,
	})

	if !waitFor(time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("driver not called within 1s; calls=%d", d.NumCalls())
	}
	calls := d.Calls()
	if len(calls[0]) != 1 || calls[0][0].Text != "hello" {
		t.Errorf("unexpected batch: %+v", calls[0])
	}
}

// TestInbox_Enqueue_InFlight_CCBackend_InjectsSteer verifies that a
// steer-eligible mid-turn message routes to Backend.Inject(SourceSteer)
// when a CC backend is registered, bypassing the buffer entirely.
func TestInbox_Enqueue_InFlight_CCBackend_InjectsSteer(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.SetInboxSteerMode(true)

	be := &recordingBackend{}
	a.SetInboxBackend(func(_ context.Context, _ string) (delegator.Delegator, error) {
		return be, nil
	})

	// Simulate in-flight turn for sk="test/s".
	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true)

	a.Enqueue(Envelope{
		SessionKey: "test/s",
		Text:       "mid-turn ping",
	})

	injects := be.Injects()
	if len(injects) != 1 {
		t.Fatalf("expected 1 inject, got %d", len(injects))
	}
	if injects[0].Source != delegator.SourceSteer {
		t.Errorf("Source = %v, want SourceSteer", injects[0].Source)
	}
	if injects[0].Text != "mid-turn ping" {
		t.Errorf("Text = %q, want %q", injects[0].Text, "mid-turn ping")
	}
	// Buffer must NOT have been used — Inject takes precedence.
	if entries := inb.drainSteer(); len(entries) != 0 {
		t.Errorf("steer buffer should be empty, got %d entries", len(entries))
	}
}

// TestInbox_Enqueue_InFlight_APIBackend_AppendsSteer verifies that a
// steer-eligible mid-turn message goes to AppendSteer when no backend
// is registered (API-mode agents).
func TestInbox_Enqueue_InFlight_APIBackend_AppendsSteer(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.SetInboxSteerMode(true)

	// No SetInboxBackend → resolveSessionBackend returns nil, nil.
	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true)

	now := time.Now()
	a.Enqueue(Envelope{
		SessionKey: "test/s",
		Text:       "buffered",
		ReceivedAt: now,
	})

	entries := inb.drainSteer()
	if len(entries) != 1 {
		t.Fatalf("expected 1 buffered entry, got %d", len(entries))
	}
	if entries[0].Text != "buffered" {
		t.Errorf("Text = %q, want %q", entries[0].Text, "buffered")
	}
	if !entries[0].ReceivedAt.Equal(now) {
		t.Errorf("ReceivedAt mismatch: %v vs %v", entries[0].ReceivedAt, now)
	}
}

// TestInbox_Enqueue_InFlight_WithAttachments_PushesToChannel verifies
// that mid-turn messages with attachments are NOT steer-eligible and
// fall through to the channel for the next turn (existing behaviour —
// the SDK's mid-turn paste primitive doesn't carry attachments).
func TestInbox_Enqueue_InFlight_WithAttachments_PushesToChannel(t *testing.T) {
	a := newTestAgent(t)
	a.SetInboxSteerMode(true)
	// Don't start the worker — we just check the channel directly.
	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true)

	a.Enqueue(Envelope{
		SessionKey:  "test/s",
		Text:        "with file",
		Attachments: []platform.Attachment{{MimeType: "image/png", Data: []byte("xx")}},
	})

	// Channel should have one envelope; buffer should be empty.
	if entries := inb.drainSteer(); len(entries) != 0 {
		t.Errorf("steer buffer should be empty, got %d entries", len(entries))
	}
	got := inb.drainAvailable()
	if len(got) != 1 || got[0].Text != "with file" {
		t.Errorf("expected 1 channel envelope %q, got %+v", "with file", got)
	}
}

// TestInbox_Enqueue_SteerMode_Disabled_PushesToChannel verifies that
// when steer_mode is off, mid-turn messages go to the next-turn channel
// instead of the steer path.
func TestInbox_Enqueue_SteerMode_Disabled_PushesToChannel(t *testing.T) {
	a := newTestAgent(t)
	// SetInboxSteerMode default = false.
	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true)

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "no steer"})

	if entries := inb.drainSteer(); len(entries) != 0 {
		t.Errorf("steer buffer should be empty, got %d", len(entries))
	}
	if got := inb.drainAvailable(); len(got) != 1 {
		t.Errorf("expected 1 channel envelope, got %d", len(got))
	}
}

// TestInbox_Enqueue_EmptySessionKey_Drops verifies that envelopes with
// no session key are dropped (logged) rather than crashing.
func TestInbox_Enqueue_EmptySessionKey_Drops(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	d := &recordingDriver{}
	a.Enqueue(Envelope{Text: "no sk", Driver: d})

	// Give the worker a moment in case it picks up; nothing should fire.
	time.Sleep(50 * time.Millisecond)
	if d.NumCalls() != 0 {
		t.Errorf("expected 0 driver calls, got %d", d.NumCalls())
	}
	// No inbox should have been created for empty key.
	if a.lookupInbox("") != nil {
		t.Errorf("inbox map should not contain empty-key entry")
	}
}

// --- Worker behaviour tests ---

// TestInbox_Worker_BatchesAvailableMessages verifies that messages
// arriving in quick succession are batched into a single Drive call.
func TestInbox_Worker_BatchesAvailableMessages(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	hook := make(chan struct{}, 4)
	done := make(chan struct{}, 4)
	d := &recordingDriver{hookCh: hook, doneCh: done, delay: 50 * time.Millisecond}

	// Push three messages quickly. The first triggers worker entry; while
	// it's holding the Drive call, messages 2 and 3 land on the channel
	// and will be batched into the next Drive iteration.
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "first", Driver: d})
	<-hook                                   // first Drive started, channel now empty
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "second", Driver: d})
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "third", Driver: d})
	<-done                                   // first Drive complete

	// Worker re-enters; second + third should batch.
	<-hook
	<-done

	calls := d.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 Drive calls, got %d", len(calls))
	}
	if len(calls[0]) != 1 || calls[0][0].Text != "first" {
		t.Errorf("first batch unexpected: %+v", calls[0])
	}
	if len(calls[1]) != 2 || calls[1][0].Text != "second" || calls[1][1].Text != "third" {
		t.Errorf("second batch unexpected: %+v", calls[1])
	}
}

// TestInbox_Worker_SetsTurnActiveAroundDrive verifies that the worker
// flips inb.turnActive true during Drive and back to false after.
func TestInbox_Worker_SetsTurnActiveAroundDrive(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	hook := make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	d := &recordingDriver{hookCh: hook, doneCh: done, delay: 30 * time.Millisecond}

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "x", Driver: d})

	<-hook // inside Drive
	if !a.InboxTurnActive("test/s") {
		t.Error("turnActive should be true during Drive")
	}
	<-done // Drive returned

	if !waitFor(time.Second, func() bool { return !a.InboxTurnActive("test/s") }) {
		t.Error("turnActive should be false after Drive")
	}
}

// TestInbox_Worker_DrainsOrphansAfterTurn verifies the orphan-steer
// drain loop: text appended to the buffer during a turn (e.g. via
// AppendSteer from a separate Enqueue race) is processed as a follow-up
// turn after the primary turn ends.
func TestInbox_Worker_DrainsOrphansAfterTurn(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	hook := make(chan struct{}, 4)
	done := make(chan struct{}, 4)
	d := &recordingDriver{hookCh: hook, doneCh: done}

	// Pre-seed the inbox so we can push an orphan steer before the
	// worker creates it.
	inb := a.getOrCreateInbox("test/s")
	inb.appendSteer("buffered-1", time.Now())

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "primary", Driver: d})

	// Wait for two Drive calls: the primary, plus the follow-up that
	// drains the orphan steer.
	if !waitFor(2*time.Second, func() bool { return d.NumCalls() == 2 }) {
		t.Fatalf("expected 2 Drive calls, got %d", d.NumCalls())
	}

	calls := d.Calls()
	if calls[0][0].Text != "primary" {
		t.Errorf("first Drive should be primary, got %q", calls[0][0].Text)
	}
	if len(calls[1]) != 1 || calls[1][0].Text != "buffered-1" {
		t.Errorf("second Drive should be orphan steer, got %+v", calls[1])
	}
}

// TestInbox_Worker_NoDriverDropsBatch verifies that an envelope without
// a Driver doesn't crash the worker — the batch is dropped with a log.
func TestInbox_Worker_NoDriverDropsBatch(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "no driver"})
	// Give the worker a moment to wake up and discard.
	time.Sleep(100 * time.Millisecond)
	// Worker should still be alive — push another with a driver.
	d := &recordingDriver{}
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "with driver", Driver: d})
	if !waitFor(time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("worker should still process subsequent envelopes")
	}
}

// --- Per-session parallelism tests ---

// TestInbox_TwoSessionsRunInParallel verifies that two sessions execute
// their turns concurrently — Session A's slow Drive does not block
// Session B's worker from running its own Drive.
func TestInbox_TwoSessionsRunInParallel(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	// Per-session drivers. A holds open until released; B should run
	// immediately while A is still inside Drive.
	releaseA := make(chan struct{})
	bDone := make(chan struct{})

	dA := &driverGated{ready: make(chan struct{}, 1), release: releaseA}
	dB := &driverGated{ready: make(chan struct{}, 1), done: bDone}

	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "slow", Driver: dA})
	<-dA.ready // Session A is inside Drive (blocked on releaseA)

	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "fast", Driver: dB})

	select {
	case <-bDone:
		// success — B completed while A is still in Drive
	case <-time.After(2 * time.Second):
		t.Fatal("Session B did not run while Session A was blocked")
	}

	close(releaseA) // let A finish
}

// TestInbox_PerSessionTurnActive verifies that each session's turnActive
// flag is independent: Session A in-flight does not make Session B route
// as in-flight.
func TestInbox_PerSessionTurnActive(t *testing.T) {
	a := newTestAgent(t)
	a.SetInboxSteerMode(true)

	// Manually create both inboxes.
	inbA := a.getOrCreateInbox("sess/A")
	inbB := a.getOrCreateInbox("sess/B")

	inbA.turnActive.Store(true)

	// Enqueue to B with steer-eligible payload — should go to B's
	// channel (not steer buffer), because B has no turn in flight.
	a.Enqueue(Envelope{SessionKey: "sess/B", Text: "to B"})

	if entries := inbB.drainSteer(); len(entries) != 0 {
		t.Errorf("Session B steer buffer should be empty, got %d", len(entries))
	}
	got := inbB.drainAvailable()
	if len(got) != 1 {
		t.Errorf("Session B channel should have 1 envelope, got %d", len(got))
	}

	// Conversely, Enqueue to A with steer-eligible should buffer
	// (no backend → API-mode fallback).
	a.Enqueue(Envelope{SessionKey: "sess/A", Text: "to A"})
	if entries := inbA.drainSteer(); len(entries) != 1 {
		t.Errorf("Session A steer buffer should have 1 entry, got %d", len(entries))
	}
}

// TestInbox_LazyWorkerSpawn verifies that each session's worker is
// spawned exactly once, even across many concurrent Enqueue calls.
func TestInbox_LazyWorkerSpawn(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	const N = 50
	d := &recordingDriver{}

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.Enqueue(Envelope{SessionKey: "sess/X", Text: "x", Driver: d})
		}()
	}
	wg.Wait()

	// Wait for all to drain.
	if !waitFor(2*time.Second, func() bool {
		// Sum of envelopes across all batches must equal N.
		total := 0
		for _, b := range d.Calls() {
			total += len(b)
		}
		return total == N
	}) {
		total := 0
		for _, b := range d.Calls() {
			total += len(b)
		}
		t.Fatalf("expected %d total envelopes, got %d", N, total)
	}

	// Verify only one worker exists for sess/X (workerStarted fired once).
	inb := a.lookupInbox("sess/X")
	if inb == nil {
		t.Fatal("inbox missing for sess/X")
	}
	// Manually inspect: workerStarted should be a fired sync.Once.
	// We can't inspect Do count directly; instead, call again and
	// verify no panic + still one inbox in registry.
	a.inboxesMu.Lock()
	count := len(a.inboxes)
	a.inboxesMu.Unlock()
	if count != 1 {
		t.Errorf("expected 1 inbox in registry, got %d", count)
	}
}

// TestInbox_StartInbox_Idempotent verifies that calling StartInbox
// multiple times doesn't double-spawn workers or panic.
func TestInbox_StartInbox_Idempotent(t *testing.T) {
	a := newTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a.StartInbox(ctx)
	a.StartInbox(ctx) // should be no-op
	a.StartInbox(ctx) // still no-op

	d := &recordingDriver{}
	a.Enqueue(Envelope{SessionKey: "sess/Y", Text: "x", Driver: d})
	if !waitFor(time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("worker did not fire after multiple StartInbox calls")
	}
}

// TestInbox_StartInbox_BeforeEnqueue_SpawnsWorker verifies the normal
// ordering: StartInbox first, then Enqueue → worker runs.
func TestInbox_StartInbox_BeforeEnqueue_SpawnsWorker(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	d := &recordingDriver{}
	a.Enqueue(Envelope{SessionKey: "sess/Z", Text: "x", Driver: d})
	if !waitFor(time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatal("worker did not fire after Enqueue")
	}
}

// TestInbox_EnqueueBeforeStart_Buffers verifies the defensive case
// where Enqueue runs before StartInbox: envelopes are buffered in the
// channel but no worker spawns until StartInbox is called.
func TestInbox_EnqueueBeforeStart_Buffers(t *testing.T) {
	a := newTestAgent(t)

	d := &recordingDriver{}
	a.Enqueue(Envelope{SessionKey: "sess/W", Text: "early", Driver: d})

	// No worker yet — Drive should not have fired.
	time.Sleep(50 * time.Millisecond)
	if d.NumCalls() != 0 {
		t.Errorf("expected 0 Drive calls before StartInbox, got %d", d.NumCalls())
	}

	// Now start — worker spawns + drains the buffered envelope.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartInbox(ctx)

	if !waitFor(time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("worker did not drain pre-StartInbox envelope; calls=%d", d.NumCalls())
	}
}

// TestInbox_ContextCancellation_StopsWorker verifies that cancelling
// the parent context exits the session worker goroutine.
func TestInbox_ContextCancellation_StopsWorker(t *testing.T) {
	a := newTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	a.StartInbox(ctx)

	d := &recordingDriver{}
	a.Enqueue(Envelope{SessionKey: "sess/V", Text: "x", Driver: d})
	if !waitFor(time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("first call did not fire")
	}

	cancel()

	// Push another after cancel — should not be processed (worker exited).
	a.Enqueue(Envelope{SessionKey: "sess/V", Text: "after cancel", Driver: d})
	time.Sleep(150 * time.Millisecond)
	if n := d.NumCalls(); n != 1 {
		t.Errorf("expected 1 Drive call after cancel, got %d", n)
	}
}

// driverGated is a Driver that signals on `ready` when entered, then
// blocks on `release` (if non-nil) before returning. Used to verify
// per-session parallelism.
type driverGated struct {
	ready   chan struct{}
	release chan struct{}
	done    chan struct{}
	count   atomic.Int32
}

func (d *driverGated) Drive(ctx context.Context, _ string, _ []Envelope, _ turnevent.Steerer) error {
	d.count.Add(1)
	select {
	case d.ready <- struct{}{}:
	default:
	}
	if d.release != nil {
		select {
		case <-d.release:
		case <-ctx.Done():
			return nil
		}
	}
	if d.done != nil {
		d.done <- struct{}{}
	}
	return nil
}
