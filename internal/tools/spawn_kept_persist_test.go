package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/platform"
	"foci/internal/turnevent"
)

// Messages a spawn sent its spawner with send_to_spawner, kept for the head
// of its result, survive a foci restart (#2677, Dick 2026-10-30 20:21: "this
// stuff needs to be saved and restored").

// noteAgent runs each turn by sending notes[i] with send_to_spawner. A turn
// with a release channel then waits on it and fails (a run a shutdown cuts);
// otherwise it completes with "done <i>".
type noteAgent struct {
	tool    func() *Tool
	notes   []string
	release []chan struct{} // per turn; nil = complete at once
	sent    chan string     // each send_to_spawner tool result
	mu      sync.Mutex
	n       int
}

func (a *noteAgent) HandleMessage(ctx context.Context, sk string, _ []string, _ []platform.Attachment) error {
	a.mu.Lock()
	i := a.n
	a.n++
	a.mu.Unlock()
	params, _ := json.Marshal(map[string]string{"message": a.notes[i]})
	res, err := a.tool().Execute(WithSessionKey(ctx, sk), params)
	if err != nil {
		a.sent <- "error: " + err.Error()
	} else {
		a.sent <- res.Text
	}
	if i < len(a.release) && a.release[i] != nil {
		select {
		case <-a.release[i]:
		case <-ctx.Done():
		}
		return errors.New("backend closed")
	}
	turnevent.Emit(ctx, turnevent.TurnComplete{FinalText: "done " + string(rune('0'+i))})
	return nil
}

func (a *noteAgent) nextSent(t *testing.T) string {
	t.Helper()
	select {
	case s := <-a.sent:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("the spawn never called send_to_spawner")
		return ""
	}
}

// holdingRoute is a SpawnRoute whose Hold the test flips.
type holdingRoute struct {
	mu   sync.Mutex
	hold bool
}

func (r *holdingRoute) route(p string) SpawnRoute {
	r.mu.Lock()
	defer r.mu.Unlock()
	return SpawnRoute{Owner: p, Hold: r.hold}
}

func (r *holdingRoute) set(hold bool) {
	r.mu.Lock()
	r.hold = hold
	r.mu.Unlock()
}

// A background spawn whose spawner is held (a depth fan-in) keeps its
// send_to_spawner message; foci restarts mid-run; the resumed run keeps
// another; the delivered result is headed by BOTH, in order, exactly as
// without the restart.
func TestKeptSpawnerMessagesSurviveRestart(t *testing.T) {
	t.Parallel()
	// Process 1.
	rt1 := &holdingRoute{hold: true}
	var h1 *persistHarness
	a1 := &noteAgent{
		tool:    func() *Tool { return h1.spawner.SendToSpawnerTool() },
		notes:   []string{"first note"},
		release: []chan struct{}{make(chan struct{})},
		sent:    make(chan string, 4),
	}
	h1 = newPersistHarness(a1, func(d *SpawnDeps) { d.Route = rt1.route })
	id := h1.spawn(t, "long task")
	if got := a1.nextSent(t); !strings.HasPrefix(got, "Kept:") {
		t.Fatalf("first send_to_spawner said %q, want it kept", got)
	}
	h1.tracker.BeginShutdown()
	close(a1.release[0])
	h1.noDelivery(t)
	if rec, ok := h1.store.record(t, id); !ok || !rec.Running || len(rec.Kept) != 1 || rec.Kept[0].Text != "first note" {
		t.Fatalf("record after the shutdown cut = %+v (ok %v), want still running with the kept note", rec, ok)
	}

	// Process 2: a fresh tracker on the same store.
	rt2 := &holdingRoute{hold: true}
	var h2 *persistHarness
	a2 := &noteAgent{
		tool:  func() *Tool { return h2.spawner.SendToSpawnerTool() },
		notes: []string{"second note"},
		sent:  make(chan string, 4),
	}
	h2 = newPersistHarness(a2, func(d *SpawnDeps) {
		d.Route = rt2.route
		d.CanResume = func(string) bool { return true }
	})
	h2.store = h1.store
	h2.tracker.SetStore(h1.store)

	h2.spawner.RestoreSpawns()
	if got := a2.nextSent(t); !strings.HasPrefix(got, "Kept:") {
		t.Fatalf("resumed send_to_spawner said %q, want it kept", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(h2.tracker.heldResults("test")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the resumed spawn's result was never held")
		}
		time.Sleep(5 * time.Millisecond)
	}
	rt2.set(false)
	h2.spawner.ReleaseHeld()

	d := h2.nextDelivery(t)
	i1, i2 := strings.Index(d.msg, "first note"), strings.Index(d.msg, "second note")
	res := strings.Index(d.msg, "[ITS RESULT]\n\n[SPAWN RESULT]")
	if !strings.Contains(d.msg, "\n[MESSAGES FROM YOUR SPAWN] Spawn "+id+" sent you 2 message(s)") ||
		i1 < 0 || i2 < 0 || res < 0 || !(i1 < i2 && i2 < res) || !strings.Contains(d.msg, "completed") {
		t.Fatalf("delivered %q, want both kept messages, in order, heading the completed result", d.msg)
	}
	d.done(true)
	if _, ok := h2.store.record(t, id); ok {
		t.Fatal("record kept after the parent took the result")
	}
}

// A spawn a restart cannot resume is reported lost; the messages it kept head
// that report, as they would have headed its result.
func TestKeptSpawnerMessagesHeadLostReport(t *testing.T) {
	t.Parallel()
	h := newPersistHarness(newScriptedSpawnAgent(0), func(d *SpawnDeps) {
		d.CanResume = func(string) bool { return false }
	})
	rec := runningRecord("spawn-kk")
	rec.Kept = []keptRecord{{At: time.Now().Add(-time.Minute), Text: "half way"}, {At: time.Now(), Text: "nearly there"}}
	h.store.put(t, rec)

	h.spawner.RestoreSpawns()
	d := h.nextDelivery(t)
	i1, i2, lost := strings.Index(d.msg, "half way"), strings.Index(d.msg, "nearly there"), strings.Index(d.msg, "lost in a foci restart")
	if !strings.Contains(d.msg, "[MESSAGES FROM YOUR SPAWN] Spawn spawn-kk sent you 2 message(s)") ||
		i1 < 0 || i2 < 0 || lost < 0 || !(i1 < i2 && i2 < lost) {
		t.Fatalf("parent got %q, want both kept messages, in order, heading the lost report", d.msg)
	}
	d.done(true)
	if _, ok := h.store.record(t, rec.ID); ok {
		t.Fatal("record kept after the parent took the lost report")
	}
}
