package turn

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// mockSink is a configurable mock StreamSink. It records Update/Close calls and
// reports a configurable surfaced flag and msgID sequence. It is mutex-guarded
// since Update is driven by the pump goroutine while the test reads its state.
// cond broadcasts on every Update so tests can wait for a target update count
// instead of sleeping a fixed duration (see waitForUpdateCount, #1503).
type mockSink struct {
	mu          sync.Mutex
	cond        *sync.Cond
	updates     []string
	closeCount  int
	surfacedRet bool
	msgIDsRet   []string
}

// newMockSink returns a ready-to-use mockSink with its condition variable wired
// to its mutex. Always use this instead of &mockSink{} when the test needs
// waitForUpdateCount.
func newMockSink() *mockSink {
	m := &mockSink{}
	m.cond = sync.NewCond(&m.mu)
	return m
}

func (m *mockSink) Update(fullText string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updates = append(m.updates, fullText)
	if m.cond != nil {
		m.cond.Broadcast()
	}
}

func (m *mockSink) Close() (surfaced bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeCount++
	return m.surfacedRet
}

func (m *mockSink) MsgIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.msgIDsRet
}

func (m *mockSink) updateCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.updates)
}

// waitForUpdateCount blocks until at least n updates have been recorded, or
// timeout elapses, returning whether the count was reached. It wakes on the
// actual Update signal (via cond) rather than sleeping a fixed duration and
// asserting after the fact, so the test completes as fast as the pump
// actually ticks and only fails when the pump genuinely never fires within
// the (generous) timeout — see #1503.
func (m *mockSink) waitForUpdateCount(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	m.mu.Lock()
	defer m.mu.Unlock()

	for len(m.updates) < n {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		timer := time.AfterFunc(remaining, func() {
			m.mu.Lock()
			m.cond.Broadcast()
			m.mu.Unlock()
		})
		m.cond.Wait()
		timer.Stop()
	}
	return true
}

func (m *mockSink) lastUpdate() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.updates) == 0 {
		return ""
	}
	return m.updates[len(m.updates)-1]
}

func TestStreamBuffer_NonLive_BufferOnly(t *testing.T) {
	// When live=false, deltas are buffered but the sink is never driven.
	sink := &mockSink{}
	sb := NewStreamBuffer(sink, 50*time.Millisecond, false)

	sb.OnDelta("hello ")
	sb.OnDelta("world")

	gotSink, surfaced := sb.Finish()
	if surfaced {
		t.Errorf("surfaced = true, want false (non-live)")
	}
	if gotSink != sink {
		t.Errorf("Finish returned wrong sink")
	}
	if got := sb.Content(); got != "hello world" {
		t.Errorf("content = %q, want %q", got, "hello world")
	}
	if sink.updateCount() != 0 {
		t.Errorf("Update calls = %d, want 0 (non-live never updates)", sink.updateCount())
	}
}

func TestStreamBuffer_Live_FirstDeltaImmediateUpdate(t *testing.T) {
	// The first non-silencing delta in live mode fires an immediate Update so
	// the first message appears promptly (before the ticker would fire).
	sink := &mockSink{}
	sb := NewStreamBuffer(sink, time.Hour, true) // huge interval: only the immediate update can fire

	sb.OnDelta("hello")

	// Immediate update should have happened synchronously in OnDelta.
	if sink.updateCount() != 1 {
		t.Fatalf("Update calls = %d, want 1 (immediate first update)", sink.updateCount())
	}
	if sink.lastUpdate() != "hello" {
		t.Errorf("first update = %q, want %q", sink.lastUpdate(), "hello")
	}
	sb.Finish()
}

