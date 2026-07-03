package command

import (
	"context"
	"encoding/json"
	"foci/internal/log"
	"foci/internal/tools"
	"os"
	"path/filepath"
	"testing"
)

// writeAPILog is a helper that creates a temporary API log file with the given entries.
func writeAPILog(t *testing.T, entries []log.APIEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, _ := json.Marshal(e)
		f.Write(data)
		f.Write([]byte("\n"))
	}
	f.Close()
	return path
}

// initAPIDB is a helper that initializes a temporary api.db and inserts entries.
// It also writes the JSONL log for commands that still use ReadAPILog.
// Returns the JSONL path (for APILogPath) and registers cleanup.
func initAPIDB(t *testing.T, entries []log.APIEntry) string {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "api.db")
	if err := log.InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	t.Cleanup(func() { log.CloseAPIDB() })

	for _, e := range entries {
		log.API(e) // writes to DB (and skips JSONL since logger has no apiFile)
	}

	return writeAPILog(t, entries)
}

// testContextInfo returns a standard ContextInfo for testing context commands.
func testContextInfo() ContextInfo {
	return ContextInfo{
		SessionKey:       "main/i0",
		Model:            "claude-sonnet-4-5",
		CompactionThresh: 0.8,
		ContextLimit:     200000,
		SystemSections: []SystemSection{
			{Name: "IDENTITY.md", Chars: 2000},
			{Name: "SOUL.md", Chars: 4000},
			{Name: "MEMORY.md", Chars: 3000},
		},
		EnvironmentChars: 1200,
		SkillsChars:      800,
		Messages: MessageBreakdown{
			UserChars:       8000,
			AssistantChars:  12000,
			ToolResultChars: 6000,
			UserCount:       5,
			AssistantCount:  5,
		},
	}
}

// mockTmuxExec returns a mock execFn that records the JSON params it receives.
func mockTmuxExec(result string, err error) (func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error), *[]map[string]interface{}) {
	var calls []map[string]interface{}
	return func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
		var m map[string]interface{}
		json.Unmarshal(params, &m)
		calls = append(calls, m)
		if err != nil {
			return tools.ToolResult{}, err
		}
		return tools.TextResult(result), err
	}, &calls
}
