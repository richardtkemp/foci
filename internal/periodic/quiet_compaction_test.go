package periodic

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/clock"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/session"
	"foci/internal/timeutil"
)

// Fixture notes. The quiet trigger reads time through an injectable clock,
// so no test here depends on the wall clock for its window, idle or
// cache-warmth verdicts: the fake clock reads an exact instant and the
// process timezone is pinned to UTC so a host in another timezone cannot
// shift "02:00" out of a window. Idle and cache-touch rows are seeded
// relative to the fake "now". The one deliberate exception is
// TestTick_KeepaliveYieldsToQuietCompaction, whose keepalive side reads the
// REAL clock (that is keepalive's behaviour); its cache touch is seeded
// relative to the real now for that reason.

// useUTC pins the process timezone to UTC for one test and restores it after.
func useUTC(t *testing.T) {
	t.Helper()
	prev := timeutil.Location()
	timeutil.SetLocation(time.UTC)
	t.Cleanup(func() { timeutil.SetLocation(prev) })
}

// fakeClockAt returns a Fake clock reading exactly `at`.
func fakeClockAt(at time.Time) *clock.Fake {
	fc := clock.NewFake()
	fc.Advance(at.Sub(time.Unix(0, 0)))
	return fc
}

// quietSession seeds one candidate. Negative idle/cacheAge means "not
// recorded".
type quietSession struct {
	key       string
	idle      time.Duration
	cacheAge  time.Duration
	fill      int
	limit     int
	noCompact bool
}

// quietOpts configures newQuietFixture. Zero values get the eligible
// defaults: window "01:00-05:00", threshold 0.5, min idle 30m, cache TTL 1h,
// and one session 41m idle with a 10m-old cache touch at 62% of a 200k
// window — the state in which a quiet compaction must fire.
type quietOpts struct {
	at         time.Time // fake "now"; zero = 02:00 UTC on 2026-10-07
	window     string
	threshold  float64 // 0 = 0.5
	minIdle    string  // "" = "30m"
	cacheTTL   time.Duration // 0 = 1h
	defaultKey string       // "" = "test/c1"
	sessions   []quietSession
	openChats  []string
	// outcomes overrides the QuietCompact result per session (nil = success).
	outcomes map[string]error
	// onQuiet runs inside quietCompactFn after the call is counted.
	onQuiet func(sk string)
}

func (o quietOpts) windowOrDefault() string {
	if o.window == "" {
		return "01:00-05:00"
	}
	return o.window
}

// quietFixture is a Runner wired for one quiet-compaction scenario.
type quietFixture struct {
	r    *Runner
	idx  *session.SessionIndex
	fc   *clock.Fake
	fake *fakeBackgroundAgent
	// compacted counts QuietCompact calls per session, order records the
	// dispatch sequence, notes collects the notify texts. All are written on
	// the dispatch goroutine and only read after waitIdle, whose flag-clear
	// under r.mu gives the happens-before edge.
	compacted map[string]int
	order     []string
	notes     []quietNote
}

type quietNote struct {
	sessionKey string
	text       string
}

