package periodic

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/clock"
	"foci/internal/config"
	"foci/internal/session"
	"foci/internal/timeutil"
)

// Pre-cold compaction tests (#1066). Every verdict reads time through the
// runner's fake clock: the user-activity, cache-touch and stamp rows are
// seeded relative to the fake "now".

// precoldOpts configures newPrecoldFixture. The zero value is the eligible
// state: feature on (threshold 0.5, lead 10m), cache TTL 1h, keepalive OFF,
// and one session whose human last spoke 2h ago, whose cache was touched 55m
// ago (cold in 5m), at 62% of a 200k context — a pre-cold compaction must
// fire. Every field moves one condition away from that state.
type precoldOpts struct {
	disabled  bool          // the master switch; every other setting stays valid
	threshold float64       // 0 = 0.5
	lead      string        // "" = "10m"
	cacheTTL  time.Duration // 0 = 1h

	kaEnabled     bool
	kaInterval    string // "" = "55m"
	kaMaxUserIdle string // keepalive max_user_idle; "" = "96h"
	kaWarmChats   bool   // keepalive warm_open_app_chats
	openChats     []string

	userIdle time.Duration // < 0 = no recorded user activity; 0 = 2h
	cacheAge time.Duration // < 0 = no recorded touch; 0 = 55m
	fill     int           // 0 = 124000 (62% of 200k)

	blocked         string        // QuietCompactBlocked reason
	precoldStampAgo time.Duration // > 0 = an earlier pre-cold attempt this long ago
	quietStampAgo   time.Duration // > 0 = an earlier quiet attempt this long ago
}

type precoldFixture struct {
	r         *Runner
	idx       *session.SessionIndex
	fc        *clock.Fake
	compacted map[string]int
	notes     []quietNote
}

const precoldKey = "test/c1"

var precoldAt = time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)

func (o precoldOpts) resolved() config.ResolvedPrecoldCompaction {
	threshold := o.threshold
	if threshold == 0 {
		threshold = 0.5
	}
	lead := o.lead
	if lead == "" {
		lead = "10m"
	}
	return config.ResolvedPrecoldCompaction{Enabled: !o.disabled, Threshold: threshold, Lead: lead}
}

func newPrecoldFixture(t *testing.T, o precoldOpts) *precoldFixture {
	t.Helper()
	useUTC(t)
	at := precoldAt
	ttl := o.cacheTTL
	if ttl == 0 {
		ttl = time.Hour
	}
	if o.kaInterval == "" {
		o.kaInterval = "55m"
	}
	if o.kaMaxUserIdle == "" {
		o.kaMaxUserIdle = "96h"
	}
	if o.userIdle == 0 {
		o.userIdle = 2 * time.Hour
	}
	if o.cacheAge == 0 {
		o.cacheAge = 55 * time.Minute
	}
	if o.fill == 0 {
		o.fill = 124000
	}

	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })

	keys := append([]string{precoldKey}, o.openChats...)
	for _, sk := range keys {
		idx.Upsert(session.SessionIndexEntry{
			SessionKey:  sk,
			FilePath:    "/tmp/test.jsonl",
			CreatedAt:   at.Add(-24 * time.Hour),
			SessionType: session.SessionTypeChat,
			Status:      session.SessionStatusActive,
		})
		if o.userIdle > 0 {
			idx.TouchUserActivity(sk, at.Add(-o.userIdle))
		}
		if o.cacheAge > 0 {
			idx.TouchCacheTouch(sk, at.Add(-o.cacheAge))
		}
	}
	if o.precoldStampAgo > 0 {
		if err := idx.SetSessionMetadata(precoldKey, session.MetaKeyPrecoldCompactedAt, timeutil.FormatNano(at.Add(-o.precoldStampAgo))); err != nil {
			t.Fatal(err)
		}
	}
	if o.quietStampAgo > 0 {
		if err := idx.SetSessionMetadata(precoldKey, session.MetaKeyQuietCompactedAt, timeutil.FormatNano(at.Add(-o.quietStampAgo))); err != nil {
			t.Fatal(err)
		}
	}

	f := &precoldFixture{idx: idx, fc: fakeClockAt(at), compacted: make(map[string]int)}
	fake := &fakeBackgroundAgent{
		sessionKeyFn:   func() string { return precoldKey },
		contextUsageFn: func(string) (int, int) { return o.fill, 200000 },
		quietBlockedFn: func(string) string { return o.blocked },
		quietCompactFn: func(_ context.Context, sk string) error {
			f.compacted[sk]++
			return nil
		},
	}
	f.r = New(RunnerConfig{
		AgentID:      "test",
		SessionIndex: idx,
		Agent:        fake,
		CacheTTL:     ttl,
		Keepalive: config.ResolvedKeepalive{
			Enabled:          o.kaEnabled,
			Interval:         o.kaInterval,
			MaxUserIdle:      o.kaMaxUserIdle,
			WarmOpenAppChats: o.kaWarmChats,
		},
		// Quiet compaction off, so a tick test sees only the pre-cold trigger.
		QuietCompaction:    config.ResolvedQuietCompaction{Window: ""},
		PrecoldCompaction:  o.resolved(),
		Clock:              f.fc,
		OpenChatSessionsFn: func() []string { return o.openChats },
		NotifyQuietCompact: func(sk, text string) {
			f.notes = append(f.notes, quietNote{sessionKey: sk, text: text})
		},
	})
	return f
}

