package ccstream

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/modelinfo"
)

// TestShadowWiring drives the Backend's own hooks: the stream names a
// main-thread call, CC appends its line to the main transcript, a result
// arrives, and the process exits. The call lands in the SHADOW ledger — and
// nowhere else: no observer (api.jsonl, trace generation) sees a shadow
// booking, and the live ledger is untouched. Not parallel: it sets the
// process's shadow ledger.
func TestShadowWiring(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-shadow.db")
	shadow, _, err := accounting.Open(path, accounting.Options{Shadow: true, NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	accounting.SetShadow(shadow)
	t.Cleanup(func() { accounting.SetShadow(nil); _ = shadow.Close() })
	accounting.BookedHook = func(b accounting.Booking) { t.Errorf("a shadow booking reached the observers: %+v", b.Call) }
	t.Cleanup(func() { accounting.BookedHook = nil })

	b := newTestBackend(&bytes.Buffer{})
	sh := newCCShadow(b, "cap/c1", "cap", nil)
	b.shadow.Store(sh)
	transcript := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"assistant","message":{"id":"old","model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":9}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(transcript)
	sh.startMainTail(transcript, fi.Size()) // a resumed process: the old line is an earlier process's

	b.turnMu.Lock()
	b.turnRowID = "cap/c1@1"
	b.turnMu.Unlock()
	stop := "end_turn"
	b.OnAssistant(&AssistantMessage{Type: "assistant", Message: BetaMessage{ID: "msg_1", Model: "claude-opus-5",
		StopReason: &stop, Usage: TokenUsage{InputTokens: 10, OutputTokens: 20}}})
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"assistant","timestamp":"2026-09-29T10:00:00Z","message":{"id":"msg_1","model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":20}}}` + "\n")
	_ = f.Close()
	b.OnResult(&ResultMessage{Type: "result", ModelUsage: map[string]ModelUsage{"claude-opus-5": {InputTokens: 10, OutputTokens: 20, CostUSD: 0.01}}})
	sh.close()

	rows, err := shadow.Calls(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TurnID != "cap/c1@1" || rows[0].Count(modelinfo.ClassOutput) != 20 {
		t.Fatalf("shadow calls = %+v, want msg_1 on cap/c1@1", rows)
	}
	if accounting.Live() != nil {
		t.Error("the test must not have a live ledger")
	}
}
