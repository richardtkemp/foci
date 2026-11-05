package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/session"
)

// capMock is a mockBackendDM whose busy states are atomics, so a test can flip
// them while the live-session limiter reads them from other goroutines.
type capMock struct {
	*mockBackendDM
	inFlight  atomic.Bool
	subagents atomic.Bool
	awaiting  atomic.Bool
}

func (c *capMock) IsTurnInFlight() bool { return c.inFlight.Load() }

func (c *capMock) RunningSubagents() []delegator.RunningSubagent {
	if c.subagents.Load() {
		return []delegator.RunningSubagent{{ID: "sub-1"}}
	}
	return nil
}

func (c *capMock) AwaitingAutonomousRun() bool { return c.awaiting.Load() }

// capRig is a DelegatedManager with capMock backends and a recorded notice
// stream, for the live-session cap tests.
type capRig struct {
	m       *DelegatedManager
	mu      sync.Mutex
	mocks   []*capMock
	started []string // session keys in Start order
	notices chan string
	busy    sync.Map // session key → true: SessionBusy reports it busy
}

func newCapRig(t *testing.T, idx *session.SessionIndex, agentCap int, lim *LiveSessionLimiter) *capRig {
	t.Helper()
	r := &capRig{notices: make(chan string, 64)}
	if lim == nil {
		lim = NewLiveSessionLimiter(0)
	}
	lim.poll = 2 * time.Millisecond
	r.m = &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			c := &capMock{mockBackendDM: &mockBackendDM{running: true}}
			r.mu.Lock()
			r.mocks = append(r.mocks, c)
			r.mu.Unlock()
			return &startRecorder{capMock: c, rig: r}, nil
		},
		StartOpts:        delegator.StartOptions{WorkDir: t.TempDir()},
		AgentID:          "test-agent",
		SessionIndex:     idx,
		IdleTimeout:      time.Hour,
		LiveSessions:     lim,
		MaxLiveSessions:  agentCap,
		SystemNoticeFunc: func(sk, text string) { r.notices <- sk + "|" + text },
		SessionBusy: func(sk string) bool {
			_, ok := r.busy.Load(sk)
			return ok
		},
	}
	t.Cleanup(func() { r.m.Close() })
	return r
}

// startRecorder records the session key each backend starts on.
type startRecorder struct {
	*capMock
	rig *capRig
}

func (s *startRecorder) Start(ctx context.Context, opts delegator.StartOptions) error {
	if err := s.capMock.Start(ctx, opts); err != nil {
		return err
	}
	s.rig.mu.Lock()
	s.rig.started = append(s.rig.started, opts.SessionKey)
	s.rig.mu.Unlock()
	return nil
}

// mock returns the backend created for sessionKey (the latest one).
func (r *capRig) mock(t *testing.T, sessionKey string) *capMock {
	t.Helper()
	mb, ok := r.m.getManaged(sessionKey)
	if !ok {
		t.Fatalf("no backend for %s", sessionKey)
	}
	return mb.be.(*startRecorder).capMock
}

func (r *capRig) startedKeys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.started...)
}

// backdate sets a backend's last activity, to fix the LRU order.
func (r *capRig) backdate(sessionKey string, ago time.Duration) {
	r.m.mu.Lock()
	r.m.backends[sessionKey].lastActive = time.Now().Add(-ago)
	r.m.mu.Unlock()
}

// liveCount is the limiter's count of live places.
func liveCount(l *LiveSessionLimiter) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.live
}

func mustGet(t *testing.T, get func(context.Context, string) (delegator.Delegator, error), sk string) {
	t.Helper()
	if _, err := get(context.Background(), sk); err != nil {
		t.Fatalf("get %s: %v", sk, err)
	}
}

