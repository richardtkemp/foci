package agent

import (
	"context"
	"database/sql"
	"testing"

	"foci/internal/convo"
)

// TestConversationRecordRule_AllTurnSites pins #2094: every site that records
// a turn's rows from its TurnState applies the one which-turns-are-recorded
// rule (nonDeliveredTurnKinds), on both transports. For each trigger it drives
// the prompt (LogConversationRecv, shared by both transports), the API final
// text (LogConversationSent), the API intermediate text (logConversationSent)
// and thinking (logConversationThinking, used by both transports) against a
// real temp conversation DB, then checks the row count and turn_kind tag.
//
// "telegram" is the positive control for an ordinary delivered turn: all
// four rows, untagged. The recorded kinds must produce all four rows tagged,
// so a broken DB setup cannot pass the unrecorded kinds vacuously.
func TestConversationRecordRule_AllTurnSites(t *testing.T) {
	cases := []struct {
		trigger  string
		recorded bool
		kind     string // expected turn_kind on every row when recorded
	}{
		{"telegram", true, ""},
		{"reflection", true, "reflection"},
		{"session_end_memory", true, "session_end_memory"},
		{"background", true, "background"},
		{"consolidation", true, "consolidation"},
		{"branch", true, "branch"},
		{"keepalive", false, ""},
		{"compaction_memory", false, ""},
		{"nudge_extraction", false, ""},
		{"summary", false, ""},
		{"prompt_diff", false, ""},
	}
	transports := []struct {
		name string
		mk   func(a *Agent) TurnContract
	}{
		{"api", func(a *Agent) TurnContract { return &APITransport{sharedTurnOps{agent: a}} }},
		{"delegated", func(a *Agent) TurnContract { return &DelegatedTransport{sharedTurnOps{agent: a}} }},
	}
	for _, tr := range transports {
		for _, tc := range cases {
			t.Run(tr.name+"/"+tc.trigger, func(t *testing.T) {
				dir := t.TempDir()
				agentID := "test"
				dbPath := dir + "/" + agentID + ".db"
				if err := convo.InitPerAgent([]string{agentID}, func(string) string { return dbPath }); err != nil {
					t.Fatalf("InitPerAgent: %v", err)
				}
				defer convo.Close()

				a := &Agent{}
				c := tr.mk(a)
				ts := NewTurnState(context.Background(), agentID+"/c12345", []string{"prompt"}, nil)
				ts.Trigger = tc.trigger
				ts.Meta = &TurnMetadata{UserID: "u1", Username: "dick"}
				ts.FinalText = "final"

				c.LogConversationRecv(ts)
				c.LogConversationSent(ts) // delegated: a no-op by design
				a.logConversationThinking(ts, "thinking")
				// Delegated: recv + thinking (its text is recorded by loggingSink).
				want := 2
				if tr.name == "api" {
					a.logConversationSent(ts, "intermediate")
					want = 4 // recv, final, intermediate, thinking
				}
				if !tc.recorded {
					want = 0
				}

				db, err := sql.Open("sqlite", dbPath)
				if err != nil {
					t.Fatalf("open DB: %v", err)
				}
				defer db.Close()
				rows, err := db.Query("SELECT direction, content_type, text, coalesce(turn_kind,'') FROM messages ORDER BY id")
				if err != nil {
					t.Fatalf("query: %v", err)
				}
				defer rows.Close()
				n := 0
				for rows.Next() {
					var dir, ct, text, kind string
					if err := rows.Scan(&dir, &ct, &text, &kind); err != nil {
						t.Fatalf("scan: %v", err)
					}
					n++
					if kind != tc.kind {
						t.Errorf("row %s/%s %q: turn_kind=%q, want %q", dir, ct, text, kind, tc.kind)
					}
				}
				if err := rows.Err(); err != nil {
					t.Fatalf("rows: %v", err)
				}
				if n != want {
					t.Errorf("trigger=%s: %d rows, want %d", tc.trigger, n, want)
				}
			})
		}
	}
}