func (f *precoldFixture) run(t *testing.T) {
	t.Helper()
	f.r.maybePrecoldCompaction(context.Background())
	waitIdle(t, f.r)
}

func TestMaybePrecoldCompaction_Table(t *testing.T) {
	cases := []struct {
		name string
		o    precoldOpts
		want bool
	}{
		{"eligible: keepalive off, cache cold in 5m", precoldOpts{}, true},

		// a. The master switch (F1): a realistic disabled config — every
		// other setting valid and the session fully eligible — must not fire.
		{"disabled, keepalive off", precoldOpts{disabled: true}, false},
		{"disabled, keepalive limited and lapsed", precoldOpts{disabled: true, kaEnabled: true, kaMaxUserIdle: "1h"}, false},

		// c. Keepalive.
		{"keepalive on, max_user_idle 96h: will re-warm", precoldOpts{kaEnabled: true}, false},
		{"keepalive on, max_user_idle 0: will re-warm", precoldOpts{kaEnabled: true, kaMaxUserIdle: "0"}, false},
		{"keepalive on, max_user_idle 1h lapsed (user idle 2h)", precoldOpts{kaEnabled: true, kaMaxUserIdle: "1h"}, true},
		// Due in 5m (touch 50m, interval 55m): user idle 85m+5m = 90m <= 90m, keepalive still warms.
		{"keepalive gate open at its next warm", precoldOpts{kaEnabled: true, kaMaxUserIdle: "90m", userIdle: 85 * time.Minute, cacheAge: 50 * time.Minute}, false},
		// 86m+5m = 91m > 90m: keepalive stops before it warms again.
		{"keepalive gate closed at its next warm", precoldOpts{kaEnabled: true, kaMaxUserIdle: "90m", userIdle: 86 * time.Minute, cacheAge: 50 * time.Minute}, true},
		// interval >= TTL: keepalive's warm window is empty, it never warms.
		{"keepalive on, interval >= TTL", precoldOpts{kaEnabled: true, kaInterval: "61m"}, true},
		// warm_open_app_chats on with a chat open: keepalive warms only the chats, not the default session.
		{"keepalive warms only open chats", precoldOpts{kaEnabled: true, kaWarmChats: true, openChats: []string{"test/other"}, kaMaxUserIdle: "0"}, true},

		// b. Time position relative to expiry.
		{"cache cold in 20m (> lead)", precoldOpts{cacheAge: 40 * time.Minute}, false},
		{"cache cold in exactly the lead", precoldOpts{cacheAge: 50 * time.Minute}, true},
		{"cache already cold", precoldOpts{cacheAge: 61 * time.Minute}, false},
		{"cache cold exactly now", precoldOpts{cacheAge: 60 * time.Minute}, false},
		{"no recorded cache touch", precoldOpts{cacheAge: -1}, false},
		// 5m TTL: the 10m lead is capped at 2m30s.
		{"short TTL: lead capped, too early", precoldOpts{cacheTTL: 5 * time.Minute, cacheAge: 1 * time.Minute, userIdle: time.Minute}, false},
		{"short TTL: inside the capped lead", precoldOpts{cacheTTL: 5 * time.Minute, cacheAge: 3 * time.Minute, userIdle: 3 * time.Minute}, true},

		// d. Usage vs threshold.
		{"below threshold", precoldOpts{fill: 98000}, false},
		{"exactly at threshold", precoldOpts{fill: 100000}, true},
		{"fill unknown (compacted since)", precoldOpts{fill: -1}, false},

		// e. Safety conditions.
		{"mid-turn", precoldOpts{blocked: "turn in flight"}, false},
		{"queued input", precoldOpts{blocked: "inbox input queued"}, false},
		{"background work", precoldOpts{blocked: "1 subagent"}, false},
		{"no recorded user activity", precoldOpts{userIdle: -1}, false},

		// Once per idle period.
		{"already pre-cold-compacted this idle period", precoldOpts{precoldStampAgo: time.Hour}, false},
		{"pre-cold-compacted before the last human message", precoldOpts{precoldStampAgo: 3 * time.Hour}, true},
		// 5. Quiet hours already compacted it this idle period.
		{"quiet-compacted this idle period", precoldOpts{quietStampAgo: time.Hour}, false},
		{"quiet-compacted before the last human message", precoldOpts{quietStampAgo: 3 * time.Hour}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrecoldFixture(t, tc.o)
			f.run(t)
			got := f.compacted[precoldKey] == 1
			if got != tc.want {
				t.Fatalf("compacted = %v (calls %d), want %v", got, f.compacted[precoldKey], tc.want)
			}
			if !tc.want {
				if len(f.notes) != 0 {
					t.Errorf("notes = %v, want none", f.notes)
				}
				return
			}
			if len(f.notes) != 1 || f.notes[0].sessionKey != precoldKey {
				t.Fatalf("notes = %+v, want one note to %s", f.notes, precoldKey)
			}
			if !strings.Contains(f.notes[0].text, "Pre-cold compaction") {
				t.Errorf("note %q does not name the pre-cold trigger", f.notes[0].text)
			}
		})
	}
}