func newQuietFixture(t *testing.T, o quietOpts) *quietFixture {
	t.Helper()
	useUTC(t)

	at := o.at
	if at.IsZero() {
		at = time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC)
	}
	threshold := o.threshold
	if threshold == 0 {
		threshold = 0.5
	}
	minIdle := o.minIdle
	if minIdle == "" {
		minIdle = "30m"
	}
	cacheTTL := o.cacheTTL
	if cacheTTL == 0 {
		cacheTTL = time.Hour
	}
	defaultKey := o.defaultKey
	if defaultKey == "" {
		defaultKey = "test/c1"
	}
	if len(o.sessions) == 0 {
		o.sessions = []quietSession{{
			key: defaultKey, idle: 41 * time.Minute, cacheAge: 10 * time.Minute,
			fill: 124000, limit: 200000,
		}}
	}

	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })

	usage := make(map[string][2]int, len(o.sessions))
	for _, s := range o.sessions {
		idx.Upsert(session.SessionIndexEntry{
			SessionKey:  s.key,
			FilePath:    "/tmp/test.jsonl",
			CreatedAt:   at.Add(-24 * time.Hour),
			SessionType: session.SessionTypeChat,
			Status:      session.SessionStatusActive,
		})
		if s.idle >= 0 {
			idx.TouchUserActivity(s.key, at.Add(-s.idle))
		}
		if s.cacheAge >= 0 {
			idx.TouchCacheTouch(s.key, at.Add(-s.cacheAge))
		}
		if s.noCompact {
			if err := idx.SetSessionMetadata(s.key, session.MetaKeyNoCompact, "true"); err != nil {
				t.Fatal(err)
			}
		}
		usage[s.key] = [2]int{s.fill, s.limit}
	}

	f := &quietFixture{
		idx:       idx,
		fc:        fakeClockAt(at),
		compacted: make(map[string]int),
	}
	f.fake = &fakeBackgroundAgent{
		sessionKeyFn: func() string { return defaultKey },
		contextUsageFn: func(sk string) (int, int) {
			u := usage[sk]
			return u[0], u[1]
		},
		quietCompactFn: func(ctx context.Context, sk string) error {
			f.compacted[sk]++
			f.order = append(f.order, sk)
			if o.onQuiet != nil {
				o.onQuiet(sk)
			}
			if err, ok := o.outcomes[sk]; ok {
				return err
			}
			return nil
		},
	}

	f.r = &Runner{
		log:          log.NewComponentLogger("keepalive:test"),
		agentID:      "test",
		sessionIndex: idx,
		agent:        f.fake,
		quietCfg: config.ResolvedQuietCompaction{
			Window:    o.windowOrDefault(),
			Threshold: threshold,
			MinIdle:   minIdle,
		},
		cacheTTL:           cacheTTL,
		clock:              f.fc,
		openChatSessionsFn: func() []string { return o.openChats },
		notifyQuietCompact: func(sk, text string) {
			f.notes = append(f.notes, quietNote{sessionKey: sk, text: text})
		},
		done: make(chan struct{}),
	}
	return f
}

// stamp returns the session's recorded quiet-compaction stamp, if any.
func (f *quietFixture) stamp(sk string) (time.Time, bool) {
	v, err := f.idx.GetSessionMetadata(sk, session.MetaKeyQuietCompactedAt)
	if err != nil || v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// tripwireAgent fails the test from any BackgroundAgent lookup — proves the
// trigger returns before touching sessions, the ledger or the agent.
func tripwireAgent(t *testing.T) *fakeBackgroundAgent {
	t.Helper()
	bang := func(what string) {
		t.Errorf("quiet compaction reached %s although the trigger must be a no-op", what)
	}
	return &fakeBackgroundAgent{
		sessionKeyFn:     func() string { bang("SessionKey"); return "test/c1" },
		contextUsageFn:   func(string) (int, int) { bang("ContextUsage"); return 0, 0 },
		lastTurnEndFn:    func(string) time.Time { bang("LastTurnEnd"); return time.Time{} },
		quietBlockedFn:   func(string) string { bang("QuietCompactBlocked"); return "" },
		quietCompactFn:   func(context.Context, string) error { bang("QuietCompact"); return nil },
		rateLimitedFn:    func(string) (bool, string) { bang("RateLimited"); return false, "" },
		isTurnInFlightFn: func(string) bool { bang("IsTurnInFlight"); return false },
	}
}

func TestMaybeQuietCompaction_OffByDefault_NoLookups(t *testing.T) {
	// Proves requirement 4: with no window configured (the default), the
	// trigger is an exact no-op — not one session, ledger or agent lookup.
	useUTC(t)
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		agent:   tripwireAgent(t),
		quietCfg: config.ResolvedQuietCompaction{
			Window: "", Threshold: 0.5, MinIdle: "30m",
		},
		clock: fakeClockAt(time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC)),
		openChatSessionsFn: func() []string {
			t.Error("quiet compaction reached openChatSessions although off")
			return nil
		},
		done: make(chan struct{}),
	}
	r.maybeQuietCompaction(context.Background())
}

