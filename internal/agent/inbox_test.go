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
	"foci/internal/session"
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

// recordBatch captures a batch — wired into Agent.SetTurnObserver by
// startedAgent so existing tests that asserted on d.Calls() / d.NumCalls()
// keep their semantics after TODO #746 Stage C moved batch ownership into
// the agent.
func (d *recordingDriver) recordBatch(_ string, batch []Envelope) {
	d.mu.Lock()
	d.calls = append(d.calls, batch)
	d.mu.Unlock()
}

// WrapTurn implements agent.Driver. recordingDriver doesn't actually
// execute turns (NewTurnSink returns nil so RunTurn no-ops); it just
// signals lifecycle for tests that gate on hookCh/doneCh and applies
// the configured delay so concurrent-turn tests still work.
//
// Fires OnPrimaryWrittenFromContext(ctx) just before fn() runs — the
// recordingDriver substitutes for a real transport whose RunInference
// would have done the primary write, and the inbox-side turnActive
// lifecycle depends on that signal under the post-TODO #777 semantics.
func (d *recordingDriver) WrapTurn(ctx context.Context, fn func() error) error {
	if d.hookCh != nil {
		d.hookCh <- struct{}{}
	}
	if d.delay > 0 {
		time.Sleep(d.delay)
	}
	OnPrimaryWrittenFromContext(ctx)()
	err := fn()
	if d.doneCh != nil {
		d.doneCh <- struct{}{}
	}
	return err
}

// NewTurnSink returns nil — tests that use recordingDriver don't run the
// turn pipeline, only assert on Drive batching/dispatch.
func (d *recordingDriver) NewTurnSink(_ Envelope) (turnevent.Sink, func()) { return nil, nil }

// Connection returns nil — these tests don't exercise platform-side ops.
func (d *recordingDriver) Connection() platform.Connection { return nil }

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
	a.SetTurnObserver(d.recordBatch)
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

// errTurnNotInFlightBackend always declines an Inject with
// ErrTurnNotInFlight, modelling the race where the turn completed between the
// inbox's turnActive check and the inject landing.
type errTurnNotInFlightBackend struct{ mockBackendDT }

func (*errTurnNotInFlightBackend) Inject(context.Context, delegator.Inject) error {
	return delegator.ErrTurnNotInFlight
}

// TestInbox_Enqueue_Steer_RacesTurnCompletion_ReRoutes verifies the steer-race
// fix: when the backend declines a steer with ErrTurnNotInFlight (the turn
// finished underneath it), the inbox re-routes the envelope to the normal idle
// channel so the worker drives a properly-tracked turn instead of dropping it.
func TestInbox_Enqueue_Steer_RacesTurnCompletion_ReRoutes(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.SetInboxSteerMode(true)
	a.SetInboxBackend(func(_ context.Context, _ string) (delegator.Delegator, error) {
		return &errTurnNotInFlightBackend{}, nil
	})

	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)

	// Steer path is taken (turnActive true), but Inject declines → re-route.
	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true)

	a.Enqueue(Envelope{
		SessionKey: "test/s",
		Text:       "raced steer",
		Driver:     d,
	})

	if !waitFor(time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("re-routed envelope did not drive a turn within 1s; calls=%d", d.NumCalls())
	}
	calls := d.Calls()
	if len(calls[0]) != 1 || calls[0][0].Text != "raced steer" {
		t.Errorf("unexpected batch after re-route: %+v", calls[0])
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

// TestInbox_Enqueue_Compacting_DoesNotSteer verifies that while a compaction
// is in flight, a steer-eligible mid-turn message is NOT injected into CC's
// stdin (which would fold it into the compaction transcript unframed). It must
// route to the channel instead, to be dispatched as a clean turn afterwards
// (#856).
func TestInbox_Enqueue_Compacting_DoesNotSteer(t *testing.T) {
	a := newTestAgent(t)
	a.SetInboxSteerMode(true)

	be := &recordingBackend{}
	a.SetInboxBackend(func(_ context.Context, _ string) (delegator.Delegator, error) {
		return be, nil
	})

	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true) // would normally make the message steer-eligible
	a.markCompacting("test/s")

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "deploy"})

	if injects := be.Injects(); len(injects) != 0 {
		t.Errorf("expected 0 injects during compaction, got %d (%+v)", len(injects), injects)
	}
	if got := inb.drainAvailable(); len(got) != 1 || got[0].Text != "deploy" {
		t.Errorf("expected the message on the channel, got %+v", got)
	}
}

