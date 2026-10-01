package command

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/log"
	"foci/internal/modelinfo"
	"foci/internal/tools"
)

// apiRow is a cost fixture: one call, booked the way a backend books one
// (accounting.RecordCall, a completed Claude Code call), on a turn of its own
// unless TurnID names a shared one.
type apiRow struct {
	Timestamp      time.Time
	Session, Model string
	// The call's counts; CacheWrite is booked as a 1h write.
	Input, Output, CacheRead, CacheWrite int
	// CostUSD, for a test about money rather than counts, books output tokens
	// worth exactly this at Model's rates; the counts above must then be zero.
	CostUSD         *float64
	AgentID, TurnID string
	// Actor is the subagent that made the call; "" is the session's own thread.
	Actor string
}

// fixtureModel prices a fixture that names no model.
const fixtureModel = "claude-opus-5"

// fixtureKeys makes every fixture call's key unique, across tests too.
var fixtureKeys atomic.Int64

func (e apiRow) call(t *testing.T) (accounting.Turn, accounting.Call) {
	t.Helper()
	at, session, model := e.Timestamp, e.Session, e.Model
	if at.IsZero() {
		at = time.Now()
	}
	if session == "" {
		session = "test/c1"
	}
	if model == "" {
		model = fixtureModel
	}
	tokens := modelinfo.Tokens{modelinfo.ClassInput: e.Input, modelinfo.ClassOutput: e.Output,
		modelinfo.ClassCacheRead: e.CacheRead, modelinfo.ClassCacheWrite1h: e.CacheWrite}
	if e.CostUSD != nil {
		if e.Input+e.Output+e.CacheRead+e.CacheWrite != 0 {
			t.Fatalf("fixture %+v: CostUSD sets the counts, so they must be zero", e)
		}
		perMTok, _ := modelinfo.CostAsOf(model, at, modelinfo.Tokens{modelinfo.ClassOutput: 1_000_000})
		tokens = modelinfo.Tokens{modelinfo.ClassOutput: int(math.Round(*e.CostUSD / perMTok * 1e6))}
		if usd, ok := modelinfo.CostAsOf(model, at, tokens); !ok || math.Abs(usd-*e.CostUSD) > 1e-9 {
			t.Fatalf("fixture %+v: no whole number of %s output tokens costs $%v", e, model, *e.CostUSD)
		}
	}
	key := fmt.Sprintf("msg_fixture_%d", fixtureKeys.Add(1))
	turnID := e.TurnID
	if turnID == "" {
		turnID = session + "@" + key
	}
	turn := accounting.Turn{TurnID: turnID, Session: session, AgentID: e.AgentID,
		Backend: accounting.BackendCCStream, Source: accounting.SourceUser, StartedAt: at}
	return turn, accounting.Call{Key: key, Backend: accounting.BackendCCStream, Provider: "anthropic",
		Model: model, Session: session, AgentID: e.AgentID, TurnID: turnID, Actor: e.Actor,
		Kind: accounting.KindCall, Finality: accounting.FinalityCompleted,
		ClassMethod: accounting.ClassMethodObserved, BilledAt: at, Tokens: tokens}
}

// bookRows books fixtures into l (nil: api.jsonl only).
func bookRows(t *testing.T, l *accounting.Ledger, entries []apiRow) {
	t.Helper()
	for _, e := range entries {
		turn, call := e.call(t)
		if _, err := l.RecordCall(turn, call, nil); err != nil {
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