func TestMaybeQuietCompaction_OutsideWindow_NoLookups(t *testing.T) {
	// Proves requirement 4: outside the configured window the trigger
	// returns before any lookup — with the feature ON but the clock at
	// midday against a 01:00-05:00 window.
	useUTC(t)
	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		agent:   tripwireAgent(t),
		quietCfg: config.ResolvedQuietCompaction{
			Window: "01:00-05:00", Threshold: 0.5, MinIdle: "30m",
		},
		clock: fakeClockAt(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)),
		openChatSessionsFn: func() []string {
			t.Error("quiet compaction reached openChatSessions although outside the window")
			return nil
		},
		done: make(chan struct{}),
	}
	r.maybeQuietCompaction(context.Background())
}

func TestMaybeQuietCompaction_FiresInsideWindowWhenIdle(t *testing.T) {
	// The happy path: inside the window, idle past the minimum, at 62% of a
	// 200k window against a 50% threshold, cache still warm, no guards —
	// exactly one compaction, one stamp, and one chat note naming the
	// trigger, the fill percentage, the threshold and the idle time.
	f := newQuietFixture(t, quietOpts{
		at:    time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC),
		window: "01:00-05:00",
	})

	f.r.maybeQuietCompaction(context.Background())
	waitIdle(t, f.r)

	if got := f.compacted["test/c1"]; got != 1 {
		t.Fatalf("quiet compactions for test/c1 = %d, want 1", got)
	}
	if _, ok := f.stamp("test/c1"); !ok {
		t.Error("no quiet_compacted_at stamp written after a successful compaction")
	}
	if len(f.notes) != 1 {
		t.Fatalf("notes = %d, want 1", len(f.notes))
	}
	note := f.notes[0]
	if note.sessionKey != "test/c1" {
		t.Errorf("note addressed to %q, want test/c1", note.sessionKey)
	}
	for _, want := range []string{"Quiet-hours compaction", "62%", "50%", "41m"} {
		if !strings.Contains(note.text, want) {
			t.Errorf("note %q missing %q", note.text, want)
		}
	}
}

func TestMaybeQuietCompaction_WrappingWindowBoundaries(t *testing.T) {
	// The 23:00-07:00 window wraps midnight: 02:00 fires, midday does not,
	// the 23:00 start is inclusive and the 07:00 end exclusive.
	cases := []struct {
		hhmm time.Time
		fire bool
	}{
		{time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC), true},
		{time.Date(2026, 10, 7, 23, 0, 0, 0, time.UTC), true}, // start inclusive
		{time.Date(2026, 10, 7, 23, 1, 0, 0, time.UTC), true},
		{time.Date(2026, 10, 7, 6, 59, 0, 0, time.UTC), true},
		{time.Date(2026, 10, 7, 7, 0, 0, 0, time.UTC), false}, // end exclusive
		{time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC), false},
		{time.Date(2026, 10, 7, 22, 59, 0, 0, time.UTC), false},
	}
	for _, tc := range cases {
		f := newQuietFixture(t, quietOpts{
			at:     tc.hhmm,
			window: "23:00-07:00",
		})
		f.r.maybeQuietCompaction(context.Background())
		waitIdle(t, f.r)
		got := f.compacted["test/c1"]
		if (got == 1) != tc.fire || got > 1 {
			t.Errorf("window 23:00-07:00 at %s: compactions = %d, want fire = %v", tc.hhmm.Format("15:04"), got, tc.fire)
		}
	}
}

func TestMaybeQuietCompaction_BelowThreshold(t *testing.T) {
	// Requirement 6c: a session at 40% of the window against a 50%
	// threshold is left alone.
	f := newQuietFixture(t, quietOpts{
		sessions: []quietSession{{
			key: "test/c1", idle: 41 * time.Minute, cacheAge: 10 * time.Minute,
			fill: 80000, limit: 200000,
		}},
	})
	f.r.maybeQuietCompaction(context.Background())
	waitIdle(t, f.r)
	if got := f.compacted["test/c1"]; got != 0 {
		t.Errorf("compactions below the threshold = %d, want 0", got)
	}
}

