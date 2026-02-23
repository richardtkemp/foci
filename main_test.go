package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clod/agent"
	"clod/config"
	"clod/memory"
	"clod/session"
)

func TestGracefulShutdown_AllIdle(t *testing.T) {
	agents := map[string]*agentInstance{
		"a": {id: "a", ag: &agent.Agent{}},
		"b": {id: "b", ag: &agent.Agent{}},
	}
	start := time.Now()
	gracefulShutdown(agents, 5*time.Second)
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("shutdown took %v, expected near-instant when all idle", elapsed)
	}
}

func TestGracefulShutdown_WaitsForProcessing(t *testing.T) {
	ag := &agent.Agent{}
	ag.SetProcessingForTest(1)

	agents := map[string]*agentInstance{
		"a": {id: "a", ag: ag},
	}

	// Complete the "turn" after 300ms
	go func() {
		time.Sleep(300 * time.Millisecond)
		ag.SetProcessingForTest(0)
	}()

	start := time.Now()
	gracefulShutdown(agents, 5*time.Second)
	elapsed := time.Since(start)

	if elapsed < 250*time.Millisecond {
		t.Errorf("shutdown returned too early (%v), should wait for processing", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("shutdown took too long (%v), should complete soon after agent finishes", elapsed)
	}
}

func TestGracefulShutdown_TimesOut(t *testing.T) {
	ag := &agent.Agent{}
	ag.SetProcessingForTest(1) // never cleared — simulates stuck agent

	agents := map[string]*agentInstance{
		"a": {id: "a", ag: ag},
	}

	start := time.Now()
	gracefulShutdown(agents, 500*time.Millisecond)
	elapsed := time.Since(start)

	if elapsed < 400*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("shutdown took %v, expected ~500ms timeout", elapsed)
	}

	ag.SetProcessingForTest(0)
}

func TestProcessingCounter(t *testing.T) {
	ag := &agent.Agent{}
	if ag.IsProcessing() {
		t.Fatal("new agent should not be processing")
	}

	ag.SetProcessingForTest(1)
	if !ag.IsProcessing() {
		t.Fatal("agent should be processing after SetProcessingForTest(1)")
	}

	ag.SetProcessingForTest(0)
	if ag.IsProcessing() {
		t.Fatal("agent should not be processing after SetProcessingForTest(0)")
	}
}

func TestProcessingCounter_Multiple(t *testing.T) {
	ag := &agent.Agent{}
	ag.SetProcessingForTest(2)
	if !ag.IsProcessing() {
		t.Fatal("should be processing with count 2")
	}

	ag.SetProcessingForTest(1)
	if !ag.IsProcessing() {
		t.Fatal("should still be processing with count 1")
	}

	ag.SetProcessingForTest(0)
	if ag.IsProcessing() {
		t.Fatal("should not be processing with count 0")
	}
}

func TestInjectWelcomeFile(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "sessions")
	os.MkdirAll(sessDir, 0755)
	sessions := session.NewStore(sessDir)

	welcomePath := filepath.Join(dir, "WELCOME.md")
	os.WriteFile(welcomePath, []byte("# Updated\n\nNew stuff here."), 0644)

	agents := map[string]*agentInstance{
		"main": {id: "main", sessionKey: "agent:main:main"},
	}
	agentOrder := []string{"main"}

	content := injectWelcomeFile(welcomePath, agents, agentOrder, sessions)

	// File should be deleted
	if _, err := os.Stat(welcomePath); !os.IsNotExist(err) {
		t.Error("welcome file should be deleted after injection")
	}

	// Should return the changelog content
	if content == "" {
		t.Fatal("expected non-empty content from welcome file")
	}
	if !strings.Contains(content, "New stuff here") {
		t.Errorf("content should contain file text, got %q", content)
	}
}

func TestInjectWelcomeFile_NoFile(t *testing.T) {
	agents := map[string]*agentInstance{
		"main": {id: "main", sessionKey: "agent:main:main"},
	}
	agentOrder := []string{"main"}

	content := injectWelcomeFile("/nonexistent/path/WELCOME.md", agents, agentOrder, nil)
	if content != "" {
		t.Errorf("expected empty content when no file, got %q", content)
	}
}

