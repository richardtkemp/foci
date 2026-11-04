package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/session"
	"foci/internal/tools"
)

// detachedFixture is a delegated agent with a facet branch that holds a live
// backend. reflected closes when the reflection turn reaches the backend, and
// closed reports whether the backend has been closed.
type detachedFixture struct {
	ag        *Agent
	dm        *DelegatedManager
	facetKey  string
	reflected chan struct{}
	turnKey   atomic.Value // the session key the backend was mapped to during reflection
	closed    atomic.Bool
}

func newDetachedFixture(t *testing.T, sessionEndEnabled bool) *detachedFixture {
	t.Helper()
	store := session.NewStore(t.TempDir())
	parentKey := "test/cdetached"
	seedSession(t, store, parentKey, 1)
	facetKey, err := store.CreateBranchWithOptions(parentKey, session.BranchOptions{BranchType: "facet"})
	if err != nil {
		t.Fatalf("create facet branch: %v", err)
	}

	f := &detachedFixture{facetKey: facetKey, reflected: make(chan struct{})}
	f.dm = &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			be := &mockBackendDM{running: true}
			be.closeFn = func() error {
				f.closed.Store(true)
				return nil
			}
			return be, nil
		},
		StartOpts:   delegator.StartOptions{WorkDir: t.TempDir()},
		AgentID:     "test",
		IdleTimeout: time.Hour,
	}
	t.Cleanup(func() { f.dm.Close() })

	if _, err := f.dm.Get(context.Background(), facetKey); err != nil {
		t.Fatalf("create facet backend: %v", err)
	}
	mb, _ := f.dm.getManaged(facetKey)
	mock := mb.be.(*mockBackendDM)
	mock.sendToPaneFn = func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
		f.turnKey.Store(mb.sessionKey)
		close(f.reflected)
		result := &delegator.TurnResult{Text: "memory formed"}
		if handler != nil && handler.OnTurnComplete != nil {
			handler.OnTurnComplete(result)
		}
		return result, nil
	}

	f.ag = &Agent{
		Sessions:         store,
		Tools:            tools.NewRegistry(),
		DelegatedManager: f.dm,
		Model:            "claude-haiku-4-5",
		Reflection:       config.ResolvedReflection{SessionEndEnabled: sessionEndEnabled},
	}
	return f
}

// TestEndDetachedSession_FormsMemoryThenClosesBackend is #2763: a facet released
// by /done must get the same session-end reflection as a reclaimed one, on a
// child of the facet that adopted its live backend, and the backend must be
// closed after that turn, not before it.
func TestEndDetachedSession_FormsMemoryThenClosesBackend(t *testing.T) {
	t.Parallel()
	f := newDetachedFixture(t, true)

	f.ag.EndDetachedSession(f.facetKey)

	select {
	case <-f.reflected:
	case <-time.After(30 * time.Second):
		t.Fatal("reflection turn never reached the facet's backend")
	}
	got, _ := f.turnKey.Load().(string)
	meta, err := f.ag.Sessions.GetBranchMeta(got)
	if err != nil || meta == nil {
		t.Fatalf("GetBranchMeta(%q): meta=%v err=%v", got, meta, err)
	}
	if meta.ParentKey != f.facetKey {
		t.Errorf("reflection ran on a branch of %q, want a branch of the facet %q", meta.ParentKey, f.facetKey)
	}

	deadline := time.Now().Add(30 * time.Second)
	for !f.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !f.closed.Load() {
		t.Error("backend was not closed after the reflection turn (CC process leak)")
	}
	if _, mapped := f.dm.getManaged(f.facetKey); mapped {
		t.Error("backend still mapped under the facet key")
	}
}

// TestEndDetachedSession_DisabledStillClosesBackend pins the existing /done
// contract: when no reflection runs, the backend still closes, so a CC process
// never outlives the released facet.
func TestEndDetachedSession_DisabledStillClosesBackend(t *testing.T) {
	t.Parallel()
	f := newDetachedFixture(t, false)

	f.ag.endDetachedSession(context.Background(), f.facetKey, time.Minute)

	select {
	case <-f.reflected:
		t.Error("reflection ran with session-end memory disabled")
	default:
	}
	if !f.closed.Load() {
		t.Error("backend was not closed")
	}
}

// TestEndDetachedSession_WaitsForCancelledTurn: /done cancels the turn and does
// not wait for it. Remapping the backend under a turn that is still unwinding
// is what ResetSession refuses to do, so this path must wait for the turn end.
func TestEndDetachedSession_WaitsForCancelledTurn(t *testing.T) {
	t.Parallel()
	f := newDetachedFixture(t, true)
	f.ag.SetTurnInFlightForTest(f.facetKey, true)

	// The first thing that runs after the wait. The reflection itself takes too
	// long to reach the backend to fall inside the window below.
	pastWait := make(chan struct{})
	f.ag.ResetOrientTemplateFn = func() string {
		close(pastWait)
		return ""
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.ag.endDetachedSession(context.Background(), f.facetKey, time.Hour)
	}()

	// A slow machine can only make this window pass without testing the wait;
	// it cannot make it fail.
	select {
	case <-pastWait:
		t.Fatal("session end proceeded while the cancelled turn was still in flight")
	case <-done:
		t.Fatal("returned while the cancelled turn was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	if f.closed.Load() {
		t.Fatal("backend closed while the cancelled turn was still in flight")
	}

	f.ag.SetTurnInFlightForTest(f.facetKey, false)
	select {
	case <-f.reflected:
	case <-time.After(30 * time.Second):
		t.Fatal("reflection did not run after the turn ended")
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("did not finish after the turn ended")
	}
}

// TestEndDetachedSession_TurnNeverEndsClosesBackendWithoutMemory: a turn that
// ignores its cancel must not keep a backend alive forever or get a reflection
// remapped under it. It falls back to the old /done behaviour: close the backend.
func TestEndDetachedSession_TurnNeverEndsClosesBackendWithoutMemory(t *testing.T) {
	t.Parallel()
	f := newDetachedFixture(t, true)
	f.ag.SetTurnInFlightForTest(f.facetKey, true)

	f.ag.endDetachedSession(context.Background(), f.facetKey, time.Millisecond)

	select {
	case <-f.reflected:
		t.Error("reflection ran under a turn that never ended")
	default:
	}
	if !f.closed.Load() {
		t.Error("backend was not closed after the wait gave up")
	}
}