// captureLogs redirects the event log to a buffer for the test.
func captureLogs(t *testing.T) *syncBuf {
	t.Helper()
	b := &syncBuf{}
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(nil) })
	return b
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitNotice waits for one notice and returns it.
func waitNotice(t *testing.T, r *capRig) string {
	t.Helper()
	select {
	case n := <-r.notices:
		return n
	case <-time.After(10 * time.Second):
		t.Fatal("no queued notice")
		return ""
	}
}

// TestLiveCap_AgentCapEvictsLRUIdleAndResumes proves that with an agent cap of
// 2, a third user-turn creation evicts the least-recently-used idle backend,
// keeps its resume ID, and that session's next Get resumes with that ID.
func TestLiveCap_AgentCapEvictsLRUIdleAndResumes(t *testing.T) {
	idx := newTestSessionIndex(t)
	r := newCapRig(t, idx, 2, nil)
	mustGet(t, r.m.GetForUserTurn, "test-agent/c1")
	mustGet(t, r.m.GetForUserTurn, "test-agent/c2")
	c1 := r.mock(t, "test-agent/c1")
	c1.mu.Lock()
	c1.sessionID = "uuid-c1"
	c1.mu.Unlock()
	r.backdate("test-agent/c1", 2*time.Hour)
	r.backdate("test-agent/c2", time.Hour)

	mustGet(t, r.m.GetForUserTurn, "test-agent/c3")

	if !c1.wasClosed() {
		t.Fatal("LRU idle backend c1 should have been evicted")
	}
	if got := r.m.Count(); got != 2 {
		t.Fatalf("Count = %d, want 2 (the cap)", got)
	}
	if v, _ := idx.GetSessionMetadata("test-agent/c1", session.MetaKeyCCResumeID); v != "uuid-c1" {
		t.Fatalf("resume ID after eviction = %q, want uuid-c1 (kept, not a reset)", v)
	}

	mustGet(t, r.m.Get, "test-agent/c1")
	if got := r.mock(t, "test-agent/c1").startOpts.ResumeSessionID; got != "uuid-c1" {
		t.Fatalf("c1 respawn ResumeSessionID = %q, want uuid-c1", got)
	}
	if got := liveCount(r.m.LiveSessions); got != 2 {
		t.Fatalf("live places = %d, want 2", got)
	}
}

// TestLiveCap_EvictionSkipsBusyBackends proves eviction never picks a backend
// with a turn in flight, background work (subagents or an awaited autonomous
// run), a pending permission prompt, or a compaction/agent-level turn
// (SessionBusy): it evicts the next-oldest idle one instead.
func TestLiveCap_EvictionSkipsBusyBackends(t *testing.T) {
	r := newCapRig(t, nil, 6, nil)
	keys := []string{"test-agent/turn", "test-agent/sub", "test-agent/await", "test-agent/perm", "test-agent/compact", "test-agent/idle"}
	for i, k := range keys {
		mustGet(t, r.m.Get, k)
		r.backdate(k, time.Duration(len(keys)-i)*time.Hour) // first key oldest
	}
	r.mock(t, "test-agent/turn").inFlight.Store(true)
	r.mock(t, "test-agent/sub").subagents.Store(true)
	r.mock(t, "test-agent/await").awaiting.Store(true)
	r.m.SetPermissionPending("test-agent/perm", true)
	r.busy.Store("test-agent/compact", true)

	mustGet(t, r.m.GetForUserTurn, "test-agent/new")

	for _, k := range keys[:5] {
		if r.mock(t, k).wasClosed() {
			t.Errorf("busy backend %s was evicted", k)
		}
	}
	if _, ok := r.m.getManaged("test-agent/idle"); ok {
		t.Fatal("the idle backend should have been evicted")
	}
	if got := r.m.Count(); got != 6 {
		t.Fatalf("Count = %d, want 6", got)
	}
}