func TestInjectWelcomeFile_EmptyFile(t *testing.T) {
	dir := t.TempDir()

	welcomePath := filepath.Join(dir, "WELCOME.md")
	os.WriteFile(welcomePath, []byte(""), 0644)

	agents := map[string]*agentInstance{
		"main": {id: "main", sessionKey: "agent:main:main"},
	}
	agentOrder := []string{"main"}

	content := injectWelcomeFile(welcomePath, agents, agentOrder, nil)

	// File should be deleted even if empty
	if _, err := os.Stat(welcomePath); !os.IsNotExist(err) {
		t.Error("empty welcome file should be deleted")
	}

	// Should return empty content
	if content != "" {
		t.Errorf("expected empty content for empty file, got %q", content)
	}
}

func TestInjectWelcomeFile_EmptyPath(t *testing.T) {
	// Should not panic when path is empty
	content := injectWelcomeFile("", nil, nil, nil)
	if content != "" {
		t.Errorf("expected empty content for empty path, got %q", content)
	}
}

func TestInjectWelcomeFile_TriggersTurnOnlyWithContent(t *testing.T) {
	dir := t.TempDir()

	agents := map[string]*agentInstance{
		"main": {id: "main", sessionKey: "agent:main:main"},
	}
	agentOrder := []string{"main"}

	// With content: should trigger a restart turn (non-empty return)
	withPath := filepath.Join(dir, "WITH.md")
	os.WriteFile(withPath, []byte("changelog text"), 0644)
	content := injectWelcomeFile(withPath, agents, agentOrder, nil)
	if content == "" {
		t.Error("SYSTEM UPDATE present: expected non-empty content (should trigger restart turn)")
	}

	// Without content (bare restart): should NOT trigger a turn (empty return)
	noPath := filepath.Join(dir, "NONE.md")
	content = injectWelcomeFile(noPath, agents, agentOrder, nil)
	if content != "" {
		t.Error("bare restart (no file): expected empty content (should NOT trigger turn)")
	}
}

// ========== Per-agent memory tests ==========

func TestBuildAgentMemorySources_GlobalOnly(t *testing.T) {
	global := map[string]memory.SourceConfig{
		"canonical": {Dir: "/shared/memory", Weight: 1.0},
		"docs":      {Dir: "/shared/docs", Weight: 0.5},
	}
	combined := buildAgentMemorySources(global, nil)

	if len(combined) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(combined))
	}
	if combined["canonical"].Weight != 1.0 {
		t.Errorf("canonical weight = %v, want 1.0", combined["canonical"].Weight)
	}
	if combined["docs"].Weight != 0.5 {
		t.Errorf("docs weight = %v, want 0.5", combined["docs"].Weight)
	}
}

func TestBuildAgentMemorySources_AgentOnly(t *testing.T) {
	agentSources := []config.MemorySource{
		{Name: "workspace", Dir: "/agent/memory", Weight: 0.8},
	}
	combined := buildAgentMemorySources(nil, agentSources)

	if len(combined) != 1 {
		t.Fatalf("expected 1 source, got %d", len(combined))
	}
	src, ok := combined["agent:workspace"]
	if !ok {
		t.Fatal("expected 'agent:workspace' key")
	}
	// Weight should include AgentMemoryBoost (1.0)
	expectedWeight := 0.8 + AgentMemoryBoost
	if src.Weight != expectedWeight {
		t.Errorf("agent source weight = %v, want %v (base 0.8 + boost %v)", src.Weight, expectedWeight, AgentMemoryBoost)
	}
}

func TestBuildAgentMemorySources_Combined(t *testing.T) {
	global := map[string]memory.SourceConfig{
		"canonical": {Dir: "/shared/memory", Weight: 1.0},
	}
	agentSources := []config.MemorySource{
		{Name: "workspace", Dir: "/agent/memory", Weight: 1.0},
	}
	combined := buildAgentMemorySources(global, agentSources)

	if len(combined) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(combined))
	}

	// Global source unchanged
	if combined["canonical"].Weight != 1.0 {
		t.Errorf("global weight = %v, want 1.0", combined["canonical"].Weight)
	}

	// Agent source boosted
	agentSrc := combined["agent:workspace"]
	expectedWeight := 1.0 + AgentMemoryBoost
	if agentSrc.Weight != expectedWeight {
		t.Errorf("agent weight = %v, want %v", agentSrc.Weight, expectedWeight)
	}

	// Agent source should rank higher than global with same base weight
	if agentSrc.Weight <= combined["canonical"].Weight {
		t.Error("agent-specific source should have higher weight than global source with same base weight")
	}
}