func TestMaybePrecoldCompaction_DisabledMakesNoLookups(t *testing.T) {
	// The off path is an exact no-op, with every other setting valid.
	useUTC(t)
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	r := New(RunnerConfig{
		AgentID:           "test",
		SessionIndex:      idx,
		Agent:             tripwireAgent(t),
		CacheTTL:          time.Hour,
		PrecoldCompaction: config.ResolvedPrecoldCompaction{Enabled: false, Threshold: 0.5, Lead: "10m"},
		Clock:             fakeClockAt(precoldAt),
		OpenChatSessionsFn: func() []string {
			t.Error("pre-cold compaction listed open chats although it is disabled")
			return nil
		},
	})
	r.maybePrecoldCompaction(context.Background())
}

func TestMaybePrecoldCompaction_NoteText(t *testing.T) {
	f := newPrecoldFixture(t, precoldOpts{})
	f.run(t)
	if len(f.notes) != 1 {
		t.Fatalf("notes = %d, want 1", len(f.notes))
	}
	for _, want := range []string{"Pre-cold compaction", "5m", "62%", "50%"} {
		if !strings.Contains(f.notes[0].text, want) {
			t.Errorf("note %q missing %q", f.notes[0].text, want)
		}
	}
	if _, ok := f.r.metaStamp(precoldKey, session.MetaKeyPrecoldCompactedAt); !ok {
		t.Error("no precold_compacted_at stamp after a compaction")
	}
}

