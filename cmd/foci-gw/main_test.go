package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/memory"
	"foci/internal/secrets"
	"foci/internal/session"
	"foci/internal/telegram"
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

// ========== buildEnvironmentBlock visibility tests ==========

func TestBuildEnvironmentBlock_VisibilitySection(t *testing.T) {
	tests := []struct {
		name        string
		toolCalls   config.ToolCallDisplay
		thinking    config.ShowThinking
		wantTool    string
		wantThink   string
	}{
		{
			name:      "off/off",
			toolCalls: config.ToolCallOff,
			thinking:  config.ShowThinkingOff,
			wantTool:  "hidden from the user",
			wantThink: "hidden from the user",
		},
		{
			name:      "preview/compact",
			toolCalls: config.ToolCallPreview,
			thinking:  config.ShowThinkingCompact,
			wantTool:  "brief previews",
			wantThink: "toggle button",
		},
		{
			name:      "full/true",
			toolCalls: config.ToolCallFull,
			thinking:  config.ShowThinkingTrue,
			wantTool:  "fully visible",
			wantThink: "shown inline",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acfg := config.AgentConfig{
				ID:        "test",
				Workspace: "/tmp/test",
			}
			cfg := &config.Config{
				Defaults: config.DefaultsConfig{
					ShowToolCalls: &tt.toolCalls,
					ShowThinking:  &tt.thinking,
				},
				Logging: config.LoggingConfig{
					EventFile: "/tmp/foci.log",
				},
			}

			block := buildEnvironmentBlock(acfg, "/tmp/foci.toml", cfg, 0)

			if !strings.Contains(block, "## Visibility") {
				t.Error("expected Visibility section")
			}
			if !strings.Contains(block, tt.wantTool) {
				t.Errorf("expected tool call description containing %q", tt.wantTool)
			}
			if !strings.Contains(block, tt.wantThink) {
				t.Errorf("expected thinking description containing %q", tt.wantThink)
			}
		})
	}
}

func TestBuildEnvironmentBlock_AgentOverridesGlobal(t *testing.T) {
	acfg := config.AgentConfig{
		ID:            "test",
		Workspace:     "/tmp/test",
		ShowToolCalls: ptr(config.ToolCallFull),
		ShowThinking:  ptr(config.ShowThinkingTrue),
	}
	cfg := &config.Config{
		Defaults: config.DefaultsConfig{
			ShowToolCalls: ptr(config.ToolCallOff),
			ShowThinking:  ptr(config.ShowThinkingOff),
		},
		Logging: config.LoggingConfig{
			EventFile: "/tmp/foci.log",
		},
	}

	block := buildEnvironmentBlock(acfg, "/tmp/foci.toml", cfg, 0)

	// Agent overrides should win
	if !strings.Contains(block, "fully visible") {
		t.Error("expected agent override for tool calls (full), got global (off)")
	}
	if !strings.Contains(block, "shown inline") {
		t.Error("expected agent override for thinking (true), got global (off)")
	}
}