func TestBuildAgentMemorySources_Empty(t *testing.T) {
	combined := buildAgentMemorySources(nil, nil)
	if len(combined) != 0 {
		t.Errorf("expected 0 sources, got %d", len(combined))
	}
}

func TestPerAgentMemoryIndex(t *testing.T) {
	dir := t.TempDir()

	// Create directories for global and agent-specific memory
	globalDir := filepath.Join(dir, "global")
	agentDir := filepath.Join(dir, "agent")
	os.MkdirAll(globalDir, 0755)
	os.MkdirAll(agentDir, 0755)

	// Write test files
	os.WriteFile(filepath.Join(globalDir, "shared.md"), []byte("Global shared knowledge about Go interfaces"), 0644)
	os.WriteFile(filepath.Join(agentDir, "local.md"), []byte("Agent-specific knowledge about Go interfaces"), 0644)

	// Build combined sources (simulating what main.go does)
	global := map[string]memory.SourceConfig{
		"global": {Dir: globalDir, Weight: 1.0},
	}
	agentSources := []config.MemorySource{
		{Name: "local", Dir: agentDir, Weight: 1.0},
	}
	combined := buildAgentMemorySources(global, agentSources)

	// Create per-agent index
	dbPath := filepath.Join(dir, "memory-test.db")
	idx, err := memory.NewIndex(dbPath, combined, 0, 0.1)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer idx.Close()

	if err := idx.Reindex(); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	results, err := idx.Search("Go interfaces")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Agent-specific source should rank higher due to weight boost
	if results[0].Source != "agent:local" {
		t.Errorf("first result source = %q, want 'agent:local' (should rank higher due to boost)", results[0].Source)
	}
	if results[1].Source != "global" {
		t.Errorf("second result source = %q, want 'global'", results[1].Source)
	}
}

func TestCheckManaPrereqs_MissingCredFile(t *testing.T) {
	warnings := checkManaPrereqs("/nonexistent/path/credentials.json")
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "credentials file not found") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected warning about missing credentials file, got %v", warnings)
	}
}

func TestCheckManaPrereqs_ExistingCredFile(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "creds.json")
	os.WriteFile(tmp, []byte(`{}`), 0644)

	warnings := checkManaPrereqs(tmp)
	for _, w := range warnings {
		if strings.Contains(w, "credentials file not found") {
			t.Errorf("should not warn about existing file, got: %s", w)
		}
	}
}

func TestCheckManaPrereqs_EmptyCredFile(t *testing.T) {
	// Empty path means no credentials file configured — no warning about file
	warnings := checkManaPrereqs("")
	for _, w := range warnings {
		if strings.Contains(w, "credentials file") {
			t.Errorf("should not warn about credentials file when path is empty, got: %s", w)
		}
	}
}

// ========== resolveResetPrompt tests ==========

func TestResolveResetPrompt_Default(t *testing.T) {
	cfg := &config.Config{}
	prompt := resolveResetPrompt(cfg)
	if prompt != config.DefaultSessionResetPrompt {
		t.Errorf("expected default prompt, got %q", prompt)
	}
	if prompt == "" {
		t.Error("default prompt should not be empty")
	}
}

func TestResolveResetPrompt_InlineOverridesDefault(t *testing.T) {
	cfg := &config.Config{}
	cfg.Sessions.SessionResetPrompt = "custom prompt"
	prompt := resolveResetPrompt(cfg)
	if prompt != "custom prompt" {
		t.Errorf("expected inline prompt, got %q", prompt)
	}
}

func TestResolveResetPrompt_FileOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "reset.md")
	os.WriteFile(promptFile, []byte("file prompt content"), 0644)

	cfg := &config.Config{}
	cfg.Sessions.SessionResetPromptFile = promptFile
	prompt := resolveResetPrompt(cfg)
	if prompt != "file prompt content" {
		t.Errorf("expected file prompt, got %q", prompt)
	}
}

func TestResolveResetPrompt_InlineTakesPrecedenceOverFile(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "reset.md")
	os.WriteFile(promptFile, []byte("file prompt"), 0644)

	cfg := &config.Config{}
	cfg.Sessions.SessionResetPrompt = "inline prompt"
	cfg.Sessions.SessionResetPromptFile = promptFile
	prompt := resolveResetPrompt(cfg)
	if prompt != "inline prompt" {
		t.Errorf("expected inline to win, got %q", prompt)
	}
}
