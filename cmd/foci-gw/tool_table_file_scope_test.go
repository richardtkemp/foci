package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/tools"
)

// TestToolTable_FileToolsFollowLiveAllowedPaths (#3076): the file tools the
// table builds are confined to the agent's workspace and the temp areas, and
// a live tools.file_allowed_paths edit (a new resolved snapshot, as the
// live-apply path stores it) applies on the next call with no rebuild.
//
// The "outside" file is this package's own source file: every writable test
// dir lies under the temp areas, which are always allowed.
func TestToolTable_FileToolsFollowLiveAllowedPaths(t *testing.T) {
	t.Parallel()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(cwd, "tool_table.go")
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	p := minimalSetupParams(t, "test")
	p.acfg.Workspace = t.TempDir()
	registry := tools.NewRegistry()
	registerTools(&toolDeps{
		p:        p,
		path:     pathAPI,
		registry: registry,
		connMgr:  stubConnMgr{},
		agLazy:   func() *agent.Agent { return nil },
		wakeFn:   stubWakeFn,
		out:      &toolOutputs{},
	})

	run := func(tool, key string) error {
		tl := registry.Get(tool)
		if tl == nil {
			t.Fatalf("%s not registered", tool)
		}
		raw, _ := json.Marshal(map[string]any{key: outside, "prompt": "x", "url": "http://127.0.0.1:1/", "method": "POST"})
		_, err := tl.Execute(context.Background(), raw)
		return err
	}
	cases := []struct{ tool, key string }{{"read", "path"}, {"http_request", "body_file"}} // summary: covered in internal/tools; its nil summariser here would panic once allowed

	for _, c := range cases {
		if err := run(c.tool, c.key); err == nil || !strings.Contains(err.Error(), tools.FileAllowedPathsKey) {
			t.Errorf("%s: outside path: want refusal naming %s, got %v", c.tool, tools.FileAllowedPathsKey, err)
		}
	}

	fresh := *p.resolvedLive.Load()
	fresh.Tools.FileAllowedPaths = []string{cwd}
	p.resolvedLive.Store(&fresh)

	for _, c := range cases {
		if err := run(c.tool, c.key); err != nil && strings.Contains(err.Error(), tools.FileAllowedPathsKey) {
			t.Errorf("%s: path under a live-added file_allowed_paths entry still refused: %v", c.tool, err)
		}
	}
}