// TestLiveCap_UserCreationWaitsForBusyToGoIdle proves that with an agent cap
// of 1 and the only backend busy, a user-turn creation sends exactly one
// queued notice with the #2281 text, blocks, and completes once that backend
// goes idle — without anything closing it — by evicting it.
func TestLiveCap_UserCreationWaitsForBusyToGoIdle(t *testing.T) {
	logs := captureLogs(t)
	r := newCapRig(t, nil, 1, nil)
	mustGet(t, r.m.Get, "test-agent/busy")
	busy := r.mock(t, "test-agent/busy")
	busy.inFlight.Store(true)

	done := make(chan error, 1)
	go func() {
		_, err := r.m.GetForUserTurn(context.Background(), "test-agent/waiter")
		done <- err
	}()
	if got := waitNotice(t, r); got != "test-agent/waiter|"+turnQueuedNotice {
		t.Fatalf("notice = %q, want the queued notice for the waiter", got)
	}
	// Let the waiter poll several times while the backend stays busy.
	waitPolls(t, r.m.LiveSessions, 5)
	select {
	case err := <-done:
		t.Fatalf("user creation returned while the cap was full and nothing idle: %v", err)
	default:
	}
	if busy.wasClosed() {
		t.Fatal("busy backend must not be evicted")
	}

	busy.inFlight.Store(false) // the turn ends: no close, no place freed by itself
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("GetForUserTurn: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waiter never got a place after the busy backend went idle")
	}
	if !busy.wasClosed() {
		t.Fatal("the formerly busy backend should have been evicted to make room")
	}
	if got := r.m.Count(); got != 1 {
		t.Fatalf("Count = %d, want 1", got)
	}
	select {
	case n := <-r.notices:
		t.Fatalf("second notice sent: %q", n)
	default:
	}
	out := logs.String()
	for _, want := range []string{
		"session test-agent/waiter waits for a live-session place (agent cap 1 full",
		"session test-agent/waiter got a live-session place after waiting",
		"evicting idle session test-agent/busy to make room (agent cap 1 full; idle",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q", want)
		}
	}
}

