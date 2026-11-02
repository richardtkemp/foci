package periodic

import (
	"context"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/session"
	"foci/internal/usagehold"
)

// holdFakeAgent is fakeBackgroundAgent plus the sessions a usage hold paused.
type holdFakeAgent struct {
	*fakeBackgroundAgent
	mu     sync.Mutex
	paused map[string]string
}

func (h *holdFakeAgent) UsageHoldPaused() map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]string{}
	for k, v := range h.paused {
		out[k] = v
	}
	return out
}

type holdSession struct {
	key       string
	cacheAge  time.Duration // -1 = never touched
	fill      int           // of a 200000 trigger
	noCompact bool
}

// newHoldRunner builds a Runner (TTL 1 h) whose chats are chats and whose
// paused sessions are paused, at now.
func newHoldRunner(t *testing.T, now time.Time, chats []holdSession, paused []holdSession) (*Runner, *holdFakeAgent) {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	usage := map[string]int{}
	var open []string
	add := func(s holdSession) {
		idx.Upsert(session.SessionIndexEntry{SessionKey: s.key, FilePath: "/tmp/t.jsonl", CreatedAt: now.Add(-24 * time.Hour),
			SessionType: session.SessionTypeChat, Status: session.SessionStatusActive})
		if s.cacheAge >= 0 {
			idx.TouchCacheTouch(s.key, now.Add(-s.cacheAge))
		}
		if s.noCompact {
			_ = idx.SetSessionMetadata(s.key, session.MetaKeyNoCompact, "true")
		}
		usage[s.key] = s.fill
	}
	for _, s := range chats {
		add(s)
		open = append(open, s.key)
	}
	fake := &holdFakeAgent{paused: map[string]string{}}
	for _, s := range paused {
		add(s)
		fake.paused[s.key] = "spawn"
	}
	fake.fakeBackgroundAgent = &fakeBackgroundAgent{
		sessionKeyFn:   func() string { return chats[0].key },
		contextUsageFn: func(sk string) (int, int) { return usage[sk], 200000 },
	}
	r := New(RunnerConfig{
		AgentID:            "test",
		SessionIndex:       idx,
		Agent:              fake,
		CacheTTL:           time.Hour,
		OpenChatSessionsFn: func() []string { return open },
		Keepalive:          config.ResolvedKeepalive{Enabled: true, Interval: "50m"},
	})
	return r, fake
}

// TestUsageHold_CompactionRule is holdout H10 and worked example A: TTL 1 h,
// weekly 0.98 with reset 12 h away. S1 (used 20 min ago, 60% of its trigger)
// is compacted; S2 (30%) and S3 (used 2 h ago) are not; with the reset 45 min
// away none is. A paused spawn at 60%, warm, is compacted despite foci's own
// no_compact on it; a chat whose owner set no_compact is not.
func TestUsageHold_CompactionRule(t *testing.T) {
	now := time.Now()
	chats := []holdSession{
		{key: "test/c1", cacheAge: 20 * time.Minute, fill: 120000},
		{key: "test/c2", cacheAge: 20 * time.Minute, fill: 60000},
		{key: "test/c3", cacheAge: 2 * time.Hour, fill: 120000},
		{key: "test/c4", cacheAge: 20 * time.Minute, fill: 120000, noCompact: true},
	}
	paused := []holdSession{{key: "test/c1/b7", cacheAge: 20 * time.Minute, fill: 120000, noCompact: true}}
	r, _ := newHoldRunner(t, now, chats, paused)

	got := r.holdCompactionTargets(now.Add(12*time.Hour), now)
	sort.Strings(got)
	if want := []string{"test/c1", "test/c1/b7"}; !equalStrings(got, want) {
		t.Errorf("reset 12 h away: compacted %v, want %v", got, want)
	}
	if got := r.holdCompactionTargets(now.Add(45*time.Minute), now); len(got) != 0 {
		t.Errorf("reset 45 min away: compacted %v, want none", got)
	}
	if got := r.holdCompactionTargets(time.Time{}, now); len(got) != 0 {
		t.Errorf("unknown reset: compacted %v, want none", got)
	}
}

// TestUsageHold_ReflectThenCompactWithPermit: the hold's compaction reflects
// each target first, then compacts it, both under the hold's permit.
func TestUsageHold_ReflectThenCompactWithPermit(t *testing.T) {
	now := time.Now()
	r, fake := newHoldRunner(t, now, []holdSession{{key: "test/c1", cacheAge: 10 * time.Minute, fill: 150000}}, nil)
	r.sessionIndex.StampReflection("test/c1", now.Add(-48*time.Hour)) // before the seeded activity: reflection due
	var mu sync.Mutex
	var steps []string
	fake.branchCtxFn = func(ctx context.Context, branchType, parentKey, _ string, _ bool) bool {
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, branchType+":"+parentKey+":"+permit(ctx))
		return true
	}
	fake.quietCompactFn = func(ctx context.Context, sk string) error {
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, "compact:"+sk+":"+permit(ctx))
		return nil
	}
	r.holdCompaction(context.Background(), now.Add(12*time.Hour))
	want := []string{"reflection:test/c1:permit", "compact:test/c1:permit"}
	if !equalStrings(steps, want) {
		t.Errorf("steps = %v, want %v", steps, want)
	}
}

