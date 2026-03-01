package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/agent"
	"foci/config"
	"foci/memory"
	"foci/session"
	"foci/telegram"
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
		"main": {id: "main", defaultSessionKey: func() string { return "agent:main:main" }},
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
		"main": {id: "main", defaultSessionKey: func() string { return "agent:main:main" }},
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
		"main": {id: "main", defaultSessionKey: func() string { return "agent:main:main" }},
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
		"main": {id: "main", defaultSessionKey: func() string { return "agent:main:main" }},
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

	results, err := idx.Search("Go interfaces", "")
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

// ========== readPromptFile tests ==========

func TestReadPromptFile_LoadsFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "prompt.md")
	os.WriteFile(f, []byte("prompt content"), 0644)

	result := readPromptFile(f, "test")
	if result != "prompt content" {
		t.Errorf("expected file content, got %q", result)
	}
}

func TestReadPromptFile_EmptyPathReturnsEmpty(t *testing.T) {
	result := readPromptFile("", "test")
	if result != "" {
		t.Errorf("expected empty for empty path, got %q", result)
	}
}

func TestReadPromptFile_MissingFileReturnsEmpty(t *testing.T) {
	result := readPromptFile("/nonexistent/path/prompt.md", "test")
	if result != "" {
		t.Errorf("expected empty for missing file, got %q", result)
	}
}

func TestReadPromptFile_TrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "prompt.md")
	os.WriteFile(f, []byte("  trimmed  \n\n"), 0644)

	result := readPromptFile(f, "test")
	if result != "trimmed" {
		t.Errorf("expected trimmed content, got %q", result)
	}
}

// ========== applyAgentDisplaySettings tests ==========

func ptr[T any](v T) *T { return &v }

func TestApplyAgentDisplaySettings_AgentOverridesGlobal(t *testing.T) {
	bot := telegram.NewBotForTest()
	acfg := config.AgentConfig{
		ShowToolCalls: ptr(config.ToolCallFull),
		ShowThinking:  ptr(config.ShowThinkingCompact),
		DisplayWidth:  ptr(80),
		MessagesInLog: ptr(true),
		ReceivedFilesDir: "/agent/files",
	}
	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			ShowToolCalls:    config.ToolCallOff,
			ShowThinking:     config.ShowThinkingOff,
			DisplayWidth:     44,
			ReceivedFilesDir: "/global/files",
		},
		Logging: config.LoggingConfig{
			MessagesInLog: false,
		},
	}

	applyAgentDisplaySettings(bot, acfg, cfg)

	stc, st, dw, mil, rfd := bot.DisplaySettings()
	if stc != "full" {
		t.Errorf("ShowToolCalls = %q, want %q", stc, "full")
	}
	if st != "compact" {
		t.Errorf("ShowThinking = %q, want %q", st, "compact")
	}
	if dw != 80 {
		t.Errorf("DisplayWidth = %d, want 80", dw)
	}
	if !mil {
		t.Error("MessagesInLog = false, want true")
	}
	if rfd != "/agent/files" {
		t.Errorf("ReceivedFilesDir = %q, want %q", rfd, "/agent/files")
	}
}

func TestApplyAgentDisplaySettings_FallsBackToGlobal(t *testing.T) {
	bot := telegram.NewBotForTest()
	acfg := config.AgentConfig{} // all nil/zero — should fall back to global
	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			ShowToolCalls:    config.ToolCallPreview,
			ShowThinking:     config.ShowThinkingTrue,
			DisplayWidth:     60,
			ReceivedFilesDir: "/global/files",
		},
		Logging: config.LoggingConfig{
			MessagesInLog: true,
		},
	}

	applyAgentDisplaySettings(bot, acfg, cfg)

	stc, st, dw, mil, rfd := bot.DisplaySettings()
	if stc != "preview" {
		t.Errorf("ShowToolCalls = %q, want %q (global fallback)", stc, "preview")
	}
	if st != "true" {
		t.Errorf("ShowThinking = %q, want %q (global fallback)", st, "true")
	}
	if dw != 60 {
		t.Errorf("DisplayWidth = %d, want 60 (global fallback)", dw)
	}
	if !mil {
		t.Error("MessagesInLog = false, want true (global fallback)")
	}
	if rfd != "/global/files" {
		t.Errorf("ReceivedFilesDir = %q, want %q (global fallback)", rfd, "/global/files")
	}
}

func TestApplyAgentDisplaySettings_ReceivedFilesDirBothEmpty(t *testing.T) {
	bot := telegram.NewBotForTest()
	// Pre-set a value to verify it's NOT overwritten when both are empty
	bot.SetReceivedFilesDir("/pre-existing")

	acfg := config.AgentConfig{ReceivedFilesDir: ""}
	cfg := &config.Config{
		Telegram: config.TelegramConfig{ReceivedFilesDir: ""},
	}

	applyAgentDisplaySettings(bot, acfg, cfg)

	_, _, _, _, rfd := bot.DisplaySettings()
	if rfd != "/pre-existing" {
		t.Errorf("ReceivedFilesDir = %q, want %q (should not be overwritten when both empty)", rfd, "/pre-existing")
	}
}

func TestApplyAgentDisplaySettings_PartialOverride(t *testing.T) {
	bot := telegram.NewBotForTest()
	// Only override ShowToolCalls; rest falls back to global
	acfg := config.AgentConfig{
		ShowToolCalls: ptr(config.ToolCallFull),
	}
	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			ShowToolCalls: config.ToolCallOff,
			ShowThinking:  config.ShowThinkingCompact,
			DisplayWidth:  44,
		},
		Logging: config.LoggingConfig{
			MessagesInLog: true,
		},
	}

	applyAgentDisplaySettings(bot, acfg, cfg)

	stc, st, dw, mil, _ := bot.DisplaySettings()
	if stc != "full" {
		t.Errorf("ShowToolCalls = %q, want %q (agent override)", stc, "full")
	}
	if st != "compact" {
		t.Errorf("ShowThinking = %q, want %q (global fallback)", st, "compact")
	}
	if dw != 44 {
		t.Errorf("DisplayWidth = %d, want 44 (global fallback)", dw)
	}
	if !mil {
		t.Error("MessagesInLog = false, want true (global fallback)")
	}
}
