package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/skills"
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

// TestToolTable_AskGraderFollowsSkillDirsAndAllowedPaths (#3083): the ask row
// passes the agent's live skill dirs and the #3076 file areas to the grader
// check. A grader path under this package's source dir is refused until the
// dir is a skill dir, or (separately) a live tools.file_allowed_paths entry.
// The path does not exist, so once it is inside an area the error turns from
// the area refusal into a stat error: the call never presents an ask.
func TestToolTable_AskGraderFollowsSkillDirsAndAllowedPaths(t *testing.T) {
	t.Parallel()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	grader := filepath.Join(cwd, "no-such-grader-3083")
	const refusal = "outside the directories a grader may run from"

	build := func(sd *skills.LiveDirs) (*tools.Tool, setupParams) {
		p := minimalSetupParams(t, "test")
		p.acfg.Workspace = t.TempDir()
		registry := tools.NewRegistry()
		registerTools(&toolDeps{
			p:          p,
			path:       pathAPI,
			registry:   registry,
			connMgr:    stubConnMgr{},
			agLazy:     func() *agent.Agent { return nil },
			wakeFn:     stubWakeFn,
			skillsDirs: sd,
			out:        &toolOutputs{},
		})
		ask := registry.Get("ask")
		if ask == nil {
			t.Fatal("ask not registered")
		}
		return ask, p
	}
	run := func(ask *tools.Tool) error {
		raw := `{"questions":[{"question":"q?","options":[{"label":"A"}]}],"grader":"` + grader + `"}`
		_, err := ask.Execute(tools.WithSessionKey(context.Background(), "test/grader-scope"), json.RawMessage(raw))
		return err
	}

	sd := skills.NewLiveDirs(skills.Dirs{})
	ask, p := build(sd)
	if err := run(ask); err == nil || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("grader outside every area: want refusal, got %v", err)
	}
	sd.Set(skills.Dirs{Extra: []string{cwd}}) // a live extra_dirs edit
	if err := run(ask); err == nil || strings.Contains(err.Error(), refusal) {
		t.Errorf("grader in a skill dir: want only the stat error, got %v", err)
	}

	ask, p = build(nil)
	fresh := *p.resolvedLive.Load()
	fresh.Tools.FileAllowedPaths = []string{cwd}
	p.resolvedLive.Store(&fresh)
	if err := run(ask); err == nil || strings.Contains(err.Error(), refusal) {
		t.Errorf("grader under a live file_allowed_paths entry: want only the stat error, got %v", err)
	}
}

// TestExploreReadTool_FollowsLiveConfineFlag (#3100): an API explore spawn's
// read reaches a path outside every area while tools.explore_confine_reads
// is off (the default), and is refused after a live edit turns it on.
func TestExploreReadTool_FollowsLiveConfineFlag(t *testing.T) {
	t.Parallel()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(cwd, "tool_table.go")
	p := minimalSetupParams(t, "test")
	p.acfg.Workspace = t.TempDir()
	read := exploreReadTool(&toolDeps{p: p})
	raw, _ := json.Marshal(map[string]any{"path": outside, "limit": 1})

	if _, err := read.Execute(context.Background(), raw); err != nil {
		t.Fatalf("default (off): outside read refused: %v", err)
	}
	fresh := *p.resolvedLive.Load()
	fresh.Tools.ExploreConfineReads = true
	p.resolvedLive.Store(&fresh)
	if _, err := read.Execute(context.Background(), raw); err == nil || !strings.Contains(err.Error(), tools.FileAllowedPathsKey) {
		t.Errorf("explore_confine_reads on: want refusal naming %s, got %v", tools.FileAllowedPathsKey, err)
	}
}
