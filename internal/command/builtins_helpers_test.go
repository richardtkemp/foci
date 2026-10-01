package command

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/log"
	"foci/internal/modelinfo"
	"foci/internal/tools"
)

// apiRow is a cost fixture in the pre-ledger row shape the /cost tests were
// written in: a turn's parent row (CallType "" or "delegated_turn") or one
// subagent's share ("subagent_turn"). Each is booked exactly as the cutover
// migrated a pre-ledger row — a legacy call (accounting.LegacyRow), the
// history every live ledger holds — so CalculatedCostUSD, when set, is the
// cost every reader sees, and without it the counts are priced.
type apiRow struct {
	Timestamp                            time.Time
	Session, Model                       string
	Input, Output, CacheRead, CacheWrite int
	CalculatedCostUSD                    *float64
	CallType                             string
	AgentID, SubagentID, TurnID          string
	// Turn is the turn-summed counts; nil prices the four above.
	Turn *modelinfo.TokenCounts
}

func (e apiRow) legacy() accounting.LegacyRow {
	at, session := e.Timestamp, e.Session
	if at.IsZero() {
		at = time.Now()
	}
	if session == "" {
		session = "test/c1"
	}
	return accounting.LegacyRow{
		At: at, Backend: accounting.BackendCCStream, Provider: "anthropic",
		Session: session, Model: e.Model,
		Fill: accounting.LegacyFill{Input: e.Input, Output: e.Output, CacheRead: e.CacheRead, CacheWrite: e.CacheWrite},
		Turn: e.Turn, CalculatedCostUSD: e.CalculatedCostUSD,
		TurnID: e.TurnID, AgentID: e.AgentID,
		Subagent: e.CallType == "subagent_turn", SubagentID: e.SubagentID,
	}
}

// bookRows books fixtures into l (nil: api.jsonl only).
func bookRows(t *testing.T, l *accounting.Ledger, entries []apiRow) {
	t.Helper()
	for _, e := range entries {
		if err := l.BookLegacy(e.legacy()); err != nil {
			t.Fatalf("book %+v: %v", e, err)
		}
	}
}

// writeAPILog writes fixtures to a temporary api.jsonl the way the ledger
// does with no api.db, and returns its path (for CommandContext.APILogPath).
func writeAPILog(t *testing.T, entries []apiRow) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	log.SetAPIWriter(f)
	bookRows(t, nil, entries)
	log.SetAPIWriter(nil)
	_ = f.Close()
	return path
}

// initAPIDB books fixtures into a temporary ledger made the live one for the
// test, and writes them to api.jsonl too; it returns the JSONL path.
func initAPIDB(t *testing.T, entries []apiRow) string {
	t.Helper()
	l := openTestLedger(t)
	bookRows(t, l, entries)
	return writeAPILog(t, entries)
}

// openTestLedger opens a ledger in a temp dir as the live one for the test.
func openTestLedger(t *testing.T) *accounting.Ledger {
	t.Helper()
	l, _, err := accounting.Open(filepath.Join(t.TempDir(), "api.db"), accounting.Options{})
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	accounting.SetLive(l)
	t.Cleanup(func() {
		accounting.SetLive(nil)
		_ = l.Close()
	})
	return l
}

// callRows books fixtures into a private ledger and reads them back as the
// cost views price them — for the pure renderer tests.
func callRows(t *testing.T, entries ...apiRow) []accounting.CallRow {
	t.Helper()
	l, _, err := accounting.Open(filepath.Join(t.TempDir(), "api.db"), accounting.Options{})
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	defer func() { _ = l.Close() }()
	bookRows(t, l, entries)
	rows, err := l.Calls(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
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
