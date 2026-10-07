package main

import (
	"context"
	"path/filepath"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// newQuietSetupInstance builds the minimal agentInstance setupPeriodic needs:
// an API-mode agent over a real session store and index, with its resolved
// snapshot installed. The periodic runner it starts is stopped by the caller
// via inst.kaRunner.Stop().
func newQuietSetupInstance(t *testing.T, cfg *config.Config, acfg config.AgentConfig) *agentInstance {
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