// TestInbox_Enqueue_AfterCompaction_ResumesSteer verifies the compaction gate
// is scoped to the compaction window, not a blanket steer disable: once
// compaction clears, a steer-eligible message injects normally (#856).
func TestInbox_Enqueue_AfterCompaction_ResumesSteer(t *testing.T) {
	a := newTestAgent(t)
	a.SetInboxSteerMode(true)

	be := &recordingBackend{}
	a.SetInboxBackend(func(_ context.Context, _ string) (delegator.Delegator, error) {
		return be, nil
	})

	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true)
	a.markCompacting("test/s")
	a.clearCompacting("test/s")

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "after"})

	injects := be.Injects()
	if len(injects) != 1 {
		t.Fatalf("expected 1 inject after compaction cleared, got %d", len(injects))
	}
	if injects[0].Source != delegator.SourceSteer || injects[0].Text != "after" {
		t.Errorf("inject = %+v, want SourceSteer %q", injects[0], "after")
	}
}

// TestInbox_Worker_HoldsDuringCompaction verifies the worker-side backstop:
// a channel-queued message is held (no turn dispatched) while compaction is in
// flight, then dispatched once it clears. Covers the manual-/compact case
// where the worker is free rather than blocked on the compacting turn (#856).
func TestInbox_Worker_HoldsDuringCompaction(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)

	a.markCompacting("test/s")
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "held", Driver: d})

	// While compacting, the worker must hold the message — no dispatch.
	if waitFor(300*time.Millisecond, func() bool { return d.NumCalls() > 0 }) {
		t.Fatalf("worker dispatched during compaction; calls=%d", d.NumCalls())
	}

	// Once compaction clears, the held message dispatches within a few polls.
	a.clearCompacting("test/s")
	if !waitFor(time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("worker did not dispatch after compaction cleared; calls=%d", d.NumCalls())
	}
}

// TestInbox_Enqueue_EmptySessionKey_Drops verifies that envelopes with
// no session key are dropped (logged) rather than crashing.
func TestInbox_Enqueue_EmptySessionKey_Drops(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)
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
	a.SetTurnObserver(d.recordBatch)

	// Push three messages quickly. The first triggers worker entry; while
	// it's holding the Drive call, messages 2 and 3 land on the channel
	// and will be batched into the next Drive iteration.
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "first", Driver: d})
	<-hook // first Drive started, channel now empty
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "second", Driver: d})
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "third", Driver: d})
	<-done // first Drive complete

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

