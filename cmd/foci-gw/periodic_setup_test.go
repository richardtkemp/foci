package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/modelinfo"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// newQuietSetupInstance builds the minimal agentInstance setupPeriodic needs:
// an API-mode agent over a real session store and index, with its resolved
// snapshot installed. Each decorate hook (if any) mutates the agent before the
// instance is assembled — the quiet wiring test turns it into a delegated
// agent this way. The periodic runner it starts is stopped by the caller
// via inst.kaRunner.Stop().
func newQuietSetupInstance(t *testing.T, cfg *config.Config, acfg config.AgentConfig, decorate ...func(*agent.Agent)) *agentInstance {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	ag := &agent.Agent{
		Sessions:     session.NewStore(t.TempDir()),
		Tools:        tools.NewRegistry(),
		Bootstrap:    workspace.NewBootstrap(t.TempDir(), nil),
		SessionIndex: idx,
		Model:        "test-model",
	}
	for _, d := range decorate {
		d(ag)
	}
	return &agentInstance{
		id:       acfg.ID,
		ag:       ag,
		resolved: config.NewLiveValue(config.Resolve(cfg, acfg)),
	}
}

// TestPeriodicRederiveCarriesQuietCompaction proves the live config-apply
// path delivers all three quiet-compaction knobs to the runner (#2218):
// periodicRederive resolves them from a freshly loaded config — the global
// window and defaults first, then a per-agent override including the ""-window
// opt-out — into Settings.QuietCompaction, which applySettings installs.
func TestPeriodicRederiveCarriesQuietCompaction(t *testing.T) {
	base := &config.Config{Sessions: config.SessionsConfig{CompactionConfig: config.CompactionConfig{
		CompactionQuietHours: config.Ptr("23:00-07:00"),
	}}}
	acfg := config.AgentConfig{ID: "qa"}
	inst := newQuietSetupInstance(t, base, acfg)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	setupPeriodic(inst, acfg, periodicParams{
		cfg:          base,
		sessions:     inst.ag.Sessions,
		connMgr:      stubConnMgr{},
		sessionIndex: inst.ag.SessionIndex,
		ctx:          ctx,
	})
	t.Cleanup(func() { inst.kaRunner.Stop() })
	if inst.periodicRederive == nil {
		t.Fatal("setupPeriodic did not install periodicRederive")
	}

	// Fresh config with only the global window set: resolved defaults for
	// the other two knobs ride along.
	s := inst.periodicRederive(base, acfg)
	if s.QuietCompaction.Window != "23:00-07:00" {
		t.Errorf("Window = %q, want the global 23:00-07:00", s.QuietCompaction.Window)
	}
	if s.QuietCompaction.Threshold != 0.5 {
		t.Errorf("Threshold = %v, want the 0.5 default", s.QuietCompaction.Threshold)
	}
	if s.QuietCompaction.MinIdle != "30m" {
		t.Errorf("MinIdle = %q, want the 30m default", s.QuietCompaction.MinIdle)
	}

	// Fresh config changing every knob: a new global threshold and min idle,
	// and this agent opting out of the global window with an explicit "".
	fresh := &config.Config{Sessions: config.SessionsConfig{CompactionConfig: config.CompactionConfig{
		CompactionQuietHours:     config.Ptr("23:00-07:00"),
		CompactionQuietThreshold: config.Ptr(0.75),
		CompactionQuietMinIdle:   config.Ptr("10m"),
	}}}
	freshAcfg := config.AgentConfig{ID: acfg.ID, Sessions: config.AgentSessionsOverride{CompactionConfig: config.CompactionConfig{
		CompactionQuietHours: config.Ptr(""),
	}}}
	s = inst.periodicRederive(fresh, freshAcfg)
	want := config.ResolvedQuietCompaction{Window: "", Threshold: 0.75, MinIdle: "10m"}
	if s.QuietCompaction != want {
		t.Errorf("per-agent override resolved to %+v, want %+v (including the \"\" window opt-out)", s.QuietCompaction, want)
	}

	// The runner must exist for UpdateSettings to deliver any of this.
	if inst.kaRunner == nil {
		t.Fatal("setupPeriodic did not install the runner")
	}
}

