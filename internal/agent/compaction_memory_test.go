package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/session"
	"foci/internal/turnevent"
)

// The pre-compaction reflection on a delegated agent (#3088). Dick's ruling,
// 2026-11-13: "Preferred is to fork and reflect in the fork, if forking is
// available. Else must be sequential". These tests drive the real hook
// (FireCompactionMemory) through a real compaction path and record, in one
// ordered log, every turn the backend runs and the /compact it is sent.

const (
	cmMarker  = "CM-REFLECT-MARKER"
	cmSession = "helen/c1"
)

// cmLog is the ordered record of what the backend saw.
type cmLog struct {
	mu     sync.Mutex
	events []string
}

func (l *cmLog) add(e string) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}

func (l *cmLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// forkingBackendDT is a mockBackendDT that can fork its conversation
// (delegator.BackendBrancher) and reports a backend session id, so
// ForkBackendBranch has a parent to clone.
type forkingBackendDT struct {
	*mockBackendDT
	forks int
	fmu   sync.Mutex
}

func (b *forkingBackendDT) SessionID() string { return "parent-session" }

func (b *forkingBackendDT) ForkSession(_ context.Context, req delegator.ForkRequest) (delegator.ForkResult, error) {
	b.fmu.Lock()
	b.forks++
	b.fmu.Unlock()
	return delegator.ForkResult{SessionID: "fork-of-" + req.ParentSessionID}, nil
}

func (b *forkingBackendDT) CleanupSession(_ context.Context, _ delegator.CleanupRequest) error {
	return nil
}

// cmAgent builds a delegated agent whose backend records each turn
// ("turn:<kind>") and each /compact ("compact") in log. Every turn reports a
// context fill far over the compaction threshold, the reflection's turn
// included: the worst case for re-entry. onReflect, when set, runs inside the
// reflection's turn before it completes.
func cmAgent(t *testing.T, canFork bool, log *cmLog, onReflect func()) (*Agent, *mockBackendDT) {
	t.Helper()
	mock := &mockBackendDT{}
	var be delegator.Delegator = mock
	spec := delegator.Spec{Caps: map[delegator.Capability]delegator.Support{}}
	if canFork {
		be = &forkingBackendDT{mockBackendDT: mock}
		spec.Caps[delegator.CapBranch] = delegator.Yes()
	}
	mock.sendToPaneFn = func(_ context.Context, prompt string, h *mockHandler) (*delegator.TurnResult, error) {
		kind := "user"
		if strings.Contains(prompt, cmMarker) {
			kind = "reflection"
			if onReflect != nil {
				onReflect()
			}
		}
		log.add("turn:" + kind)
		if h != nil && h.OnTurnComplete != nil {
			h.OnTurnComplete(&delegator.TurnResult{
				Text:  "ok",
				Model: "test-model",
				Usage: &delegator.TurnUsage{InputTokens: 2, OutputTokens: 10, CacheReadInputTokens: 190000},
			})
		}
		return nil, nil
	}
	mock.sendCommandFn = func(_ context.Context, cmd string) error {
		if strings.HasPrefix(cmd, "/compact") {
			log.add("compact")
		}
		return nil
	}

	promptPath := filepath.Join(t.TempDir(), "reflect.md")
	if err := os.WriteFile(promptPath, []byte(cmMarker+": write down what matters"), 0o644); err != nil {
		t.Fatal(err)
	}

	idx := newTestSessionIndex(t)
	store := session.NewStore(t.TempDir())
	mgr := &DelegatedManager{
		AgentID:      "helen",
		StartOpts:    delegator.StartOptions{AgentID: "helen", WorkDir: t.TempDir(), Model: "test-model"},
		NewBackend:   func() (delegator.Delegator, error) { return be, nil },
		Spec:         spec,
		SessionIndex: idx,
	}
	a := &Agent{
		AgentID:          "helen",
		Model:            "test-model",
		DelegatedManager: mgr,
		Sessions:         store,
		SessionIndex:     idx,
		Compactor:        compaction.NewCompactor(store, 0.5),
		Reflection:       config.ResolvedReflection{CompactionEnabled: true, CompactionPrompt: promptPath},
		injectGatePoll:   testGatePoll,
	}
	mgr.AttachDelivery = a.AttachDelivery
	mgr.RunBatchTurn = a.RunBatchTurn
	// The session has existed for a while: its birth seeds last_reflection,
	// and a turn now is activity since then.
	born := time.Now().Add(-time.Hour)
	idx.Upsert(session.SessionIndexEntry{
		SessionKey: cmSession, CreatedAt: born, LastActivityAt: born,
		SessionType: session.SessionTypeChat, Status: session.SessionStatusActive,
	})
	a.CompactionMemoryFunc.Add(func(ctx context.Context, sk string) {
		a.FireCompactionMemory(ctx, sk, "")
	})
	return a, mock
}

// runWithin runs fn and fails the test if it does not return in time: a
// deadlock in the nested turn shows as this failure, not a hung package.
func runWithin(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second): // hang guard only
		t.Fatalf("%s did not return: deadlock in the pre-compaction reflection?", what)
	}
}

// userTurnThroughInbox runs a user turn the way a platform message does: an
// envelope on the session's inbox, driven by the session's worker. The
// post-turn compaction (and so the pre-compaction reflection) then runs on
// that worker, where waiting on the inbox would deadlock.
func userTurnThroughInbox(t *testing.T, a *Agent, sk string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.StartInbox(ctx)
	done := make(chan struct{}, 1)
	d := &sinkDriver{
		recordingDriver: recordingDriver{doneCh: done},
		sink:            fnSink(func(context.Context, turnevent.Event) {}),
	}
	if !a.Enqueue(Envelope{SessionKey: sk, Text: "hello", Driver: d}) {
		t.Fatal("the inbox refused the user turn")
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second): // hang guard only
		t.Fatal("the user turn did not return: deadlock in the pre-compaction reflection?")
	}
}

func assertEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("backend saw %v, want %v", got, want)
	}
}

func assertStamped(t *testing.T, a *Agent, sk string, notBefore time.Time) {
	t.Helper()
	at, ok := a.SessionIndex.LastReflection(sk)
	if !ok {
		t.Fatalf("session %s has no last_reflection: the pre-compaction pass did not count as its reflection", sk)
	}
	if at.Before(notBefore.Truncate(time.Second)) {
		t.Errorf("last_reflection = %s, before the pass (%s)", at, notBefore)
	}
}

// TestCompactionMemory_Delegated_InPlace_AutoCompaction: a backend that
// cannot fork, auto-compaction after a turn over the threshold. The
// reflection runs as a nested turn in the session itself (no inbox: this is
// the session's own post-turn phase, where an inbox wait would deadlock), the
// /compact goes out only after it ends, and the reflection's own turn — also
// over the threshold — neither compacts nor reflects again.
func TestCompactionMemory_Delegated_InPlace_AutoCompaction(t *testing.T) {
	initTestConvo(t, "helen") // a process-wide log: not parallel
	log := &cmLog{}
	a, _ := cmAgent(t, false, log, nil)
	const sk = cmSession
	if a.BranchStrategyFor("compaction-memory") != RunInPlace {
		t.Fatal("fixture: a backend without CapBranch must reflect in place")
	}

	start := time.Now()
	userTurnThroughInbox(t, a, sk)

	assertEvents(t, log.get(), "turn:user", "turn:reflection", "compact")
	assertStamped(t, a, sk, start)
}

// TestCompactionMemory_Delegated_Fork_AutoCompaction: a backend that can
// fork. The reflection runs in a fork of the pre-compaction conversation, and
// the session is compacted after it.
func TestCompactionMemory_Delegated_Fork_AutoCompaction(t *testing.T) {
	initTestConvo(t, "helen") // a process-wide log: not parallel
	log := &cmLog{}
	a, _ := cmAgent(t, true, log, nil)
	const sk = cmSession
	if a.BranchStrategyFor("compaction-memory") != BranchForkBackend {
		t.Fatal("fixture: a backend with CapBranch must reflect in a fork")
	}
	fb := func() *forkingBackendDT {
		be, _ := a.DelegatedManager.NewBackend()
		return be.(*forkingBackendDT)
	}()

	start := time.Now()
	userTurnThroughInbox(t, a, sk)

	assertEvents(t, log.get(), "turn:user", "turn:reflection", "compact")
	fb.fmu.Lock()
	forks := fb.forks
	fb.fmu.Unlock()
	if forks != 1 {
		t.Errorf("backend forks = %d, want 1: the reflection must run in a fork", forks)
	}
	assertStamped(t, a, sk, start)
}

// TestCompactionMemory_Delegated_InPlace_ManualCompact: /compact (and quiet
// compaction, which runs the same pipeline) comes from outside the session's
// turn. Without a fork the reflection runs in the session through its inbox
// worker, and compaction waits for it: while the reflection's turn is held
// open, no /compact may go out.
func TestCompactionMemory_Delegated_InPlace_ManualCompact(t *testing.T) {
	initTestConvo(t, "helen") // a process-wide log: not parallel
	log := &cmLog{}
	release := make(chan struct{})
	inReflection := make(chan struct{})
	a, _ := cmAgent(t, false, log, func() {
		close(inReflection)
		<-release
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartInbox(ctx)
	const sk = cmSession
	// The session has a backend conversation to compact, and activity since
	// its last reflection.
	if _, err := a.DelegatedManager.Get(ctx, sk); err != nil {
		t.Fatalf("Get: %v", err)
	}
	a.SessionIndex.TouchActivity(sk)

	start := time.Now()
	errc := make(chan error, 1)
	go func() {
		_, err := a.CompactSession(ctx, sk, false)
		errc <- err
	}()

	select {
	case <-inReflection:
	case <-time.After(30 * time.Second): // hang guard only
		t.Fatal("the reflection turn never started")
	}
	// The reflection turn is running. Compaction must still be waiting.
	if got := log.get(); len(got) != 0 {
		t.Fatalf("while the reflection turn runs the backend already saw %v; compaction must wait for it", got)
	}
	close(release)

	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("CompactSession: %v", err)
		}
	case <-time.After(30 * time.Second): // hang guard only
		t.Fatal("CompactSession did not return after the reflection ended")
	}
	assertEvents(t, log.get(), "turn:reflection", "compact")
	assertStamped(t, a, sk, start)
}

// TestCompactionMemory_SkipsWhenReflectedSinceActivity: a session reflected
// after its last activity is not reflected again before compaction; the skip
// is logged and compaction still runs.
func TestCompactionMemory_SkipsWhenReflectedSinceActivity(t *testing.T) {
	initTestConvo(t, "helen") // a process-wide log: not parallel
	log := &cmLog{}
	a, _ := cmAgent(t, false, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartInbox(ctx)
	const sk = cmSession
	if _, err := a.DelegatedManager.Get(ctx, sk); err != nil {
		t.Fatalf("Get: %v", err)
	}
	a.SessionIndex.StampReflection(sk, time.Now())
	if !a.SessionIndex.ReflectionRedundant(sk) {
		t.Fatal("fixture: the session should read as reflected since its activity")
	}

	runWithin(t, "CompactSession", func() {
		if _, err := a.CompactSession(ctx, sk, false); err != nil {
			t.Errorf("CompactSession: %v", err)
		}
	})
	assertEvents(t, log.get(), "compact")
}