func TestBuildEnvironmentBlock_CrontabInfo(t *testing.T) {
	acfg := config.AgentConfig{
		ID:        "test",
		Workspace: "/tmp/test",
	}
	cfg := &config.Config{
		Environment: config.EnvironmentConfig{
			Enabled: true,
		},
		Logging: config.LoggingConfig{
			EventFile: "/tmp/foci.log",
		},
	}

	// Test with 0 cron jobs
	block := buildEnvironmentBlock(acfg, "/tmp/foci.toml", cfg, 0)
	if !strings.Contains(block, "You may schedule recurring tasks using crontab. You have 0 jobs scheduled.") {
		t.Error("expected crontab info with 0 jobs")
	}

	// Test with 3 cron jobs
	block = buildEnvironmentBlock(acfg, "/tmp/foci.toml", cfg, 3)
	if !strings.Contains(block, "You may schedule recurring tasks using crontab. You have 3 jobs scheduled.") {
		t.Error("expected crontab info with 3 jobs")
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
		Defaults: config.DefaultsConfig{
			ShowToolCalls: ptr(config.ToolCallOff),
			ShowThinking:  ptr(config.ShowThinkingOff),
			DisplayWidth:  ptr(44),
		},
		Telegram: config.TelegramConfig{
			ReceivedFilesDir: "/global/files",
		},
		Logging: config.LoggingConfig{
			MessagesInLog: false,
		},
	}

	applyAgentDisplaySettings(bot, acfg, cfg)

	stc, st, dw, mil, rfd, _ := bot.DisplaySettings()
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

func TestApplyAgentDisplaySettings_FallsBackToDefaults(t *testing.T) {
	bot := telegram.NewBotForTest()
	acfg := config.AgentConfig{} // all nil/zero — should fall back to defaults
	cfg := &config.Config{
		Defaults: config.DefaultsConfig{
			ShowToolCalls: ptr(config.ToolCallPreview),
			ShowThinking:  ptr(config.ShowThinkingTrue),
			DisplayWidth:  ptr(60),
		},
		Telegram: config.TelegramConfig{
			ReceivedFilesDir: "/global/files",
		},
		Logging: config.LoggingConfig{
			MessagesInLog: true,
		},
	}

	applyAgentDisplaySettings(bot, acfg, cfg)

	stc, st, dw, mil, rfd, _ := bot.DisplaySettings()
	if stc != "preview" {
		t.Errorf("ShowToolCalls = %q, want %q (defaults fallback)", stc, "preview")
	}
	if st != "true" {
		t.Errorf("ShowThinking = %q, want %q (defaults fallback)", st, "true")
	}
	if dw != 60 {
		t.Errorf("DisplayWidth = %d, want 60 (defaults fallback)", dw)
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

	_, _, _, _, rfd, _ := bot.DisplaySettings()
	if rfd != "/pre-existing" {
		t.Errorf("ReceivedFilesDir = %q, want %q (should not be overwritten when both empty)", rfd, "/pre-existing")
	}
}

func TestApplyAgentDisplaySettings_PartialOverride(t *testing.T) {
	bot := telegram.NewBotForTest()
	// Only override ShowToolCalls; rest falls back to defaults
	acfg := config.AgentConfig{
		ShowToolCalls: ptr(config.ToolCallFull),
	}
	cfg := &config.Config{
		Defaults: config.DefaultsConfig{
			ShowToolCalls: ptr(config.ToolCallOff),
			ShowThinking:  ptr(config.ShowThinkingCompact),
			DisplayWidth:  ptr(44),
		},
		Logging: config.LoggingConfig{
			MessagesInLog: true,
		},
	}

	applyAgentDisplaySettings(bot, acfg, cfg)

	stc, st, dw, mil, _, _ := bot.DisplaySettings()
	if stc != "full" {
		t.Errorf("ShowToolCalls = %q, want %q (agent override)", stc, "full")
	}
	if st != "compact" {
		t.Errorf("ShowThinking = %q, want %q (defaults fallback)", st, "compact")
	}
	if dw != 44 {
		t.Errorf("DisplayWidth = %d, want 44 (defaults fallback)", dw)
	}
	if !mil {
		t.Error("MessagesInLog = false, want true (global fallback)")
	}
}

// ========== tokenHolder tests ==========

func TestTokenHolder_GetSet(t *testing.T) {
	h := &tokenHolder{token: "initial"}
	tok, err := h.Get()
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if tok != "initial" {
		t.Errorf("Get = %q, want %q", tok, "initial")
	}

	h.Set("updated")
	tok, err = h.Get()
	if err != nil {
		t.Fatalf("Get after Set: unexpected error: %v", err)
	}
	if tok != "updated" {
		t.Errorf("Get after Set = %q, want %q", tok, "updated")
	}
}

func TestTokenHolder_EmptyReturnsError(t *testing.T) {
	h := &tokenHolder{}
	_, err := h.Get()
	if err == nil {
		t.Fatal("expected error for empty tokenHolder")
	}
}

func TestTokenHolder_ConcurrentAccess(t *testing.T) {
	h := &tokenHolder{token: "start"}
	var wg sync.WaitGroup

	// Concurrent writers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.Set("token-" + strings.Repeat("x", i))
		}(i)
	}

	// Concurrent readers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := h.Get()
			if err != nil {
				t.Errorf("concurrent Get error: %v", err)
			}
			if tok == "" {
				t.Error("concurrent Get returned empty")
			}
		}()
	}

	wg.Wait()
}