func TestMaybeQuietCompaction_MinIdleNotReached(t *testing.T) {
	// Requirement 6b: idle 10m against a 30m minimum — no compaction.
	f := newQuietFixture(t, quietOpts{
		sessions: []quietSession{{
			key: "test/c1", idle: 10 * time.Minute, cacheAge: 5 * time.Minute,
			fill: 124000, limit: 200000,
		}},
	})
	f.r.maybeQuietCompaction(context.Background())
	waitIdle(t, f.r)
	if got := f.compacted["test/c1"]; got != 0 {
		t.Errorf("compactions before the idle minimum = %d, want 0", got)
	}
}

func TestMaybeQuietCompaction_OncePerWindow(t *testing.T) {
	// Requirement 6d, first rule: a second check inside the SAME window
	// occurrence does not fire again — the stamp from the first attempt
	// covers the rest of the night.
	f := newQuietFixture(t, quietOpts{
		at:     time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC),
		window: "01:00-05:00",
	})

	f.r.maybeQuietCompaction(context.Background())
	waitIdle(t, f.r)
	if got := f.compacted["test/c1"]; got != 1 {
		t.Fatalf("first check: compactions = %d, want 1", got)
	}

	// 30 minutes later, still inside 01:00-05:00 (and idle 71m ≥ 30m, cache
	// re-warmed so the warm guard cannot explain a skip away).
	f.idx.TouchCacheTouch("test/c1", f.fc.Now().Add(-10*time.Minute))
	f.fc.Advance(30 * time.Minute)
	f.r.maybeQuietCompaction(context.Background())
	waitIdle(t, f.r)
	if got := f.compacted["test/c1"]; got != 1 {
		t.Errorf("second check in the same window: compactions = %d, want 1", got)
	}
}

func TestMaybeQuietCompaction_NextNight_NeedsHumanInteraction(t *testing.T) {
	// Requirement 6d, second rule: the next night the session is NOT
	// compacted again until a human has interacted since the last stamp —
	// the ledger still reports the pre-compaction fill (it skips compaction
	// turns), so without this rule an untouched session would re-fire every
	// night. The cache is re-warmed before the blocked check so a cold cache
	// cannot explain the skip away; after a human touch it fires.
	at := time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC)
	f := newQuietFixture(t, quietOpts{at: at, window: "01:00-05:00"})

	f.r.maybeQuietCompaction(context.Background())
	waitIdle(t, f.r)
	if got := f.compacted["test/c1"]; got != 1 {
		t.Fatalf("first night: compactions = %d, want 1", got)
	}

	// Next night, same wall time: cache warm, fill unchanged, idle — but no
	// human interaction since the stamp. One compaction still.
	f.fc.Advance(24 * time.Hour)
	f.idx.TouchCacheTouch("test/c1", f.fc.Now().Add(-10*time.Minute))
	f.r.maybeQuietCompaction(context.Background())
	waitIdle(t, f.r)
	if got := f.compacted["test/c1"]; got != 1 {
		t.Fatalf("next night without human interaction: compactions = %d, want 1 (only the human-interaction rule may block)", got)
	}

	// A human interacts that evening (and the turn re-warms the cache); the
	// following 02:00 fires again.
	f.idx.TouchUserActivity("test/c1", at.Add(42*time.Hour)) // day 8, 20:00
	f.fc.Advance(24 * time.Hour)
	f.idx.TouchCacheTouch("test/c1", f.fc.Now().Add(-10*time.Minute))
	f.r.maybeQuietCompaction(context.Background())
	waitIdle(t, f.r)
	if got := f.compacted["test/c1"]; got != 2 {
		t.Errorf("night after a human interaction: compactions = %d, want 2", got)
	}
}