// TestInbox_Worker_TurnActiveLifecycle verifies the post-TODO #777
// turnActive semantics: false on dequeue, flips to true only after the
// transport writes the primary to the backend (via the OnPrimaryWritten
// callback the worker installs on ctx), and back to false after the
// drive returns.
//
// The recordingDriver fires OnPrimaryWrittenFromContext after hookCh and
// before fn(), simulating a real transport's RunInference Inject path.
func TestInbox_Worker_TurnActiveLifecycle(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	hook := make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	d := &recordingDriver{hookCh: hook, doneCh: done, delay: 30 * time.Millisecond}
	a.SetTurnObserver(d.recordBatch)

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "x", Driver: d})

	<-hook // inside WrapTurn, before OnPrimaryWritten fires
	if a.InboxTurnActive("test/s") {
		t.Error("turnActive should be false before primary-written signal")
	}
	<-done // WrapTurn returned — primary-written fired between hook and done

	// After WrapTurn, turnActive is still true until the worker clears it
	// at the end of driveAndDrainOrphans. The transition true→false happens
	// asynchronously; waitFor catches it within the budget.
	if !waitFor(time.Second, func() bool { return !a.InboxTurnActive("test/s") }) {
		t.Error("turnActive should be cleared after driveAndDrainOrphans returns")
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
	a.SetTurnObserver(d.recordBatch)

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

// TestInbox_Worker_OrphanDrainIsRecursive verifies that orphan steers
// arriving DURING orphan-drain processing are themselves drained on the
// next iteration, not left stale until the next turn. This was the bug
// the original telegram-side TestAgentWorker_OrphanDrainIsRecursive
// caught — covering it at the agent level since the orphan loop now
// lives in agent.driveAndDrainOrphans.
func TestInbox_Worker_OrphanDrainIsRecursive(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	// Pre-seed orphan-1 before the primary turn.
	inb := a.getOrCreateInbox("test/s")
	inb.appendSteer("orphan-1", time.Now())

	// Recording driver that, on seeing "orphan-1" arrive as a follow-up,
	// pushes orphan-2 into the steer buffer mid-Drive — simulating a
	// message that arrives during orphan processing.
	d := &recursiveDriver{onText: "orphan-1", appendNext: "orphan-2", inb: inb}
	a.SetTurnObserver(d.observeBatch)

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "primary", Driver: d})

	// Expect 3 Drive calls: primary, orphan-1 (which appends orphan-2),
	// orphan-2 (recursive drain).
	if !waitFor(2*time.Second, func() bool { return d.NumCalls() == 3 }) {
		t.Fatalf("expected 3 Drive calls, got %d", d.NumCalls())
	}

	calls := d.Calls()
	want := []string{"primary", "orphan-1", "orphan-2"}
	for i, w := range want {
		if len(calls[i]) != 1 || calls[i][0].Text != w {
			t.Errorf("call[%d] = %+v, want %q", i, calls[i], w)
		}
	}
}

// recursiveDriver wraps recordingDriver and appends a follow-up steer
// when a configured trigger text appears in the batch. Used to test the
// orphan-drain loop's recursive behaviour.
type recursiveDriver struct {
	recordingDriver
	onText     string
	appendNext string
	inb        *sessionInbox
	once       sync.Once
}

// observeBatch is the recursiveDriver's TurnObserver — captures the
// batch via the agent's SetTurnObserver hook (TODO #746 Stage C
// replacement for the old Drive-with-batch parameter).
func (d *recursiveDriver) observeBatch(sk string, batch []Envelope) {
	if len(batch) > 0 && batch[0].Text == d.onText {
		d.once.Do(func() {
			d.inb.appendSteer(d.appendNext, time.Now())
		})
	}
	d.recordingDriver.recordBatch(sk, batch)
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
	a.SetTurnObserver(d.recordBatch)
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
	a.SetTurnObserver(d.recordBatch)

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
	a.SetTurnObserver(d.recordBatch)
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
	a.SetTurnObserver(d.recordBatch)
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
	a.SetTurnObserver(d.recordBatch)
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
	a.SetTurnObserver(d.recordBatch)
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

func (d *driverGated) WrapTurn(ctx context.Context, fn func() error) error {
	d.count.Add(1)
	select {
	case d.ready <- struct{}{}:
	default:
	}
	if d.release != nil {
		<-d.release
	}
	OnPrimaryWrittenFromContext(ctx)()
	err := fn()
	if d.done != nil {
		d.done <- struct{}{}
	}
	return err
}

func (d *driverGated) NewTurnSink(_ Envelope) (turnevent.Sink, func()) { return nil, nil }
func (d *driverGated) Connection() platform.Connection                 { return nil }

// --- TODO #745 — SessionRouter wiring tests ---

// routerObservingDriver tracks each turn's session key so tests can
// look up the inbox's SessionRouter via the agent. Replaces the old
// Drive-router-parameter capture pattern after TODO #746 Stage C
// removed Drive from the Driver interface.
type routerObservingDriver struct {
	mu  sync.Mutex
	a   *Agent
	sks []string
}

func (d *routerObservingDriver) WrapTurn(ctx context.Context, fn func() error) error {
	OnPrimaryWrittenFromContext(ctx)()
	return fn()
}

// NewTurnSink records the sk this turn is for; tests look up the router
// for that sk via the inbox.
func (d *routerObservingDriver) NewTurnSink(env Envelope) (turnevent.Sink, func()) {
	d.mu.Lock()
	d.sks = append(d.sks, env.SessionKey)
	d.mu.Unlock()
	return nil, nil
}
func (d *routerObservingDriver) Connection() platform.Connection { return nil }

// seenRouters returns the SessionRouter from each driven turn's inbox.
// Same sk → same router (router is session-scoped); test assertions on
// reuse across calls map directly.
func (d *routerObservingDriver) seenRouters() []*sessionRouter {
	if d.a == nil {
		return nil
	}
	d.mu.Lock()
	sks := append([]string(nil), d.sks...)
	d.mu.Unlock()
	out := make([]*sessionRouter, 0, len(sks))
	for _, sk := range sks {
		d.a.inboxesMu.Lock()
		inb := d.a.inboxes[sk]
		d.a.inboxesMu.Unlock()
		if inb == nil {
			out = append(out, nil)
			continue
		}
		out = append(out, inb.router)
	}
	return out
}

// numSeen returns the number of NewTurnSink invocations seen so far.
// Used by waitFor in tests.
func (d *routerObservingDriver) numSeen() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sks)
}

