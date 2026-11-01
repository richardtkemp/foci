package main

import (
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/tools"
)

// [tools] max_concurrent_spawns is live (#2722): a resolved-snapshot swap
// (what the liveApplyResolvedAddrs applier does on a config edit) resizes the
// agent's Spawner slots, on both tool paths, with no restart.
func TestMaxConcurrentSpawnsIsLive(t *testing.T) {
	t.Parallel()
	for _, path := range []toolPath{pathAPI, pathExec} {
		p := minimalSetupParams(t, "spawn-limit-live-"+map[toolPath]string{pathAPI: "api", pathExec: "exec"}[path])
		if path == pathExec {
			p.acfg.Backend = "claude-code"
		}
		start := *p.resolved
		start.Tools.MaxConcurrentSpawns = 2
		p.resolved = &start
		p.resolvedLive = config.NewLiveValue(&start)

		out := &toolOutputs{}
		registerTools(&toolDeps{
			p:        p,
			path:     path,
			registry: tools.NewRegistry(),
			connMgr:  stubConnMgr{},
			agLazy:   func() *agent.Agent { return nil },
			wakeFn:   stubWakeFn,
			out:      out,
		})
		if out.spawner == nil {
			t.Fatalf("path %d: no spawner registered", path)
		}
		if got := out.spawner.MaxConcurrent(); got != 2 {
			t.Fatalf("path %d: initial limit %d, want 2", path, got)
		}
		raised := start
		raised.Tools.MaxConcurrentSpawns = 10
		p.resolvedLive.Store(&raised)
		if got := out.spawner.MaxConcurrent(); got != 10 {
			t.Errorf("path %d: limit %d after a live raise, want 10", path, got)
		}
		lowered := start
		lowered.Tools.MaxConcurrentSpawns = 1
		p.resolvedLive.Store(&lowered)
		if got := out.spawner.MaxConcurrent(); got != 1 {
			t.Errorf("path %d: limit %d after a live lower, want 1", path, got)
		}
	}
}
