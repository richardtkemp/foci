package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/config"
)

func keepaliveRow(t *testing.T, data PromptsData) PromptInfo {
	t.Helper()
	for _, p := range data.Prompts {
		if p.Label == "keepalive" {
			return p
		}
	}
	t.Fatal("no keepalive row")
	return PromptInfo{}
}

func writeExec(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho ping\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func keepaliveCC(t *testing.T, global, agent *string, dirs ...string) CommandContext {
	t.Helper()
	cfg := &config.Config{}
	cfg.Keepalive.Prompt = global
	acfg := config.AgentConfig{ID: "a", Workspace: t.TempDir()}
	acfg.Keepalive.Prompt = agent
	return CommandContext{Config: cfg, AgentConfig: acfg, PromptSearchDirs: dirs}
}

// TestPromptsKeepaliveRowUsesMergedConfig (#1351 F1): /prompts shows the
// keepalive prompt the runtime uses — agent [keepalive] over global.
func TestPromptsKeepaliveRowUsesMergedConfig(t *testing.T) {
	dir := t.TempDir()
	globalScript := filepath.Join(dir, "global-ka.sh")
	writeExec(t, globalScript)
	agentMD := filepath.Join(dir, "agent-ka.md")
	if err := os.WriteFile(agentMD, []byte("agent prompt"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("global-only script", func(t *testing.T) {
		data := buildPromptsData(keepaliveCC(t, &globalScript, nil))
		row := keepaliveRow(t, data)
		if !row.Script || row.Path != globalScript {
			t.Errorf("row = %+v, want script %s", row, globalScript)
		}
		text := promptsDisplay(data)
		if !strings.Contains(text, globalScript+" [script]") && !strings.Contains(text, relPath(globalScript)+" [script]") {
			t.Errorf("display lacks the script path marked script:\n%s", text)
		}
	})
	t.Run("agent override beats global", func(t *testing.T) {
		row := keepaliveRow(t, buildPromptsData(keepaliveCC(t, &globalScript, &agentMD)))
		if row.Script || row.Path != agentMD {
			t.Errorf("row = %+v, want agent md %s", row, agentMD)
		}
	})
	t.Run("neither shows default", func(t *testing.T) {
		data := buildPromptsData(keepaliveCC(t, nil, nil))
		row := keepaliveRow(t, data)
		if row.Script || row.Path != "" || !row.Default {
			t.Errorf("row = %+v, want default", row)
		}
		if !strings.Contains(promptsDisplay(data), "[default]") {
			t.Error("display lacks [default]")
		}
	})
}

// TestPromptsKeepaliveScriptInSearchDir: a runnable keepalive.sh in a prompt
// dir shows as a script, and /prompts diff does not read it as a file.
func TestPromptsKeepaliveScriptInSearchDir(t *testing.T) {
	dir := t.TempDir()
	sp := filepath.Join(dir, "keepalive.sh")
	writeExec(t, sp)
	if err := os.WriteFile(filepath.Join(dir, "keepalive.md"), []byte("md"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := buildPromptsData(keepaliveCC(t, nil, nil, dir))
	row := keepaliveRow(t, data)
	if !row.Script || row.Path != sp {
		t.Errorf("row = %+v, want script %s", row, sp)
	}
	if got := data.ResolvedTexts["keepalive"]; !strings.Contains(got, "script") || strings.Contains(got, "echo ping") {
		t.Errorf("resolved text = %q, want a script note, not the script's source", got)
	}
}

// TestPromptsDisplayScriptRow: the rendered row names the path and says script.
func TestPromptsDisplayScriptRow(t *testing.T) {
	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{
		AgentID: "a",
		Prompts: []PromptInfo{{Label: "keepalive", Path: "/x/prompts/keepalive.sh", Filename: "keepalive.md", Exists: true, Script: true}},
	})
	res, err := cmd.Execute(context.Background(), Request{Args: "list"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "/x/prompts/keepalive.sh [script]") {
		t.Errorf("missing script row in:\n%s", res.Text)
	}
}
