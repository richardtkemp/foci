package tools

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"foci/internal/turnevent"
)

// TestSpawnTextSink_ReportsRetryWait proves a spawn's rate-limit wait reaches
// the observer with the time the next try is due, and that the spawn's next
// output clears it (#3146). Before, the sink dropped BackendRetry and the app
// showed the waiting spawn as "thinking".
func TestSpawnTextSink_ReportsRetryWait(t *testing.T) {
	t.Parallel()
	const parent, id = "cam/c1", "spawn-1"
	obs := &recordingSpawnObserver{}
	sink := &spawnTextSink{BufferSink: turnevent.NewBufferSink(), obs: obs, tracker: NewSpawnTracker(), parent: parent, id: id}
	ctx := context.Background()

	const delay = 95 * time.Minute
	before := time.Now()
	sink.Emit(ctx, turnevent.BackendRetry{Endpoint: "Claude Code", Attempt: 1, Delay: delay, Status: 429, Category: "rate_limit"})
	after := time.Now()

	obs.mu.Lock()
	got := slices.Clone(obs.retries)
	obs.mu.Unlock()
	if len(got) != 1 || got[0].id != id || got[0].status != 429 {
		t.Fatalf("observer retries = %+v, want one for %s with status 429", got, id)
	}
	lo, hi := before.Add(delay).Round(time.Second).UnixMilli(), after.Add(delay).Round(time.Second).UnixMilli()
	if got[0].untilMs < lo || got[0].untilMs > hi || got[0].untilMs%1000 != 0 {
		t.Errorf("untilMs = %d, want in [%d, %d] (now + the retry delay, to the second)", got[0].untilMs, lo, hi)
	}

	// Output clears the wait once; more output sends nothing more.
	sink.Emit(ctx, turnevent.TextBlock{Text: "back"})
	sink.Emit(ctx, turnevent.ToolCall{ID: "t1", Name: "Bash"})
	obs.mu.Lock()
	got = slices.Clone(obs.retries)
	obs.mu.Unlock()
	if len(got) != 2 || got[1].untilMs != 0 {
		t.Fatalf("observer retries after output = %+v, want a second, clearing one", got)
	}
}

// TestSpawnTextSink_RetryClearedByEachClearingEvent proves every event that
// means the backend is no longer waiting clears the wait (#3146).
func TestSpawnTextSink_RetryClearedByEachClearingEvent(t *testing.T) {
	t.Parallel()
	for _, ev := range []turnevent.Event{
		turnevent.TextBlock{Text: "x"},
		turnevent.TextDelta{Delta: "x"},
		turnevent.ThinkingDelta{Delta: "x"},
		turnevent.ToolCall{ID: "t1", Name: "Bash"},
		turnevent.TurnComplete{},
	} {
		t.Run(fmt.Sprintf("%T", ev), func(t *testing.T) {
			obs := &recordingSpawnObserver{}
			sink := &spawnTextSink{BufferSink: turnevent.NewBufferSink(), obs: obs, tracker: NewSpawnTracker(), parent: "p", id: "s"}
			sink.Emit(context.Background(), turnevent.BackendRetry{Delay: time.Minute, Status: 529})
			sink.Emit(context.Background(), ev)
			if n := len(obs.retries); n != 2 || obs.retries[1].untilMs != 0 {
				t.Errorf("retries = %+v, want the wait then a clear", obs.retries)
			}
		})
	}
}