func TestMaybeQuietCompaction_Guards(t *testing.T) {
	// Requirements 6a, 6b (no recorded activity), 6c (unknown fill/limit),
	// 6e, 6f and 6g: every guard skips the session and none is stamped. The
	// eligible baseline — the same fixture with no guard — is proven by
	// TestMaybeQuietCompaction_FiresInsideWindowWhenIdle.
	cases := []struct {
		name    string
		opts    quietOpts
		prepare func(t *testing.T, f *quietFixture)
	}{
		{
			name: "no_compact set (6a)",
			opts: quietOpts{sessions: []quietSession{{
				key: "test/c1", idle: 41 * time.Minute, cacheAge: 10 * time.Minute,
				fill: 124000, limit: 200000, noCompact: true,
			}}},
		},
		{
			name: "no recorded user activity (6b)",
			opts: quietOpts{sessions: []quietSession{{
				key: "test/c1", idle: -1, cacheAge: 10 * time.Minute,
				fill: 124000, limit: 200000,
			}}},
		},
		{
			name: "unknown fill (6c)",
			opts: quietOpts{sessions: []quietSession{{
				key: "test/c1", idle: 41 * time.Minute, cacheAge: 10 * time.Minute,
				fill: 0, limit: 200000,
			}}},
		},
		{
			name: "unknown limit (6c)",
			opts: quietOpts{sessions: []quietSession{{
				key: "test/c1", idle: 41 * time.Minute, cacheAge: 10 * time.Minute,
				fill: 124000, limit: 0,
			}}},
		},
		{
			name:    "endpoint rate-limited (6e)",
			opts:    quietOpts{},
			prepare: func(t *testing.T, f *quietFixture) { f.fake.rateLimitedFn = func(string) (bool, string) { return true, "endpoint capped" } },
		},
		{
			name:    "turn in flight (6f)",
			opts:    quietOpts{},
			prepare: func(t *testing.T, f *quietFixture) { f.fake.quietBlockedFn = func(string) string { return "turn in flight" } },
		},
		{
			name: "no recorded cache touch (6g)",
			opts: quietOpts{sessions: []quietSession{{
				key: "test/c1", idle: 41 * time.Minute, cacheAge: -1,
				fill: 124000, limit: 200000,
			}}},
		},
		{
			name: "cache expired (6g)",
			opts: quietOpts{sessions: []quietSession{{
				key: "test/c1", idle: 41 * time.Minute, cacheAge: 2 * time.Hour, // > the 1h TTL
				fill: 124000, limit: 200000,
			}}},
		},
		{
			name:    "unknown TTL fails closed (6g)",
			opts:    quietOpts{},
			prepare: func(t *testing.T, f *quietFixture) { f.r.cacheTTL = 0 },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newQuietFixture(t, tc.opts)
			if tc.prepare != nil {
				tc.prepare(t, f)
			}
			f.r.maybeQuietCompaction(context.Background())
			waitIdle(t, f.r)
			if got := f.compacted["test/c1"]; got != 0 {
				t.Errorf("compactions = %d, want 0 (guard must skip the session)", got)
			}
			if _, ok := f.stamp("test/c1"); ok {
				t.Error("a guarded-off session was stamped")
			}
		})
	}
}

func TestMaybeQuietCompaction_AllOpenSessions(t *testing.T) {
	// Requirement 5: candidates are the default session plus EVERY open app
	// chat, deduplicated — independent of warm_open_app_chats (the runner's
	// keepalive openSessionsFn is nil here) — and only those above the
	// threshold are compacted.
	f := newQuietFixture(t, quietOpts{
		openChats: []string{"test/c3", "test/c2", "test/c1"}, // c1 again: dedup
		sessions: []quietSession{
			{key: "test/c1", idle: 41 * time.Minute, cacheAge: 10 * time.Minute, fill: 124000, limit: 200000}, // 62%: compact
			{key: "test/c2", idle: 41 * time.Minute, cacheAge: 10 * time.Minute, fill: 60000, limit: 200000},  // 30%: skip
			{key: "test/c3", idle: 41 * time.Minute, cacheAge: 10 * time.Minute, fill: 160000, limit: 200000}, // 80%: compact
		},
	})

	f.r.maybeQuietCompaction(context.Background())
	waitIdle(t, f.r)

	if got := f.compacted["test/c1"]; got != 1 {
		t.Errorf("c1 compactions = %d, want 1", got)
	}
	if got := f.compacted["test/c2"]; got != 0 {
		t.Errorf("c2 (below threshold) compactions = %d, want 0", got)
	}
	if got := f.compacted["test/c3"]; got != 1 {
		t.Errorf("c3 compactions = %d, want 1", got)
	}
	if len(f.notes) != 2 {
		t.Errorf("notes = %d, want 2 (c1 and c3)", len(f.notes))
	}
}