func permit(ctx context.Context) string {
	if usagehold.Permitted(ctx) {
		return "permit"
	}
	return "none"
}

// TestUsageHold_KeepaliveWarmsPausedSpawns is holdout H12 and worked example
// E: with max_user_idle 2 h, a paused spawn nobody touched is warmed during a
// hold in its warm window; with nothing paused (no hold) no spawn key is
// ever a keepalive target.
func TestUsageHold_KeepaliveWarmsPausedSpawns(t *testing.T) {
	now := time.Now()
	chats := []holdSession{{key: "test/c1", cacheAge: 55 * time.Minute, fill: 1}}
	paused := []holdSession{{key: "test/c1/b3", cacheAge: 55 * time.Minute, fill: 1}}
	r, fake := newHoldRunner(t, now, chats, paused)
	r.kaCfg.MaxUserIdle = "2h"
	interval := 50 * time.Minute

	ready, _ := r.keepaliveTargetsAt(interval, now)
	if !containsString(ready, "test/c1/b3") {
		t.Errorf("during a hold: keepalive targets %v, want the paused spawn", ready)
	}
	if containsString(ready, "test/c1") {
		t.Errorf("the chat with no user activity passed max_user_idle: %v", ready)
	}

	fake.mu.Lock()
	fake.paused = map[string]string{} // the hold lifted: nothing is paused
	fake.mu.Unlock()
	ready, _ = r.keepaliveTargetsAt(interval, now)
	if containsString(ready, "test/c1/b3") {
		t.Errorf("outside a hold: keepalive targets %v include a spawn", ready)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// idleFake is a holdFakeAgent whose turn on each session ends when the test
// says so (WaitTurnIdle: the agent's in-flight signal).
type idleFake struct {
	*holdFakeAgent
	ended   chan struct{}
	waiting chan struct{}
}

func (f *idleFake) WaitTurnIdle(ctx context.Context, _ string) bool {
	if f.waiting != nil {
		f.waiting <- struct{}{}
	}
	select {
	case <-f.ended:
		return true
	case <-ctx.Done():
		return false
	}
}

// TestUsageHold_CompactionWaitsForTurnEnd: the hold's compaction of a session
// waits for its interrupted turn's real end (the in-flight signal), not a
// timer; once the turn ends it compacts.
func TestUsageHold_CompactionWaitsForTurnEnd(t *testing.T) {
	now := time.Now()
	r, hold := newHoldRunner(t, now, []holdSession{{key: "test/c1", cacheAge: 10 * time.Minute, fill: 150000}}, nil)
	f := &idleFake{holdFakeAgent: hold, ended: make(chan struct{}), waiting: make(chan struct{}, 1)}
	r.agent = f
	compacted := make(chan string, 1)
	hold.quietCompactFn = func(_ context.Context, sk string) error { compacted <- sk; return nil }
	done := make(chan struct{})
	go func() { r.holdCompaction(context.Background(), now.Add(12*time.Hour)); close(done) }()
	select {
	case <-f.waiting: // the compaction is waiting on the turn's end
	case <-done:
		t.Fatal("the hold's compaction finished without waiting for the turn's end")
	}
	select {
	case sk := <-compacted:
		t.Fatalf("compacted %s while its turn was still in flight", sk)
	default:
	}
	close(f.ended) // the interrupted turn ends
	<-done
	select {
	case sk := <-compacted:
		if sk != "test/c1" {
			t.Errorf("compacted %s, want test/c1", sk)
		}
	default:
		t.Fatal("not compacted after its turn ended")
	}
}

// TestUsageHold_CompactionIdleWaitIsBounded: a turn that never ends does not
// stall the hold's compaction past the bound; it goes on (the compaction's
// own guard re-checks).
func TestUsageHold_CompactionIdleWaitIsBounded(t *testing.T) {
	now := time.Now()
	r, hold := newHoldRunner(t, now, []holdSession{{key: "test/c1", cacheAge: 10 * time.Minute, fill: 150000}}, nil)
	r.agent = &idleFake{holdFakeAgent: hold, ended: make(chan struct{})} // never ends
	r.holdIdleTimeout = time.Millisecond
	var got []string
	hold.quietCompactFn = func(_ context.Context, sk string) error { got = append(got, sk); return nil }
	r.holdCompaction(context.Background(), now.Add(12*time.Hour))
	if len(got) != 1 {
		t.Errorf("compactions after the bounded wait = %v, want one attempt", got)
	}
}
