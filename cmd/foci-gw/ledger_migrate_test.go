package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/log"
	"foci/internal/modelinfo"
)

// TestLedgerMigrateDryRun drives `foci-gw ledger-migrate` against an api.db
// written by the REAL pre-ledger writer (log.InitAPIDB + log.API), so the
// migration is proven against the schema production actually has rather than
// a hand-copied one. The source must come out byte-identical; the kept copy
// must hold the ledger.
func TestLedgerMigrateDryRun(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "api.db")
	if err := log.InitAPIDB(src); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)
	calc := 0.25
	log.API(log.APIEntry{Timestamp: at, Session: "helen/c1", Model: "claude-opus-5", Input: 10, Output: 20,
		CacheRead: 300, CacheWrite: 40, CalculatedCostUSD: &calc, CallType: "delegated_turn",
		TurnID: "helen/c1@1", AgentID: "helen",
		Turn: &modelinfo.TokenCounts{Input: 11, Output: 21, CacheRead: 301, CacheWrite: 41}})
	log.API(log.APIEntry{Timestamp: at, Session: "helen/api", Model: "claude-opus-5", Input: 5, Output: 6, CallType: "conversation"})
	log.CloseAPIDB()
	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "migrated.db")
	var stdout, stderr bytes.Buffer
	if code := runLedgerMigrateDryRun([]string{"-from", src, "-out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ledger migration: 2 v1 rows -> 2 legacy calls, 2 turns") {
		t.Errorf("report:\n%s", stdout.String())
	}
	after, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the dry run modified its source database")
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("-out copy missing: %v", err)
	}

	// Run again on the migrated copy: it already holds the ledger.
	stdout.Reset()
	if code := runLedgerMigrateDryRun([]string{"-from", out}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "nothing to migrate") {
		t.Errorf("second run: exit %d, %q %q", code, stdout.String(), stderr.String())
	}
	if code := runLedgerMigrateDryRun(nil, &stdout, &stderr); code != 2 {
		t.Errorf("no -from: exit %d, want 2", code)
	}
}