func TestMaybeQuietCompaction_OutcomeStamping(t *testing.T) {
	// Requirement 9: success, a backend decline (ErrCompactionNoBoundary)
	// and a failure are all stamped — a failed attempt is not retried for
	// the rest of the window. A refusal (guards changed between pick and
	// dispatch) is NOT an attempt: unstamped, so the next tick retries it.
	// Requirement 7: the targets are compacted one after another, in
	// candidate order. Requirement 10: only the success posts a note.
	f := newQuietFixture(t, quietOpts{
		sessions: []quietSession{
			{key: "test/c1", idle: 41 * time.Minute, cacheAge: 10 * time.Minute, fill: 124000, limit: 200000},
			{key: "test/c2", idle: 41 * time.Minute, cacheAge: 10 * time.Minute, fill: 124000, limit: 200000},
			{key: "test/c3", idle: 41 * time.Minute, cacheAge: 10 * time.Minute, fill: 124000, limit: 200000},
			{key: "test/c4", idle: 41 * time.Minute, cacheAge: 10 * time.Minute, fill: 124000, limit: 200000},
		},
		openChats: []string{"test/c2", "test/c3", "test/c4"},
		outcomes: map[string]error{
			"test/c2": delegator.ErrCompactionNoBoundary,
			"test/c3": errors.New("backend exploded"),
			"test/c4": fmt.Errorf("%w: turn in flight", ErrQuietCompactionRefused),
		},
	})

	f.r.maybeQuietCompaction(context.Background())
	waitIdle(t, f.r)

	wantOrder := []string{"test/c1", "test/c2", "test/c3", "test/c4"}
	if len(f.order) != len(wantOrder) {
		t.Fatalf("dispatch order = %v, want %v (one after another, in candidate order)", f.order, wantOrder)
	}
	for i := range wantOrder {
		if f.order[i] != wantOrder[i] {
			t.Fatalf("dispatch order = %v, want %v", f.order, wantOrder)
		}
	}
	for _, sk := range wantOrder {
		if got := f.compacted[sk]; got != 1 {
			t.Errorf("%s: compactions = %d, want 1", sk, got)
		}
	}
	for _, sk := range []string{"test/c1", "test/c2", "test/c3"} {
		if _, ok := f.stamp(sk); !ok {
			t.Errorf("%s: no stamp after an attempt that passed the guards", sk)
		}
	}
	if _, ok := f.stamp("test/c4"); ok {
		t.Error("refused session was stamped — a refusal is not an attempt")
	}
	if len(f.notes) != 1 || f.notes[0].sessionKey != "test/c1" {
		t.Errorf("notes = %+v, want exactly one, for the success", f.notes)
	}

	// Second tick in the same window (cache re-warmed so only the stamps
	// differ): stamped sessions are not retried; the refused one is — its
	// blocker may have cleared.
	f.idx.TouchCacheTouch("test/c1", f.fc.Now().Add(-10*time.Minute))
	f.idx.TouchCacheTouch("test/c2", f.fc.Now().Add(-10*time.Minute))
	f.idx.TouchCacheTouch("test/c3", f.fc.Now().Add(-10*time.Minute))
	f.idx.TouchCacheTouch("test/c4", f.fc.Now().Add(-10*time.Minute))
	f.fc.Advance(30 * time.Minute)
	f.r.maybeQuietCompaction(context.Background())
	waitIdle(t, f.r)
	for _, sk := range []string{"test/c1", "test/c2", "test/c3"} {
		if got := f.compacted[sk]; got != 1 {
			t.Errorf("%s: stamped attempt retried in the same window (%d calls), want 1", sk, got)
		}
	}
	if got := f.compacted["test/c4"]; got != 2 {
		t.Errorf("refused attempt not retried on the next tick (%d calls), want 2", got)
	}
}