// waitPolls blocks until the limiter has run at least n more eviction scans
// (each poll of a waiter runs one), so a test proves a waiter kept waiting
// across polls without sleeping a fixed time.
func waitPolls(t *testing.T, l *LiveSessionLimiter, n int64) {
	t.Helper()
	start := l.scans.Load()
	deadline := time.Now().Add(10 * time.Second)
	for l.scans.Load() < start+n {
		if time.Now().After(deadline) {
			t.Fatalf("limiter ran %d scans, want %d", l.scans.Load()-start, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestLiveCap_SystemCreationGoesOverCap proves a system creation (Get) in the
// same situation does not wait: it creates over the cap, sends no notice and
// logs the over-cap line; once the busy backend is idle, the next creation
// brings the count back to the cap.
func TestLiveCap_SystemCreationGoesOverCap(t *testing.T) {
	logs := captureLogs(t)
	r := newCapRig(t, nil, 1, nil)
	mustGet(t, r.m.Get, "test-agent/busy")
	busy := r.mock(t, "test-agent/busy")
	busy.inFlight.Store(true)

	mustGet(t, r.m.Get, "test-agent/system")
	if got := r.m.Count(); got != 2 {
		t.Fatalf("Count = %d, want 2 (one over the cap)", got)
	}
	select {
	case n := <-r.notices:
		t.Fatalf("system creation sent a notice: %q", n)
	default:
	}
	if want := "session test-agent/system created over the agent cap 1 (2 live)"; !strings.Contains(logs.String(), want) {
		t.Fatalf("log missing %q:\n%s", want, logs.String())
	}

	busy.inFlight.Store(false)
	mustGet(t, r.m.Get, "test-agent/next")
	if got := r.m.Count(); got != 1 {
		t.Fatalf("Count after the next creation = %d, want 1 (back at the cap)", got)
	}
	if got := liveCount(r.m.LiveSessions); got != 1 {
		t.Fatalf("live places = %d, want 1", got)
	}
}

// TestLiveCap_SystemOverCapTrimsOnClose proves the other half of "back to the
// cap": after a system creation went over, a close brings the count back down
// by evicting what has become idle.
func TestLiveCap_SystemOverCapTrimsOnClose(t *testing.T) {
	r := newCapRig(t, nil, 1, nil)
	for _, k := range []string{"test-agent/a", "test-agent/b", "test-agent/c"} {
		mustGet(t, r.m.Get, k)
		r.mock(t, k).inFlight.Store(true)
	}
	if got := r.m.Count(); got != 3 {
		t.Fatalf("Count = %d, want 3", got)
	}
	r.mock(t, "test-agent/a").inFlight.Store(false)
	r.mock(t, "test-agent/b").inFlight.Store(false)
	r.m.ResetSession("test-agent/c") // a close
	deadline := time.Now().Add(10 * time.Second)
	for r.m.Count() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("Count = %d after a close, want 1", r.m.Count())
		}
		time.Sleep(time.Millisecond)
	}
}

// TestLiveCap_GlobalCapEvictsAcrossAgents proves the gateway cap is shared by
// every manager: with a global cap of 1, a user-turn creation on agent 2
// evicts agent 1's idle backend.
func TestLiveCap_GlobalCapEvictsAcrossAgents(t *testing.T) {
	lim := NewLiveSessionLimiter(1)
	r1 := newCapRig(t, nil, 0, lim)
	r2 := newCapRig(t, nil, 0, lim)
	r2.m.AgentID = "agent-two"
	mustGet(t, r1.m.GetForUserTurn, "test-agent/a1")
	a1 := r1.mock(t, "test-agent/a1")

	mustGet(t, r2.m.GetForUserTurn, "agent-two/a2")
	if !a1.wasClosed() {
		t.Fatal("agent 1's idle backend should have been evicted for agent 2")
	}
	if r1.m.Count() != 0 || r2.m.Count() != 1 {
		t.Fatalf("counts = %d/%d, want 0/1", r1.m.Count(), r2.m.Count())
	}
	if got := liveCount(lim); got != 1 {
		t.Fatalf("global live = %d, want 1", got)
	}
}

// TestLiveCap_WaitersServedFIFO proves two waiting user creations are served
// in arrival order: the first waiter's backend starts before the second's,
// whichever of them evicts.
func TestLiveCap_WaitersServedFIFO(t *testing.T) {
	r := newCapRig(t, nil, 1, nil)
	mustGet(t, r.m.Get, "test-agent/busy")
	busy := r.mock(t, "test-agent/busy")
	busy.inFlight.Store(true)

	done := make(chan string, 2)
	launch := func(sk string) {
		go func() {
			if _, err := r.m.GetForUserTurn(context.Background(), sk); err != nil {
				t.Errorf("GetForUserTurn %s: %v", sk, err)
			}
			done <- sk
		}()
		waitNotice(t, r) // queued before the next one arrives
	}
	launch("test-agent/first")
	launch("test-agent/second")

	busy.inFlight.Store(false)
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("waiters did not complete")
		}
	}
	started := r.startedKeys()
	if len(started) != 3 || started[1] != "test-agent/first" || started[2] != "test-agent/second" {
		t.Fatalf("start order = %v, want busy, first, second", started)
	}
}

// TestLiveCap_CancelledWaitLeaksNothing proves a waiting user creation whose
// ctx is cancelled returns the ctx error, creates nothing, and gives up no
// place: a later creation still gets it.
func TestLiveCap_CancelledWaitLeaksNothing(t *testing.T) {
	r := newCapRig(t, nil, 1, nil)
	mustGet(t, r.m.Get, "test-agent/busy")
	busy := r.mock(t, "test-agent/busy")
	busy.inFlight.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.m.GetForUserTurn(ctx, "test-agent/waiter")
		done <- err
	}()
	waitNotice(t, r)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := len(r.startedKeys()); n != 1 {
		t.Fatalf("%d backends started, want 1 (the cancelled wait creates nothing)", n)
	}
	if got := liveCount(r.m.LiveSessions); got != 1 {
		t.Fatalf("live places = %d, want 1 (no leak)", got)
	}

	busy.inFlight.Store(false)
	mustGet(t, r.m.GetForUserTurn, "test-agent/later")
	if got := r.m.Count(); got != 1 {
		t.Fatalf("Count = %d, want 1", got)
	}
	select {
	case n := <-r.notices:
		t.Fatalf("later creation should not have waited, got notice %q", n)
	default:
	}
}