// TestSetupPeriodicWiresQuietCompaction proves the BOOT wiring (#2218) end to
// end: the runner setupPeriodic builds carries the live resolved quiet config
// — the global window plus the resolved threshold and idle defaults — and
// fires the trigger on its own tick loop, before any config event rederives
// Settings (that live half is pinned by TestPeriodicRederiveCarriesQuietCompaction).
//
// The agent is a hand-built delegated instance: Spec.CacheTTL makes the
// cache-warm guard provable (an API agent has no TTL and always fails closed,
// requirement 6g), and NewBackend always fails, so the one attempt this test
// observes FAILS — which still records the attempt as quiet_compacted_at in
// the session index (requirement 9), the durable outcome every later guard
// reads. The window is computed around the real now (production reads the
// real clock) with an hour of slack either side; the tick is 10ms so the pass
// lands well inside the poll deadline.
func TestSetupPeriodicWiresQuietCompaction(t *testing.T) {
	now := time.Now()
	quietHours := now.Add(-time.Hour).Format("15:04") + "-" + now.Add(time.Hour).Format("15:04")
	const sk = "qa/c1"

	cfg := &config.Config{
		Scheduler: config.SchedulerConfig{TickInterval: config.Ptr("10ms")},
		Sessions: config.SessionsConfig{CompactionConfig: config.CompactionConfig{
			CompactionQuietHours: config.Ptr(quietHours),
		}},
	}
	acfg := config.AgentConfig{ID: "qa"}

	ledger, _, err := accounting.Open(filepath.Join(t.TempDir(), "api.db"), accounting.Options{})
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	accounting.SetLive(ledger)
	t.Cleanup(func() {
		accounting.SetLive(nil)
		_ = ledger.Close()
	})

	inst := newQuietSetupInstance(t, cfg, acfg, func(a *agent.Agent) {
		a.ModelMetaFn = func(string) modelinfo.ModelMeta { return modelinfo.ModelMeta{ContextWindow: 200000} }
		a.DelegatedManager = &agent.DelegatedManager{
			Spec:       delegator.Spec{CacheTTL: time.Hour},
			NewBackend: func() (delegator.Delegator, error) { return nil, errors.New("no backend in this test") },
		}
	})

	// Seed the one candidate on the instance's own index: a root chat
	// (so it IS the agent's default session), idle 41m against the 30m
	// minimum, cache touched 10m ago against the 1h TTL.
	idx := inst.ag.SessionIndex
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:     sk,
		FilePath:       "/tmp/test.jsonl",
		CreatedAt:      now.Add(-24 * time.Hour),
		LastActivityAt: now,
		SessionType:    session.SessionTypeChat,
		Status:         session.SessionStatusActive,
	})
	idx.TouchUserActivity(sk, now.Add(-41*time.Minute))
	idx.TouchCacheTouch(sk, now.Add(-10*time.Minute))

	// The fill: one booked user turn at 62% of the 200k window.
	turn := accounting.Turn{
		TurnID: sk + "@1", Session: sk, AgentID: "qa",
		Backend: accounting.BackendAPI, Source: accounting.SourceUser,
		StartedAt: now.Add(-41 * time.Minute), EndedAt: now.Add(-40 * time.Minute),
	}
	call := accounting.APIResponse{
		ID: "msg_quiet", Kind: accounting.KindCall, Provider: "anthropic", Model: "test-model",
		Session: sk, AgentID: "qa", TurnID: turn.TurnID,
		Start: turn.StartedAt, Duration: time.Second, StopReason: "end_turn",
		Tokens: modelinfo.Tokens{modelinfo.ClassInput: 124000, modelinfo.ClassOutput: 50},
	}.Call()
	if err := accounting.Record(turn, call); err != nil {
		t.Fatalf("book the fill call: %v", err)
	}

	// Fixture sanity, so a failure here names the seed and not the wiring.
	if got := defaultSessionKeyFor(inst.ag, "qa"); got != sk {
		t.Fatalf("default session = %q, want %q (fixture sanity)", got, sk)
	}
	if st, err := ledger.SessionStats(sk); err != nil || st.ContextTokens <= 0 {
		t.Fatalf("ledger fill for %s = %+v, %v (fixture sanity)", sk, st, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	setupPeriodic(inst, acfg, periodicParams{
		cfg:          cfg,
		sessions:     inst.ag.Sessions,
		connMgr:      stubConnMgr{},
		sessionIndex: idx,
		ctx:          ctx,
	})
	t.Cleanup(func() { inst.kaRunner.Stop() })

	// The observable: the boot-wired runner fires the quiet trigger through
	// its own loop and records the (failed) attempt. Missing wiring means a
	// zero window, so the stamp never appears and the deadline fails the test.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if v, err := idx.GetSessionMetadata(sk, session.MetaKeyQuietCompactedAt); err == nil && v != "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner built by setupPeriodic never quiet-compacted the seeded idle session — the boot wiring of the quiet-hours trigger is missing")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestBackgroundAgentContextUsageIgnoresPreCompactionFill (#2235): the quiet
// trigger's fill comes from backgroundAgent.ContextUsage. After a compaction
// the ledger's last fill is the PRE-compaction size (a compaction turn books
// no fill), so the adapter must report the fill as unknown (0) until a real
// turn books a fresh one; otherwise a session compacted by another trigger
// just before the quiet window would be compacted again.
func TestBackgroundAgentContextUsageIgnoresPreCompactionFill(t *testing.T) {
	const sk = "qa/c1"
	ledger, _, err := accounting.Open(filepath.Join(t.TempDir(), "api.db"), accounting.Options{})
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	accounting.SetLive(ledger)
	t.Cleanup(func() {
		accounting.SetLive(nil)
		_ = ledger.Close()
	})
	inst := newQuietSetupInstance(t, &config.Config{}, config.AgentConfig{ID: "qa"}, func(a *agent.Agent) {
		a.ModelMetaFn = func(string) modelinfo.ModelMeta { return modelinfo.ModelMeta{ContextWindow: 200000} }
	})
	ba := &backgroundAgent{inst: inst, agentID: "qa"}

	now := time.Now()
	book := func(id, source string, at time.Time, kind string, in int) {
		t.Helper()
		turn := accounting.Turn{TurnID: sk + "@" + id, Session: sk, AgentID: "qa",
			Backend: accounting.BackendAPI, Source: source, StartedAt: at, EndedAt: at.Add(time.Second)}
		call := accounting.APIResponse{ID: "msg_" + id, Kind: kind, Provider: "anthropic", Model: "test-model",
			Session: sk, AgentID: "qa", TurnID: turn.TurnID, Start: at, Duration: time.Second, StopReason: "end_turn",
			Tokens: modelinfo.Tokens{modelinfo.ClassInput: in, modelinfo.ClassOutput: 50}}.Call()
		if err := accounting.Record(turn, call); err != nil {
			t.Fatalf("book %s: %v", id, err)
		}
	}

	book("1", accounting.SourceUser, now.Add(-50*time.Minute), accounting.KindCall, 124000)
	if fill, limit := ba.ContextUsage(sk); fill != 124000 || limit != 200000 {
		t.Fatalf("before compaction: fill, limit = %d, %d; want 124000, 200000 (fixture sanity)", fill, limit)
	}
	book("2", accounting.SourceCompaction, now.Add(-45*time.Minute), accounting.KindCompaction, 124000)
	if fill, _ := ba.ContextUsage(sk); fill != 0 {
		t.Errorf("after a compaction: fill = %d, want 0 (unknown) — the last fill predates the compaction", fill)
	}
}

// TestBackgroundAgentContextUsageLimitIsAutoCompactionLimit: the quiet
// threshold is a fraction of the USABLE context — the limit at which foci
// auto-compacts (Agent.CompactionLimitTokens) — not the model's whole window
// (Dick, 2026-10-08). On a 1M window the default curve compacts near 48%,
// so a fraction of the raw window let 370-450K sessions sit below 50% all
// night. With no Compactor (auto-compaction off) the window is the limit.
func TestBackgroundAgentContextUsageLimitIsAutoCompactionLimit(t *testing.T) {
	const sk = "qa/c1"
	inst := newQuietSetupInstance(t, &config.Config{}, config.AgentConfig{ID: "qa"}, func(a *agent.Agent) {
		a.ModelMetaFn = func(string) modelinfo.ModelMeta { return modelinfo.ModelMeta{ContextWindow: 200000} }
		a.Compactor = compaction.NewCompactor(nil, 0.5)
	})
	ba := &backgroundAgent{inst: inst, agentID: "qa"}
	if _, limit := ba.ContextUsage(sk); limit != 100000 {
		t.Errorf("limit = %d, want 100000 (0.5 auto-compaction threshold of a 200000 window)", limit)
	}
	inst.ag.Compactor = nil
	if _, limit := ba.ContextUsage(sk); limit != 200000 {
		t.Errorf("no Compactor: limit = %d, want the 200000 window", limit)
	}
}
