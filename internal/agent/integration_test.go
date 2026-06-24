package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/anthropic"
	"foci/internal/log"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestBranchCacheSharing(t *testing.T) {
	// Proves end-to-end that branched sessions share the Anthropic prompt cache with their parent: a branch request must show cache_read > 0, confirming no redundant token billing for the shared system prompt prefix.
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}

	tmpDir := t.TempDir()
	workspaceDir := filepath.Join(tmpDir, "workspace")
	sessionsDir := filepath.Join(tmpDir, "sessions")
	apiLogPath := filepath.Join(tmpDir, "api.jsonl")

	// Write large workspace content — Haiku requires >= 2048 tokens for caching.
	os.MkdirAll(workspaceDir, 0755)
	var sb strings.Builder
	sb.WriteString("# Agent Identity\n\nYou are a helpful programming assistant.\n\n")
	for i := range 100 {
		fmt.Fprintf(&sb, "Knowledge domain %d: You have deep expertise in area %d, covering subtopics %d-alpha, %d-beta, and %d-gamma. When asked about domain %d, provide comprehensive explanations with practical examples and best practices for real-world applications.\n", i, i, i, i, i, i)
	}
	os.WriteFile(filepath.Join(workspaceDir, "IDENTITY.md"), []byte(sb.String()), 0644)

	// Set up API log capture
	apiLogFile, err := os.OpenFile(apiLogPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("create api log: %v", err)
	}
	oldAPIWriter := captureAPIWriter(apiLogFile)
	defer func() {
		restoreAPIWriter(oldAPIWriter)
		apiLogFile.Close()
	}()

	// Create components
	client := anthropic.NewClient(func() (string, error) { return apiKey, nil }, 60*time.Second)
	sessions := session.NewStore(sessionsDir)
	bootstrap := workspace.NewBootstrap(workspaceDir, nil)
	registry := tools.NewRegistry()

	ag := &Agent{
		Client:    client,
		Sessions:  sessions,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	ctx := context.Background()
	parentKey := "cachetest/imain/1000000000"

	// --- Step 1: First parent request (expect cache WRITE) ---
	t.Log("=== Step 1: First parent request (expect cache WRITE) ===")
	resp1, err := ag.hmTest(ctx, parentKey, "Tell me about the Go programming language.")
	if err != nil {
		t.Fatalf("Step 1 failed: %v", err)
	}
	t.Logf("Step 1 response: %s", ellipsis(resp1, 80))

	entry1 := lastAPIEntry(t, apiLogPath)
	t.Logf("Step 1: input=%d output=%d cache_creation=%d cache_read=%d",
		entry1.Input, entry1.Output, entry1.CacheWrite, entry1.CacheRead)
	if entry1.CacheWrite == 0 && entry1.CacheRead == 0 {
		t.Error("Step 1: expected caching activity (cache_creation or cache_read > 0)")
	}
	if entry1.CacheRead > 0 {
		t.Log("Step 1: cache already warm from previous run (cache_read > 0)")
	}

	// --- Step 2: Second parent request (expect cache READ) ---
	t.Log("=== Step 2: Second parent request (expect cache READ) ===")
	resp2, err := ag.hmTest(ctx, parentKey, "How do goroutines work?")
	if err != nil {
		t.Fatalf("Step 2 failed: %v", err)
	}
	t.Logf("Step 2 response: %s", ellipsis(resp2, 80))

	entry2 := lastAPIEntry(t, apiLogPath)
	t.Logf("Step 2: input=%d output=%d cache_creation=%d cache_read=%d",
		entry2.Input, entry2.Output, entry2.CacheWrite, entry2.CacheRead)
	if entry2.CacheRead == 0 {
		t.Error("Step 2: expected cache_read > 0 (cache read)")
	}

	// --- Step 3: Create branch ---
	branchKey := "cachetest/imain/1000000000/b1000000001"
	if err := sessions.TestCreateBranch(parentKey, branchKey); err != nil {
		t.Fatalf("CreateBranch failed: %v", err)
	}
	t.Log("=== Step 3: Branch created ===")

	// Verify branch loaded the parent prefix
	branchMsgs, _ := sessions.LoadFull(branchKey)
	parentMsgs, _ := sessions.Load(parentKey)
	t.Logf("Parent messages: %d, Branch loaded: %d (should be equal)", len(parentMsgs), len(branchMsgs))
	if len(branchMsgs) != len(parentMsgs) {
		t.Fatalf("Branch LoadFull returned %d messages, want %d (parent count)", len(branchMsgs), len(parentMsgs))
	}

	// --- Step 4: Branch request (expect cache READ for shared prefix) ---
	t.Log("=== Step 4: BRANCH request (expect cache READ on shared prefix) ===")
	resp4, err := ag.hmTest(ctx, branchKey, "What is the Go module system?")
	if err != nil {
		t.Fatalf("Step 4 failed: %v", err)
	}
	t.Logf("Step 4 response: %s", ellipsis(resp4, 80))

	entry4 := lastAPIEntry(t, apiLogPath)
	t.Logf("Step 4: input=%d output=%d cache_creation=%d cache_read=%d",
		entry4.Input, entry4.Output, entry4.CacheWrite, entry4.CacheRead)
	if entry4.CacheRead == 0 {
		t.Fatal("Step 4: CRITICAL FAILURE — expected cache_read > 0 on branch, got 0. Branch cache sharing does NOT work.")
	}
	t.Log("Step 4: SUCCESS — branch shares cache with parent!")

	// --- Step 5: Parent after branch (expect cache READ still works) ---
	t.Log("=== Step 5: Parent after branch (expect cache READ still works) ===")
	resp5, err := ag.hmTest(ctx, parentKey, "Tell me about Go interfaces.")
	if err != nil {
		t.Fatalf("Step 5 failed: %v", err)
	}
	t.Logf("Step 5 response: %s", ellipsis(resp5, 80))

	entry5 := lastAPIEntry(t, apiLogPath)
	t.Logf("Step 5: input=%d output=%d cache_creation=%d cache_read=%d",
		entry5.Input, entry5.Output, entry5.CacheWrite, entry5.CacheRead)
	if entry5.CacheRead == 0 {
		t.Error("Step 5: expected cache_read > 0 (parent cache still works after branch)")
	}

	t.Log("=== ALL STEPS PASSED — Full-stack branch cache sharing works. ===")
}

// apiEntry matches the JSON structure in api.jsonl.
type apiEntry struct {
	Input      int    `json:"input"`
	Output     int    `json:"output"`
	CacheRead  int    `json:"cache_read"`
	CacheWrite int    `json:"cache_write"`
	StopReason string `json:"stop_reason"`
}

// lastAPIEntry reads the last entry from the api.jsonl file.
func lastAPIEntry(t *testing.T, path string) apiEntry {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open api log: %v", err)
	}
	defer f.Close()

	var last string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			last = line
		}
	}
	if last == "" {
		t.Fatal("api log is empty")
	}

	var entry apiEntry
	if err := json.Unmarshal([]byte(last), &entry); err != nil {
		t.Fatalf("parse api entry: %v (line: %s)", err, last)
	}
	return entry
}

// captureAPIWriter redirects API logging to the given file.
// Returns the previous file for restoration.
func captureAPIWriter(f *os.File) *os.File {
	// We can't directly read the old value, so just set the new one.
	// The test will restore to nil.
	log.SetAPIWriter(f)
	return nil
}

// restoreAPIWriter restores the API log writer.
func restoreAPIWriter(f *os.File) {
	log.SetAPIWriter(f)
}

func ellipsis(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
