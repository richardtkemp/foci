package cctmux

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/modelinfo"
)

// TestAssistantLinesBookOnTheirTurn: the tmux backend books each main-thread
// call once its usage is final — the line carrying its stop_reason — on the
// foci turn open then; an in-flight line books nothing, and a re-appended copy
// of a booked call (CC re-appends history after a compaction) books nothing
// more. With no foci turn open the call books on an autonomous turn of its
// own. Not parallel: it sets the process's live ledger.
func TestAssistantLinesBookOnTheirTurn(t *testing.T) {
	l, _, err := accounting.Open(filepath.Join(t.TempDir(), "api.db"), accounting.Options{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	accounting.SetLive(l)
	t.Cleanup(func() { accounting.SetLive(nil); _ = l.Close() })

	b := &Backend{}
	b.ledgerID.Store(&ledgerID{session: "tm/c1", agentID: "tm"})
	b.turnEvents = &delegator.TurnEvents{TurnID: "tm/c1@1"}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	line := func(id, stop string, out int) []byte {
		s := `{"type":"assistant","timestamp":"` + at + `","message":{"id":"` + id + `","model":"claude-opus-5",` +
			`"usage":{"input_tokens":3,"output_tokens":` + strconv.Itoa(out) + `,"cache_creation_input_tokens":100,` +
			`"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":100}}`
		if stop != "" {
			s += `,"stop_reason":"` + stop + `"`
		}
		return []byte(s + `}}`)
	}

	b.onAssistantLine(line("msg_1", "", 2), "/s.jsonl")           // in flight
	b.onAssistantLine(line("msg_1", "end_turn", 400), "/s.jsonl") // final
	b.onAssistantLine(line("msg_1", "end_turn", 400), "/s.jsonl") // a re-appended copy
	b.turnEvents = nil
	b.onAssistantLine(line("msg_2", "end_turn", 7), "/s.jsonl")

	rows, err := l.Calls(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("calls = %+v, want msg_1 and msg_2 once each", rows)
	}
	if r := rows[0]; r.TurnID != "tm/c1@1" || r.Session != "tm/c1" || r.AgentID != "tm" ||
		r.Count(modelinfo.ClassOutput) != 400 || r.Count(modelinfo.ClassCacheWrite1h) != 100 {
		t.Errorf("msg_1 = %+v, want the final line's counts on the open turn tm/c1@1", r)
	}
	if r := rows[1]; r.TurnID == "tm/c1@1" || !strings.HasPrefix(r.TurnID, "tm/c1@") {
		t.Errorf("msg_2 on turn %q, want a turn of its own in tm/c1", r.TurnID)
	}
}
