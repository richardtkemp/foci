package agent

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"foci/internal/convo"
	"foci/internal/delegator"
)

// A message that folds into the running turn as a steer never becomes a turn
// of its own, so the turn's LogConversationRecv never sees it. #2661: it must
// still get exactly one recv row in conversation.db — written when the fold
// lands, and never when the steer falls back to the channel (the fresh turn
// it becomes records it there).

type steerRecvRow struct {
	Text, UserID, Username, Session string
	ChatID                          int64
}

// openSteerRecvLog opens a conversation log for agentID alone (parallel-safe:
// each test uses its own agent id, #2358) and returns a reader of its recv
// rows.
func openSteerRecvLog(t *testing.T, agentID string) func() []steerRecvRow {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "conversation.db")
	closeLog, err := convo.OpenAgent(agentID, dbPath)
	if err != nil {
		t.Fatalf("OpenAgent: %v", err)
	}
	t.Cleanup(closeLog)
	return func() []steerRecvRow {
		t.Helper()
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("open convo DB: %v", err)
		}
		defer db.Close() // nolint:errcheck
		rows, err := db.Query(`SELECT text, COALESCE(user_id,''), COALESCE(username,''), COALESCE(chat_id,0), session
			FROM messages WHERE direction = 'recv' ORDER BY id`)
		if err != nil {
			t.Fatalf("query recv rows: %v", err)
		}
		defer rows.Close() // nolint:errcheck
		var out []steerRecvRow
		for rows.Next() {
			var r steerRecvRow
			if err := rows.Scan(&r.Text, &r.UserID, &r.Username, &r.ChatID, &r.Session); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, r)
		}
		return out
	}
}

// midTurnAgent returns an agent (worker not started) with steer mode on, a
// turn in flight on sk, and be as the session backend (nil = API mode).
func midTurnAgent(t *testing.T, sk string, be delegator.Delegator) (*Agent, *sessionInbox) {
	t.Helper()
	a := newTestAgent(t)
	a.SetInboxSteerMode(true)
	if be != nil {
		a.SetInboxBackend(func(_ context.Context, _ string) (delegator.Delegator, error) {
			return be, nil
		})
	}
	inb := a.getOrCreateInbox(sk)
	inb.turnActive.Store(true)
	return a, inb
}

func TestSteerRecv_BackendFoldRecordsOneRecvRow(t *testing.T) {
	t.Parallel()
	recv := openSteerRecvLog(t, "steerrecv1")
	be := &recordingBackend{}
	a, _ := midTurnAgent(t, "steerrecv1/c42", be)

	a.Enqueue(Envelope{SessionKey: "steerrecv1/c42", Text: "I mean dotfiles should enable it", UserID: "u1", Username: "dick"})

	if n := len(be.Injects()); n != 1 {
		t.Fatalf("injects = %d, want 1 (premise: the message steered)", n)
	}
	rows := recv()
	if len(rows) != 1 {
		t.Fatalf("recv rows = %d, want 1: %+v", len(rows), rows)
	}
	want := steerRecvRow{Text: "I mean dotfiles should enable it", UserID: "u1", Username: "dick", ChatID: 42, Session: "steerrecv1/c42"}
	if rows[0] != want {
		t.Errorf("recv row = %+v, want %+v", rows[0], want)
	}
}

// The envelope's own ChatID wins over the one in the session key, as on a turn.
func TestSteerRecv_BackendFoldUsesEnvelopeChatID(t *testing.T) {
	t.Parallel()
	recv := openSteerRecvLog(t, "steerrecv2")
	a, _ := midTurnAgent(t, "steerrecv2/c42", &recordingBackend{})

	a.Enqueue(Envelope{SessionKey: "steerrecv2/c42", Text: "x", ChatID: 7})

	rows := recv()
	if len(rows) != 1 || rows[0].ChatID != 7 {
		t.Fatalf("recv rows = %+v, want one row with chat_id 7", rows)
	}
}

func TestSteerRecv_FailedFoldRecordsNothingAtEnqueue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, agentID string
		be            delegator.Delegator
	}{
		{"dispatch failed", "steerrecv3a", &failingSteerBackend{}},
		{"turn ended under it", "steerrecv3b", &errTurnNotInFlightBackend{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := tc.agentID
			recv := openSteerRecvLog(t, id)
			a, inb := midTurnAgent(t, id+"/c1", tc.be)

			a.Enqueue(Envelope{SessionKey: id + "/c1", Text: "falls back"})

			if got := inb.drainAvailable(); len(got) != 1 {
				t.Fatalf("channel = %d envelopes, want 1 (premise: the steer fell back)", len(got))
			}
			if rows := recv(); len(rows) != 0 {
				t.Errorf("recv rows = %+v, want none (the fresh turn records it)", rows)
			}
		})
	}
}

// API mode: a buffered steer is recorded when the turn drains it at a tool
// boundary — not when buffered, and not when it is left over (an orphan
// becomes a follow-up turn, whose LogConversationRecv records it).
func TestSteerRecv_APIBufferRecordsOnMidTurnDrainOnly(t *testing.T) {
	t.Parallel()
	recv := openSteerRecvLog(t, "steerrecv4")
	a, inb := midTurnAgent(t, "steerrecv4/c9", nil)

	a.Enqueue(Envelope{SessionKey: "steerrecv4/c9", Text: "drained mid-turn", UserID: "u", Username: "n"})
	if rows := recv(); len(rows) != 0 {
		t.Fatalf("recv rows after buffering = %+v, want none", rows)
	}
	if got := inb.drainSteerTexts(); len(got) != 1 || got[0] != "drained mid-turn" {
		t.Fatalf("drainSteerTexts = %q (premise: the steer was buffered)", got)
	}
	rows := recv()
	want := steerRecvRow{Text: "drained mid-turn", UserID: "u", Username: "n", ChatID: 9, Session: "steerrecv4/c9"}
	if len(rows) != 1 || rows[0] != want {
		t.Fatalf("recv rows after drain = %+v, want [%+v]", rows, want)
	}

	a.Enqueue(Envelope{SessionKey: "steerrecv4/c9", Text: "orphan"})
	if got := inb.drainSteer(); len(got) != 1 {
		t.Fatalf("orphans = %d, want 1", len(got))
	}
	if rows := recv(); len(rows) != 1 {
		t.Errorf("recv rows after orphan drain = %d, want still 1 (the follow-up turn records the orphan)", len(rows))
	}
}

// A group-chat steer is recorded with its sender's attribution, as a fresh
// turn records it (attributedText) — on both the backend and the API path.
func TestSteerRecv_GroupChatAttribution(t *testing.T) {
	t.Parallel()
	recv := openSteerRecvLog(t, "steerrecv5")
	a, _ := midTurnAgent(t, "steerrecv5/c3", &recordingBackend{})
	a.Enqueue(Envelope{SessionKey: "steerrecv5/c3", Text: "hi", IsGroupChat: true, SenderName: "Eleni"})

	recvAPI := openSteerRecvLog(t, "steerrecv6")
	b, inb := midTurnAgent(t, "steerrecv6/c3", nil)
	b.Enqueue(Envelope{SessionKey: "steerrecv6/c3", Text: "hi", IsGroupChat: true, SenderName: "Eleni"})
	inb.drainSteerTexts()

	for name, rows := range map[string][]steerRecvRow{"backend": recv(), "api": recvAPI()} {
		if len(rows) != 1 || rows[0].Text != "[Eleni] hi" {
			t.Errorf("%s: recv rows = %+v, want one row %q", name, rows, "[Eleni] hi")
		}
	}
}