// TestLiveCap_ShutdownEndsWait proves BeginShutdown's refuseNewBackends ends a
// waiting user creation with ErrShuttingDown.
func TestLiveCap_ShutdownEndsWait(t *testing.T) {
	r := newCapRig(t, nil, 1, nil)
	mustGet(t, r.m.Get, "test-agent/busy")
	r.mock(t, "test-agent/busy").inFlight.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := r.m.GetForUserTurn(context.Background(), "test-agent/waiter")
		done <- err
	}()
	waitNotice(t, r)
	r.m.refuseNewBackends()
	select {
	case err := <-done:
		if !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("err = %v, want ErrShuttingDown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not end the wait")
	}
	if got := liveCount(r.m.LiveSessions); got != 1 {
		t.Fatalf("live places = %d, want 1", got)
	}
}

// TestLiveCap_ConcurrentCreationsNeverExceedCap proves concurrent user
// creations on distinct keys never hold more places than the cap, counting
// creations still in progress (run with -race).
func TestLiveCap_ConcurrentCreationsNeverExceedCap(t *testing.T) {
	const capN = 3
	r := newCapRig(t, nil, capN, nil)
	var maxLive atomic.Int64
	inner := r.m.NewBackend
	r.m.NewBackend = func() (delegator.Delegator, error) {
		if n := int64(liveCount(r.m.LiveSessions)); n > maxLive.Load() {
			maxLive.Store(n)
		}
		return inner()
	}
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := r.m.GetForUserTurn(context.Background(), fmt.Sprintf("test-agent/c%d", i)); err != nil {
				t.Errorf("GetForUserTurn: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if got := maxLive.Load(); got > capN {
		t.Fatalf("live places reached %d, cap %d", got, capN)
	}
	if got := r.m.Count(); got > capN {
		t.Fatalf("Count = %d, cap %d", got, capN)
	}
	if got, want := liveCount(r.m.LiveSessions), r.m.Count(); got != want {
		t.Fatalf("live places = %d, mapped backends = %d: a place leaked", got, want)
	}
}

// TestLiveCap_FailedStartFreesPlace proves a creation whose Start fails gives
// its place back: with a cap of 1, the next creation needs no eviction and no
// wait.
func TestLiveCap_FailedStartFreesPlace(t *testing.T) {
	r := newCapRig(t, nil, 1, nil)
	inner := r.m.NewBackend
	fail := atomic.Bool{}
	fail.Store(true)
	r.m.NewBackend = func() (delegator.Delegator, error) {
		be, err := inner()
		if err == nil && fail.Load() {
			be.(*startRecorder).startErr = errors.New("boom")
		}
		return be, err
	}
	if _, err := r.m.GetForUserTurn(context.Background(), "test-agent/fails"); err == nil {
		t.Fatal("expected the Start error")
	}
	if got := liveCount(r.m.LiveSessions); got != 0 {
		t.Fatalf("live places after a failed Start = %d, want 0", got)
	}
	fail.Store(false)
	mustGet(t, r.m.GetForUserTurn, "test-agent/ok")
	select {
	case n := <-r.notices:
		t.Fatalf("unexpected notice %q", n)
	default:
	}
}

// TestLiveCap_NoCapsChangesNothing pins today's behaviour with both caps 0:
// any number of backends, no eviction, no notice, no cap log line.
func TestLiveCap_NoCapsChangesNothing(t *testing.T) {
	logs := captureLogs(t)
	r := newCapRig(t, nil, 0, nil)
	for i := 0; i < 12; i++ {
		mustGet(t, r.m.GetForUserTurn, fmt.Sprintf("test-agent/c%d", i))
	}
	if got := r.m.Count(); got != 12 {
		t.Fatalf("Count = %d, want 12", got)
	}
	r.mu.Lock()
	for _, c := range r.mocks {
		if c.wasClosed() {
			t.Error("a backend was closed with no cap")
		}
	}
	r.mu.Unlock()
	select {
	case n := <-r.notices:
		t.Fatalf("unexpected notice %q", n)
	default:
	}
	for _, s := range []string{"live-session place", "evicting idle session", "created over the"} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("unexpected log line containing %q", s)
		}
	}
}

