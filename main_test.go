package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"clod/agent"
	"clod/session"
)

func TestGracefulShutdown_AllIdle(t *testing.T) {
	agents := map[string]*agentInstance{
		"a": {id: "a", ag: &agent.Agent{}},
		"b": {id: "b", ag: &agent.Agent{}},
	}
	start := time.Now()
	gracefulShutdown(agents)
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
	gracefulShutdown(agents)
	elapsed := time.Since(start)

	if elapsed < 250*time.Millisecond {
		t.Errorf("shutdown returned too early (%v), should wait for processing", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("shutdown took too long (%v), should complete soon after agent finishes", elapsed)
	}
}

func TestGracefulShutdown_TimesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 5s timeout test in short mode")
	}

	ag := &agent.Agent{}
	ag.SetProcessingForTest(1) // never cleared — simulates stuck agent

	agents := map[string]*agentInstance{
		"a": {id: "a", ag: ag},
	}

	start := time.Now()
	gracefulShutdown(agents)
	elapsed := time.Since(start)

	// Should time out after ~5s (50 * 100ms)
	if elapsed < 4*time.Second || elapsed > 7*time.Second {
		t.Errorf("shutdown took %v, expected ~5s timeout", elapsed)
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

	injectWelcomeFile(welcomePath, agents, agentOrder, sessions)

	// File should be deleted
	if _, err := os.Stat(welcomePath); !os.IsNotExist(err) {
		t.Error("welcome file should be deleted after injection")
	}

	// Session should have messages
	msgs, err := sessions.LoadFull("agent:main:main")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (user + assistant), got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("first message role = %q, want 'user'", msgs[0].Role)
	}
}

func TestInjectWelcomeFile_NoFile(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "sessions")
	os.MkdirAll(sessDir, 0755)
	sessions := session.NewStore(sessDir)

	agents := map[string]*agentInstance{
		"main": {id: "main", sessionKey: "agent:main:main"},
	}
	agentOrder := []string{"main"}

	// Should not panic or error when file doesn't exist
	injectWelcomeFile(filepath.Join(dir, "nonexistent.md"), agents, agentOrder, sessions)

	msgs, _ := sessions.LoadFull("agent:main:main")
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages when no welcome file, got %d", len(msgs))
	}
}

func TestInjectWelcomeFile_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "sessions")
	os.MkdirAll(sessDir, 0755)
	sessions := session.NewStore(sessDir)

	welcomePath := filepath.Join(dir, "WELCOME.md")
	os.WriteFile(welcomePath, []byte(""), 0644)

	agents := map[string]*agentInstance{
		"main": {id: "main", sessionKey: "agent:main:main"},
	}
	agentOrder := []string{"main"}

	injectWelcomeFile(welcomePath, agents, agentOrder, sessions)

	// File should be deleted even if empty
	if _, err := os.Stat(welcomePath); !os.IsNotExist(err) {
		t.Error("empty welcome file should be deleted")
	}

	// No messages should be injected
	msgs, _ := sessions.LoadFull("agent:main:main")
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages for empty welcome file, got %d", len(msgs))
	}
}

func TestInjectWelcomeFile_EmptyPath(t *testing.T) {
	// Should not panic when path is empty
	injectWelcomeFile("", nil, nil, nil)
}
