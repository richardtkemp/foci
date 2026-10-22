package agent

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"foci/internal/convo"
	"foci/internal/delegator"
)

// TestRedelivery_WritesNoRecvRow: a redelivery re-sends an input the backend
// never consumed (#2050). Its text is the input exactly as it was first
// composed — [meta] header, nudges and all — not something the user typed, and
// the message that produced it was recorded when it first arrived. So the
// redelivery turn must not record another `recv` row. Its reply is a normal
// delivered turn and is still recorded (by the loggingSink, not checked here).
func TestRedelivery_WritesNoRecvRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	if err := convo.InitPerAgent([]string{"test"}, func(string) string { return dbPath }); err != nil {
		t.Fatalf("InitPerAgent: %v", err)
	}
	defer convo.Close()

	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	var prompts []string
	be.sendToPaneFn = func(_ context.Context, prompt string, h *mockHandler) (*delegator.TurnResult, error) {
		prompts = append(prompts, prompt)
		if h != nil && h.OnTurnComplete != nil {
			go h.OnTurnComplete(&delegator.TurnResult{Text: "ok"})
		}
		return nil, nil
	}
	// No SessionIndex on the manager: stillUndelivered reports true, so the
	// redelivery really runs (the premise this test needs).
	a := &Agent{Model: "test-model", DelegatedManager: newMockDelegatedManager(t, be)}

	// The original message: recorded once, as it arrives.
	if err := a.HandleMessage(WithTrigger(context.Background(), "voice"), "test/s", []string{"hello"}, nil); err != nil {
		t.Fatalf("original turn: %v", err)
	}
	if n := countRecv(t, dbPath); n != 1 {
		t.Fatalf("recv rows after the original = %d, want 1 (premise)", n)
	}

	// Its redelivery, shaped as agents_delegated.go's Redeliver sends it: the
	// composed text as the turn's text, under TriggerRedelivery.
	r := Redelivery{Input: delegator.PendingInput{ID: "in1", Text: prompts[0]}, Reason: "cc-exit"}
	ctx := WithRedelivery(WithTrigger(context.Background(), TriggerRedelivery), r)
	if err := a.HandleMessage(ctx, "test/s", []string{r.Input.Text}, nil); err != nil {
		t.Fatalf("redelivery turn: %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("backend saw %d prompts, want 2 (premise: the redelivery reached the backend)", len(prompts))
	}
	if n := countRecv(t, dbPath); n != 1 {
		t.Errorf("recv rows after the redelivery = %d, want 1 (the redelivery duplicated the message)", n)
	}
}

func countRecv(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open convo DB: %v", err)
	}
	defer db.Close() // nolint:errcheck
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE direction = 'recv'`).Scan(&n); err != nil {
		t.Fatalf("count recv rows: %v", err)
	}
	return n
}
