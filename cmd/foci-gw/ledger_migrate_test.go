package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/sqlite"
)

// TestLedgerMigrateDryRun drives `foci-gw ledger-migrate` against a
// pre-ledger api.db built from the v1 schema fixture (the v1 writer itself is
// gone since the cutover, #2111 P2). The source must come out byte-identical;
// the kept copy must hold the ledger.
func TestLedgerMigrateDryRun(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "api.db")
	ddl, err := os.ReadFile("../../internal/delegator/accounting/testdata/v1_api_calls.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.OpenInit(src, string(ddl))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC).Format(time.RFC3339)
	for _, stmt := range []string{
		`INSERT INTO api_calls (ts, session, model, input_tokens, output_tokens, cache_read_tokens,
			cache_write_tokens, calculated_cost_usd, call_type, turn_id, agent_id,
			turn_input_tokens, turn_output_tokens, turn_cache_read_tokens, turn_cache_write_tokens)
			VALUES (?, 'helen/c1', 'claude-opus-5', 10, 20, 300, 40, 0.25, 'delegated_turn', 'helen/c1@1', 'helen',
			11, 21, 301, 41)`,
		`INSERT INTO api_calls (ts, session, model, input_tokens, output_tokens, call_type)
			VALUES (?, 'helen/api', 'claude-opus-5', 5, 6, 'conversation')`,
	} {
		if _, err := db.Exec(stmt, at); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
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