// TestIdleReapPeriod proves the idle reaper checks often enough that a backend
// closes at most half its timeout (never more than 10m) late: a 10m timeout
// closes within 15m, and the 3h default keeps today's 10-minute check.
func TestIdleReapPeriod(t *testing.T) {
	if got := idleReapPeriod(10 * time.Minute); got+10*time.Minute > 15*time.Minute {
		t.Errorf("10m timeout: period %s lets a backend live past 15m", got)
	}
	if got := idleReapPeriod(DefaultIdleTimeout); got != 10*time.Minute {
		t.Errorf("default timeout: period %s, want 10m (unchanged)", got)
	}
}

// TestCloseFinishedSession_ClosesIdleKeepsResume proves a finished spawn's
// branch backend is closed at once (not left 3h for the idle reaper, which
// kept a codex thread loaded all that time), with its resume ID kept so a
// spawn_send resumes the same conversation; and that a backend with work in
// progress is left alone.
func TestCloseFinishedSession_ClosesIdleKeepsResume(t *testing.T) {
	idx := newTestSessionIndex(t)
	r := newCapRig(t, idx, 0, nil)
	keys := []string{"test-agent/turn", "test-agent/sub", "test-agent/await", "test-agent/perm", "test-agent/compact", "test-agent/done"}
	for _, k := range keys {
		mustGet(t, r.m.Get, k)
	}
	r.mock(t, "test-agent/turn").inFlight.Store(true)
	r.mock(t, "test-agent/sub").subagents.Store(true)
	r.mock(t, "test-agent/await").awaiting.Store(true)
	r.m.SetPermissionPending("test-agent/perm", true)
	r.busy.Store("test-agent/compact", true)
	done := r.mock(t, "test-agent/done")
	done.mu.Lock()
	done.sessionID = "thread-done"
	done.mu.Unlock()

	for _, k := range keys[:5] {
		if r.m.CloseFinishedSession(k, "test") {
			t.Errorf("busy backend %s was closed", k)
		}
		if r.mock(t, k).wasClosed() {
			t.Errorf("busy backend %s was closed", k)
		}
	}
	if !r.m.CloseFinishedSession("test-agent/done", "test") {
		t.Fatal("CloseFinishedSession = false for an idle backend")
	}
	if !done.wasClosed() {
		t.Fatal("the finished backend was not closed")
	}
	if _, ok := r.m.getManaged("test-agent/done"); ok {
		t.Fatal("the finished backend is still mapped")
	}
	if v, _ := idx.GetSessionMetadata("test-agent/done", session.MetaKeyCCResumeID); v != "thread-done" {
		t.Fatalf("resume ID after close = %q, want thread-done (kept)", v)
	}
	if r.m.CloseFinishedSession("test-agent/done", "test") {
		t.Fatal("second CloseFinishedSession reported a close")
	}
	mustGet(t, r.m.Get, "test-agent/done")
	if got := r.mock(t, "test-agent/done").startOpts.ResumeSessionID; got != "thread-done" {
		t.Fatalf("resumed ResumeSessionID = %q, want thread-done", got)
	}
}