// ========== /-/reload-credentials endpoint tests ==========

func TestReloadCredentialsEndpoint_Success(t *testing.T) {
	// Write a secrets.toml with a setup token
	dir := t.TempDir()
	secretsPath := filepath.Join(dir, "secrets.toml")
	os.WriteFile(secretsPath, []byte(`[anthropic]
setup_token = "sk-ant-oat01-new-token-value-for-testing-that-is-long-enough-to-pass-validation-checks-here"`+"\n"), 0600)

	holder := &tokenHolder{token: "old-token"}
	cfg := &config.Config{}

	reloadFn := func() error {
		st, err := secrets.Load(secretsPath)
		if err != nil {
			return err
		}
		token, _ := st.Get("anthropic.setup_token")
		if token == "" {
			token, _ = st.Get("anthropic.api_key")
		}
		if token == "" {
			return nil
		}
		holder.Set(token)
		return nil
	}

	mux := http.NewServeMux()
	registerHTTPHandlers(mux, httpHandlerDeps{
		agents:            map[string]*agentInstance{},
		agentOrder:        []string{},
		cfg:               cfg,
		reloadCredentials: reloadFn,
	})

	req := httptest.NewRequest(http.MethodPost, "/-/reload-credentials", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] == "" {
		t.Error("expected non-empty status in response")
	}

	tok, _ := holder.Get()
	expected := "sk-ant-oat01-new-token-value-for-testing-that-is-long-enough-to-pass-validation-checks-here"
	if tok != expected {
		t.Errorf("holder token = %q, want %q", tok, expected)
	}
}

func TestReloadCredentialsEndpoint_MethodNotAllowed(t *testing.T) {
	mux := http.NewServeMux()
	registerHTTPHandlers(mux, httpHandlerDeps{
		agents:            map[string]*agentInstance{},
		agentOrder:        []string{},
		cfg:               &config.Config{},
		reloadCredentials: func() error { return nil },
	})

	req := httptest.NewRequest(http.MethodGet, "/-/reload-credentials", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestReloadCredentialsEndpoint_NotRegisteredWithoutHolder(t *testing.T) {
	mux := http.NewServeMux()
	registerHTTPHandlers(mux, httpHandlerDeps{
		agents:     map[string]*agentInstance{},
		agentOrder: []string{},
		cfg:        &config.Config{},
		// reloadCredentials is nil — endpoint should not be registered
	})

	req := httptest.NewRequest(http.MethodPost, "/-/reload-credentials", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (endpoint not registered)", w.Code)
	}
}

func TestAuthMiddleware(t *testing.T) {
	const apiKey = "test-secret-key"

	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	handler := authMiddleware(apiKey, backend)

	tests := []struct {
		name       string
		path       string
		bearer     string   // Authorization: Bearer value
		queryKey   string   // api_key query param
		wantStatus int
	}{
		{
			name:       "bearer valid",
			path:       "/status",
			bearer:     apiKey,
			wantStatus: http.StatusOK,
		},
		{
			name:       "bearer invalid",
			path:       "/status",
			bearer:     "wrong-key",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "query param valid",
			path:       "/status",
			queryKey:   apiKey,
			wantStatus: http.StatusOK,
		},
		{
			name:       "query param invalid",
			path:       "/status",
			queryKey:   "wrong-key",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "no auth",
			path:       "/status",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "voice endpoint requires auth",
			path:       "/voice",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "voice endpoint with bearer",
			path:       "/voice",
			bearer:     apiKey,
			wantStatus: http.StatusOK,
		},
		{
			name:       "bearer on send",
			path:       "/send",
			bearer:     apiKey,
			wantStatus: http.StatusOK,
		},
		{
			name:       "no auth on command",
			path:       "/command",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "no auth on reload-credentials",
			path:       "/-/reload-credentials",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "bearer on reload-credentials",
			path:       "/-/reload-credentials",
			bearer:     apiKey,
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := tt.path
			if tt.queryKey != "" {
				target += "?api_key=" + tt.queryKey
			}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			if tt.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tt.bearer)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
		})
	}
}
