package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/tools"
)

// [depth] enabled is live (#2603): with an app platform the depth tools are
// registered whatever the switch says, and a resolved-snapshot swap (what the
// liveApplyResolvedAddrs applier does on a config edit) hides or shows them in
// the model's tool list and refuses or allows a call, with no restart.
func TestDepthEnabledIsLive(t *testing.T) {
	t.Parallel()
	for _, path := range []toolPath{pathAPI, pathExec} {
		p := minimalSetupParams(t, "depth-live-"+map[toolPath]string{pathAPI: "api", pathExec: "exec"}[path])
		p.cfg.Platforms = []config.PlatformConfig{{ID: "app"}}
		p.acfg.Backend = "claude-code"
		off := *p.resolved
		off.Depth.Enabled = false
		p.resolvedLive = config.NewLiveValue(&off)

		registry := tools.NewRegistry()
		out := &toolOutputs{}
		registerTools(&toolDeps{
			p:        p,
			path:     path,
			registry: registry,
			connMgr:  stubConnMgr{},
			agLazy:   func() *agent.Agent { return nil },
			wakeFn:   stubWakeFn,
			out:      out,
		})
		fanout := registry.Get("depth_fanout")
		if fanout == nil || out.depth == nil {
			t.Fatalf("path %d: depth tools not registered while off (they must be, for the live switch)", path)
		}
		listed := func() bool {
			for _, d := range registry.ToolDefs() {
				if d.Name() == "depth_fanout" {
					return true
				}
			}
			return false
		}
		if listed() {
			t.Errorf("path %d: depth_fanout listed while [depth] enabled = false", path)
		}
		ctx := tools.WithSessionKey(context.Background(), p.acfg.ID+"/c1")
		raw := json.RawMessage(`{"leaves": [{"title": "A", "prompt": "p"}]}`)
		if _, err := fanout.Execute(ctx, raw); err == nil || !strings.Contains(err.Error(), "depth mode is off") {
			t.Errorf("path %d: fanout while off: %v", path, err)
		}

		on := off
		on.Depth.Enabled = true
		p.resolvedLive.Store(&on)
		if !listed() {
			t.Errorf("path %d: depth_fanout not listed once [depth] enabled = true", path)
		}
		if _, err := fanout.Execute(ctx, raw); err != nil && strings.Contains(err.Error(), "depth mode is off") {
			t.Errorf("path %d: fanout still refused once on: %v", path, err)
		}

		p.resolvedLive.Store(&off)
		if listed() {
			t.Errorf("path %d: depth_fanout still listed after turning it back off", path)
		}
	}
}