func TestMaybePrecoldCompaction_OncePerIdlePeriod(t *testing.T) {
	f := newPrecoldFixture(t, precoldOpts{})
	f.run(t)
	if got := f.compacted[precoldKey]; got != 1 {
		t.Fatalf("first pass: compactions = %d, want 1", got)
	}

	// An hour later the compacted session's cache is again about to go cold
	// and the (stale) fill still reads above the threshold. No human spoke:
	// no second compaction.
	f.fc.Advance(time.Hour)
	f.idx.TouchCacheTouch(precoldKey, f.r.now().Add(-55*time.Minute))
	f.run(t)
	if got := f.compacted[precoldKey]; got != 1 {
		t.Fatalf("second idle check without a human interaction: compactions = %d, want 1", got)
	}

	// A human interaction re-arms it: the next idle period may compact again.
	f.idx.TouchUserActivity(precoldKey, f.r.now().Add(-56*time.Minute))
	f.run(t)
	if got := f.compacted[precoldKey]; got != 2 {
		t.Fatalf("after a human interaction: compactions = %d, want 2", got)
	}
}

func TestMaybePrecoldCompaction_UpdateSettingsLive(t *testing.T) {
	// Off at boot, switched on live: fires on the next check, no restart.
	f := newPrecoldFixture(t, precoldOpts{disabled: true})
	f.run(t)
	if got := f.compacted[precoldKey]; got != 0 {
		t.Fatalf("disabled at boot: compactions = %d, want 0", got)
	}
	f.r.applySettings(Settings{PrecoldCompaction: config.ResolvedPrecoldCompaction{Enabled: true, Threshold: 0.5, Lead: "10m"}}, nil)
	f.run(t)
	if got := f.compacted[precoldKey]; got != 1 {
		t.Fatalf("enabled live: compactions = %d, want 1", got)
	}

	// Threshold and lead changes apply live as well.
	g := newPrecoldFixture(t, precoldOpts{})
	g.r.applySettings(Settings{PrecoldCompaction: config.ResolvedPrecoldCompaction{Enabled: true, Threshold: 0.9, Lead: "10m"}}, nil)
	g.run(t)
	if got := g.compacted[precoldKey]; got != 0 {
		t.Errorf("threshold 0.9 at 62%%: compactions = %d, want 0", got)
	}
	g = newPrecoldFixture(t, precoldOpts{})
	g.r.applySettings(Settings{PrecoldCompaction: config.ResolvedPrecoldCompaction{Enabled: true, Threshold: 0.5, Lead: "2m"}}, nil)
	g.run(t)
	if got := g.compacted[precoldKey]; got != 0 {
		t.Errorf("lead 2m with expiry in 5m: compactions = %d, want 0", got)
	}
}

func TestMaybePrecoldCompaction_YieldsToOtherTasks(t *testing.T) {
	for _, flag := range []string{"quiet", "keepalive", "reflection", "consolidation", "reset"} {
		t.Run(flag, func(t *testing.T) {
			f := newPrecoldFixture(t, precoldOpts{})
			f.r.mu.Lock()
			switch flag {
			case "quiet":
				f.r.quietCompactionRunning = true
			case "keepalive":
				f.r.keepaliveRunning = map[string]bool{precoldKey: true}
			case "reflection":
				f.r.reflectionRunning = true
			case "consolidation":
				f.r.consolidationRunning = true
			case "reset":
				f.r.resetRunning = true
			}
			f.r.mu.Unlock()
			f.r.maybePrecoldCompaction(context.Background())
			if got := f.compacted[precoldKey]; got != 0 {
				t.Fatalf("compactions while %s runs = %d, want 0", flag, got)
			}
		})
	}
}