func TestStreamBuffer_Live_SubsequentDeltasPump(t *testing.T) {
	// After the first release, subsequent deltas are pushed by the pump.
	sink := newMockSink()
	sb := NewStreamBuffer(sink, 10*time.Millisecond, true)

	sb.OnDelta("first")
	sb.OnDelta(" second")

	// Wait for the immediate update plus at least one pump tick, rather than
	// sleeping fixed durations and asserting after the fact — a wall-clock
	// sleep is not a property the test can rely on under load (#1503). This
	// wakes as soon as the pump actually delivers, and only times out if it
	// genuinely never fires within the generous window.
	if !sink.waitForUpdateCount(2, 5*time.Second) {
		t.Fatalf("Update calls = %d, want >= 2 (immediate + pump) within timeout", sink.updateCount())
	}
	sb.Finish()

	if got := sink.lastUpdate(); !strings.Contains(got, "first second") {
		t.Errorf("last update = %q, want it to contain %q", got, "first second")
	}
}

func TestStreamBuffer_SilencingPrefixHeld(t *testing.T) {
	// While the buffer could still resolve to a silencing sentinel, the pump is
	// held and no Update fires. Once the text diverges, the pump releases.
	sink := &mockSink{}
	sb := NewStreamBuffer(sink, time.Hour, true)

	// "[[NO_RESPONSE" is a prefix of the silencing sentinel — held.
	sb.OnDelta("[[NO_RESPONSE")
	if sink.updateCount() != 0 {
		t.Fatalf("Update calls = %d while in silencing prefix, want 0", sink.updateCount())
	}

	// Diverge: now it can't be a silencing sentinel — release + immediate update.
	sb.OnDelta(" but actually real text")
	if sink.updateCount() != 1 {
		t.Fatalf("Update calls = %d after divergence, want 1 (released)", sink.updateCount())
	}
	sb.Finish()
}

func TestStreamBuffer_SilencingPrefix_NeverDiverges_NoUpdate(t *testing.T) {
	// A stream that resolves entirely to a silencing sentinel never surfaces.
	sink := &mockSink{}
	sb := NewStreamBuffer(sink, time.Hour, true)

	sb.OnDelta("[[NO_RESPONSE]]")
	_, surfaced := sb.Finish()

	if sink.updateCount() != 0 {
		t.Errorf("Update calls = %d, want 0 (never diverged)", sink.updateCount())
	}
	if surfaced {
		// surfaced reflects sink.Close()'s return; the mock returns false by default.
		t.Errorf("surfaced = true, want false")
	}
}

func TestStreamBuffer_Finish_Idempotent(t *testing.T) {
	// Finish is safe to call multiple times; the sink is closed exactly once and
	// the cached surfaced value is returned on subsequent calls.
	sink := &mockSink{surfacedRet: true}
	sb := NewStreamBuffer(sink, 10*time.Millisecond, true)

	sb.OnDelta("test")
	s1, surf1 := sb.Finish()
	s2, surf2 := sb.Finish()

	if s1 != sink || s2 != sink {
		t.Errorf("Finish returned wrong sink")
	}
	if !surf1 || !surf2 {
		t.Errorf("surfaced = (%v,%v), want (true,true)", surf1, surf2)
	}
	sink.mu.Lock()
	closes := sink.closeCount
	sink.mu.Unlock()
	if closes != 1 {
		t.Errorf("Close calls = %d, want 1 (idempotent)", closes)
	}
}

func TestStreamBuffer_DeltaAfterFinish_Ignored(t *testing.T) {
	// Deltas after Finish are silently ignored and don't alter Content.
	sink := &mockSink{}
	sb := NewStreamBuffer(sink, 50*time.Millisecond, true)

	sb.OnDelta("before")
	sb.Finish()
	sb.OnDelta("after")

	if got := sb.Content(); got != "before" {
		t.Errorf("content = %q, want %q", got, "before")
	}
}

func TestStreamBuffer_ContentAfterFinish(t *testing.T) {
	// Content() must still return the full buffer after Finish (used by the
	// empty-FinalText fallback in the renderer).
	sink := &mockSink{}
	sb := NewStreamBuffer(sink, 50*time.Millisecond, true)

	sb.OnDelta("accumulated text")
	sb.Finish()

	if got := sb.Content(); got != "accumulated text" {
		t.Errorf("content after Finish = %q, want %q", got, "accumulated text")
	}
}