// TestInbox_SessionRouter_LazyConstructedAndReused verifies driveAndDrainOrphans
// builds the SessionRouter once per session (via the Driver's
// NewLateDeliverySink) and reuses the same instance across follow-up Drive
// calls within the same session. Closes the lifecycle gap that TODO #745
// addresses: the router must outlive any single Drive() invocation so late
// deliveries from ccstream's rearm path land somewhere live.
func TestInbox_SessionRouter_LazyConstructedAndReused(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	d := &routerObservingDriver{a: a}
	a.Enqueue(Envelope{SessionKey: "test/sess-A", Text: "first", Driver: d})
	if !waitFor(time.Second, func() bool { return d.numSeen() >= 1 }) {
		t.Fatalf("first turn did not run; numSeen=%d", d.numSeen())
	}
	a.Enqueue(Envelope{SessionKey: "test/sess-A", Text: "second", Driver: d})
	if !waitFor(time.Second, func() bool { return d.numSeen() >= 2 }) {
		t.Fatalf("second turn did not run")
	}

	rs := d.seenRouters()
	if rs[0] == nil {
		t.Fatal("inbox.router is nil; sessionRouterFor must construct one")
	}
	if rs[0] != rs[1] {
		t.Errorf("router reused across calls: got %p then %p, want same instance", rs[0], rs[1])
	}
}

// TestInbox_SessionRouter_DistinctPerSession verifies different session keys
// get different SessionRouter instances. Each session's late-delivery
// fallback is bound to its own session ID (turn.SessionSink's sessionKey).
func TestInbox_SessionRouter_DistinctPerSession(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	d := &routerObservingDriver{a: a}
	a.Enqueue(Envelope{SessionKey: "test/sess-A", Text: "a", Driver: d})
	a.Enqueue(Envelope{SessionKey: "test/sess-B", Text: "b", Driver: d})
	if !waitFor(time.Second, func() bool { return d.numSeen() >= 2 }) {
		t.Fatalf("two sessions did not produce two turns")
	}

	rs := d.seenRouters()
	if rs[0] == rs[1] {
		t.Errorf("two different sessions shared a router; want distinct instances")
	}
}

// (Late-delivery dispatch is covered by SessionRouter unit tests in
// router_test.go and the lateDeliverySink construction is exercised
// via the integration layer when a real Connection is wired.)