func TestMaybeKeepaliveAndQuiet_YieldToPrecold(t *testing.T) {
	calls := 0
	r := dueRunner(t, &fakeBackgroundAgent{
		sessionKeyFn: func() string { return "test/c1" },
		branchFn: func(string, string, string, bool) bool {
			calls++
			return true
		},
	})
	r.precoldCompactionRunning = true
	r.maybeKeepalive(context.Background())
	waitIdleExceptPrecold(t, r)
	if calls != 0 {
		t.Fatalf("keepalive fired %d time(s) during a pre-cold compaction, want 0", calls)
	}

	q := newQuietFixture(t, quietOpts{})
	q.r.precoldCompactionRunning = true
	q.r.maybeQuietCompaction(context.Background())
	q.r.mu.Lock()
	quietStarted := q.r.quietCompactionRunning
	q.r.mu.Unlock()
	if quietStarted || q.compacted["test/c1"] != 0 {
		t.Fatal("quiet compaction started during a pre-cold compaction")
	}
}

// waitIdleExceptPrecold waits for every scheduler goroutine except a
// pre-cold pass the test latched by hand.
func waitIdleExceptPrecold(t *testing.T, r *Runner) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		busy := len(r.keepaliveRunning) > 0 || r.quietCompactionRunning
		r.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduler goroutine did not finish within 2s")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestKeepaliveWillRewarm_AgreesWithKeepalive replays keepalive's own target
// selection at every minute from now until the cache expires and checks that
// keepaliveWillRewarm predicted it: requirement 2c's "the two must not drift
// apart", tested against the code that warms, not against a restatement.
func TestKeepaliveWillRewarm_AgreesWithKeepalive(t *testing.T) {
	type kaCase struct {
		enabled   bool
		interval  string
		maxIdle   string
		warmChats bool
		openChats []string
	}
	kas := []kaCase{
		{enabled: false, interval: "55m", maxIdle: "96h"},
		{enabled: true, interval: "55m", maxIdle: "96h"},
		{enabled: true, interval: "55m", maxIdle: "0"},
		{enabled: true, interval: "55m", maxIdle: "1h"},
		{enabled: true, interval: "55m", maxIdle: "90m"},
		{enabled: true, interval: "30m", maxIdle: "70m"},
		{enabled: true, interval: "61m", maxIdle: "0"},
		{enabled: true, interval: "55m", maxIdle: "0", warmChats: true, openChats: []string{"test/other"}},
		{enabled: true, interval: "55m", maxIdle: "0", warmChats: true, openChats: []string{"test/c1"}},
	}
	userIdles := []time.Duration{-1, 10 * time.Minute, 50 * time.Minute, 85 * time.Minute, 86 * time.Minute, 2 * time.Hour, 100 * time.Hour}
	cacheAges := []time.Duration{-1, 5 * time.Minute, 29 * time.Minute, 30 * time.Minute, 50 * time.Minute, 55 * time.Minute, 59 * time.Minute, 61 * time.Minute}

	n := 0
	for _, ka := range kas {
		for _, ui := range userIdles {
			for _, ca := range cacheAges {
				name := fmt.Sprintf("ka=%v/%s/%s/chats=%v%v ui=%s age=%s", ka.enabled, ka.interval, ka.maxIdle, ka.warmChats, ka.openChats, ui, ca)
				f := newPrecoldFixture(t, precoldOpts{
					kaEnabled: ka.enabled, kaInterval: ka.interval, kaMaxUserIdle: ka.maxIdle,
					kaWarmChats: ka.warmChats, openChats: ka.openChats,
					userIdle: ui, cacheAge: ca,
				})
				now := f.r.now()
				predicted := f.r.keepaliveWillRewarm(precoldKey, now)

				actual := false
				if ka.enabled {
					interval, _ := time.ParseDuration(ka.interval)
					ttl := f.r.ttlFor(precoldKey)
					// Every case's decisive instants fall on whole minutes.
					for step := time.Duration(0); step < ttl; step += time.Minute {
						ready, _ := f.r.keepaliveTargetsAt(interval, now.Add(step))
						for _, sk := range ready {
							if sk == precoldKey {
								actual = true
							}
						}
						if actual {
							break
						}
					}
				}
				if predicted != actual {
					t.Errorf("%s: keepaliveWillRewarm = %v, keepalive actually warms before expiry = %v", name, predicted, actual)
				}
				n++
			}
		}
	}
	t.Logf("checked %d states", n)
}