func TestMaybeQuietCompaction_YieldsToOtherTasks(t *testing.T) {
	// Requirement 3: the quiet check yields while reflection, consolidation,
	// reset or keepalive is running, and never overlaps itself.
	cases := []struct {
		name string
		park func(r *Runner)
	}{
		{name: "reflection running", park: func(r *Runner) { r.reflectionRunning = true }},
		{name: "consolidation running", park: func(r *Runner) { r.consolidationRunning = true }},
		{name: "reset running", park: func(r *Runner) { r.resetRunning = true }},
		{name: "keepalive running", park: func(r *Runner) { r.keepaliveRunning = true }},
		{name: "quiet compaction already running", park: func(r *Runner) { r.quietCompactionRunning = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newQuietFixture(t, quietOpts{})
			tc.park(f.r)
			f.r.maybeQuietCompaction(context.Background())
			// Clear the parked flags so isIdle reflects real work only.
			f.r.mu.Lock()
			f.r.reflectionRunning, f.r.consolidationRunning, f.r.resetRunning = false, false, false
			f.r.keepaliveRunning, f.r.quietCompactionRunning = false, false
			f.r.mu.Unlock()
			if got := f.compacted["test/c1"]; got != 0 {
				t.Errorf("compactions = %d, want 0 while another task runs", got)
			}
			if !isIdle(f.r) {
				t.Error("quiet check set its running flag although it yielded")
			}
		})
	}
}

func TestMaybeKeepalive_YieldsToQuietCompaction(t *testing.T) {
	// The flags-level half of the mutual yield: keepalive is due but a
	// quiet compaction holds the flag — no branch. (The order half, which
	// makes the flag visible on the SAME tick, is pinned by
	// TestTick_KeepaliveYieldsToQuietCompaction below.)
	calls := 0
	r := dueRunner(t, &fakeBackgroundAgent{
		sessionKeyFn: func() string { return "test/c1" },
		branchFn: func(branchType, parentKey, promptText string, noCompact bool) bool {
			calls++
			return true
		},
	})
	r.mu.Lock()
	r.quietCompactionRunning = true
	r.mu.Unlock()

	r.maybeKeepalive(context.Background())
	r.mu.Lock()
	r.quietCompactionRunning = false
	r.mu.Unlock()
	waitIdle(t, r)

	if calls != 0 {
		t.Errorf("keepalive branched %d times while a quiet compaction was in flight, want 0", calls)
	}
}