// TestInbox_Worker_SinkDeliveryGate_BlocksNonDelivering asserts the
// sink-delivery gate in sessionWorker: when a non-delivering turn is in
// flight on the session base, an enqueued envelope waits rather than
// dispatching. Once the non-delivering turn ends, the worker proceeds.
//
// Repro of TODO #767: reflection/keepalive/compaction-memory turns dispatch
// via handleDelegatedBranch with no sink on ctx, so their in-flight entry
// is non-delivering. Without this gate, a Telegram message arriving during
// that turn folds into the inject path and goes to NopSink.
func TestInbox_Worker_SinkDeliveryGate_BlocksNonDelivering(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	hook := make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	d := &recordingDriver{hookCh: hook, doneCh: done}
	a.SetTurnObserver(d.recordBatch)

	sk := "test/s"
	base := session.SessionKeyBase(sk)

	// Simulate a non-delivering turn in-flight (reflection-style).
	releaseInFlight := a.markInFlight(base, false)

	// Enqueue. The worker should NOT dispatch — it must wait on the gate.
	a.Enqueue(Envelope{SessionKey: sk, Text: "hello", Driver: d})

	select {
	case <-hook:
		releaseInFlight()
		t.Fatalf("worker dispatched while non-delivering turn was in flight; expected to wait")
	case <-time.After(150 * time.Millisecond):
		// expected — worker is parked in InFlightWaitCh.
	}

	// Release the in-flight; the worker should now wake and dispatch.
	releaseInFlight()
	select {
	case <-hook:
		// expected — worker proceeded.
	case <-time.After(time.Second):
		t.Fatalf("worker did not dispatch after non-delivering in-flight released")
	}
	<-done

	calls := d.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 Drive call after gate opened, got %d", len(calls))
	}
	if calls[0][0].Text != "hello" {
		t.Errorf("dispatched batch unexpected: %+v", calls[0])
	}
}

// TestInbox_Worker_SinkDeliveryGate_AllowsDelivering asserts the complement:
// when the in-flight turn IS delivering (its sink reaches the user's
// platform), the worker dispatches immediately. The existing in-flight
// follow-up path is the legitimate behaviour for same-session mid-turn
// addenda (Dick sending "in London" after "what's the weather"), and the
// gate must not break it.
func TestInbox_Worker_SinkDeliveryGate_AllowsDelivering(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	hook := make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	d := &recordingDriver{hookCh: hook, doneCh: done}
	a.SetTurnObserver(d.recordBatch)

	sk := "test/s"
	base := session.SessionKeyBase(sk)

	// Simulate a delivering turn in-flight (a normal user-facing turn).
	releaseInFlight := a.markInFlight(base, true)
	defer releaseInFlight()

	a.Enqueue(Envelope{SessionKey: sk, Text: "addendum", Driver: d})

	select {
	case <-hook:
		// expected — gate let it through because delivering=true.
	case <-time.After(time.Second):
		t.Fatalf("worker did not dispatch while delivering turn was in flight")
	}
	<-done
}

// TestInbox_Worker_SinkDeliveryGate_BatchesArrivalsWhileWaiting asserts that
// envelopes arriving while the worker is parked on the gate accumulate in
// the channel and batch together when the gate opens — matching the
// existing batches-available semantics for the non-gated case.
func TestInbox_Worker_SinkDeliveryGate_BatchesArrivalsWhileWaiting(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	hook := make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	d := &recordingDriver{hookCh: hook, doneCh: done}
	a.SetTurnObserver(d.recordBatch)

	sk := "test/s"
	base := session.SessionKeyBase(sk)

	releaseInFlight := a.markInFlight(base, false)

	a.Enqueue(Envelope{SessionKey: sk, Text: "first", Driver: d})
	// Give the worker a moment to read the first envelope and park on the
	// gate before further enqueues arrive on the channel.
	time.Sleep(20 * time.Millisecond)
	a.Enqueue(Envelope{SessionKey: sk, Text: "second", Driver: d})
	a.Enqueue(Envelope{SessionKey: sk, Text: "third", Driver: d})

	// Still no Drive — gate is closed.
	select {
	case <-hook:
		releaseInFlight()
		t.Fatalf("worker dispatched while non-delivering turn was in flight")
	case <-time.After(100 * time.Millisecond):
	}

	releaseInFlight()

	select {
	case <-hook:
	case <-time.After(time.Second):
		t.Fatalf("worker did not dispatch after gate opened")
	}
	<-done

	calls := d.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 batched Drive call, got %d", len(calls))
	}
	if len(calls[0]) != 3 {
		t.Fatalf("expected 3 envelopes in batch, got %d: %+v", len(calls[0]), calls[0])
	}
	wantTexts := []string{"first", "second", "third"}
	for i, env := range calls[0] {
		if env.Text != wantTexts[i] {
			t.Errorf("batch[%d].Text = %q, want %q", i, env.Text, wantTexts[i])
		}
	}
}