func TestTick_KeepaliveYieldsToQuietCompaction(t *testing.T) {
	// The real run loop: quiet compaction parks mid-pass (its goroutine holds
	// quietCompactionRunning) while keepalive is due on every tick. Keepalive
	// must stay silent for as long as the compaction runs, then fire once the
	// flag clears — proving both the yield AND that the loop order makes the
	// flag reachable on the same tick (a flags-only test passes under a
	// broken order; see keepalive_yield_test.go for the pattern).
	//
	// Keepalive reads the REAL clock, so the cache touch is seeded relative
	// to the real now (touched 2s ago against a 1s interval and 1h TTL);
	// the quiet side reads the fake clock, whose window and idle rows are
	// seeded relative to it.
	quietStarted := make(chan struct{})
	release := make(chan struct{})
	// releaseOnce guards the double release: once mid-test (to let the
	// parked pass finish) and once in the cleanup defer.
	var releaseOnce sync.Once
	keepaliveFired := make(chan struct{}, 1)

	f := newQuietFixture(t, quietOpts{
		window: "01:00-05:00",
		onQuiet: func(sk string) {
			select {
			case <-quietStarted:
			default:
				close(quietStarted)
			}
			<-release // hold quietCompactionRunning for the assert window
		},
	})
	f.idx.TouchCacheTouch("test/c1", time.Now().Add(-2*time.Second)) // keepalive's clock
	f.r.kaCfg = config.ResolvedKeepalive{Enabled: true, Interval: "1s"}
	f.fake.branchFn = func(branchType, parentKey, promptText string, noCompact bool) bool {
		if branchType == "keepalive" {
			select {
			case keepaliveFired <- struct{}{}:
			default:
			}
		}
		return true
	}
	f.r.tickInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.r.Start(ctx)
	defer func() {
		releaseOnce.Do(func() { close(release) })
		f.r.Stop()
		waitIdle(t, f.r)
	}()

	select {
	case <-quietStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("quiet compaction never fired — the fixture is not due, so this test proves nothing")
	}

	// The quiet pass is parked inside QuietCompact, so quietCompactionRunning
	// stays true. Keepalive is due on every subsequent tick and must not fire
	// on any of them. Load-tolerant: heavier load means FEWER ticks in the
	// window, and the regression fires on the very first one.
	select {
	case <-keepaliveFired:
		t.Fatal("keepalive branched while a quiet compaction was in flight — it re-warmed the full context the compaction is about to shrink")
	case <-time.After(250 * time.Millisecond):
	}

	// Release the parked pass: once the flag clears, keepalive (still due)
	// must fire on a later tick — the yield is a yield, not a dead fixture.
	releaseOnce.Do(func() { close(release) })
	select {
	case <-keepaliveFired:
	case <-time.After(5 * time.Second):
		t.Fatal("keepalive never fired after the quiet compaction finished — the fixture is not due")
	}
}

func TestMaybeQuietCompaction_UpdateSettingsLive(t *testing.T) {
	// Requirement 2: a live settings change to the window, threshold or
	// idle minimum takes effect on the next check with no restart — the
	// ephemeral_retention_days precedent.
	at := time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC)

	// Baseline: the boot config fires at 02:00.
	f := newQuietFixture(t, quietOpts{at: at, window: "01:00-05:00"})
	f.r.maybeQuietCompaction(context.Background())
	waitIdle(t, f.r)
	if got := f.compacted["test/c1"]; got != 1 {
		t.Fatalf("baseline compactions = %d, want 1", got)
	}

	// Window flip: 03:00-05:00 excludes 02:00.
	g := newQuietFixture(t, quietOpts{at: at})
	g.r.applySettings(Settings{QuietCompaction: config.ResolvedQuietCompaction{
		Window: "03:00-05:00", Threshold: 0.5, MinIdle: "30m",
	}}, nil)
	g.r.maybeQuietCompaction(context.Background())
	waitIdle(t, g.r)
	if got := g.compacted["test/c1"]; got != 0 {
		t.Errorf("after a window change to 03:00-05:00, compactions at 02:00 = %d, want 0", got)
	}

	// Threshold flip: 0.9 excludes a 62% session.
	g = newQuietFixture(t, quietOpts{at: at})
	g.r.applySettings(Settings{QuietCompaction: config.ResolvedQuietCompaction{
		Window: "01:00-05:00", Threshold: 0.9, MinIdle: "30m",
	}}, nil)
	g.r.maybeQuietCompaction(context.Background())
	waitIdle(t, g.r)
	if got := g.compacted["test/c1"]; got != 0 {
		t.Errorf("after a threshold change to 0.9, compactions at 62%% = %d, want 0", got)
	}

	// Min-idle flip: 2h excludes a 41m-idle session.
	g = newQuietFixture(t, quietOpts{at: at})
	g.r.applySettings(Settings{QuietCompaction: config.ResolvedQuietCompaction{
		Window: "01:00-05:00", Threshold: 0.5, MinIdle: "2h",
	}}, nil)
	g.r.maybeQuietCompaction(context.Background())
	waitIdle(t, g.r)
	if got := g.compacted["test/c1"]; got != 0 {
		t.Errorf("after a min-idle change to 2h, compactions at 41m idle = %d, want 0", got)
	}
}
