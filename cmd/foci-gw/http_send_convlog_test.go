package main

import (
	"context"
	"database/sql"
	"testing"

	"foci/internal/agent"
	"foci/internal/agent/turnevent"
	"foci/internal/convo"
	"foci/internal/platform"

	_ "modernc.org/sqlite"
)

// #1784: injected turns (async /send, wakes, restart changelogs,
// send_to_session) build their delivery sink via turnSinkForConn rather than
// Agent.RunTurn, which is where platform turns pick up the conversation-DB
// wrapper. Before the fix those replies streamed to the user and were never
// persisted, so conversation.db silently omitted every cron- and
// system-originated reply.
//
// The existing TestSend_AsyncDeliveryTogglesTypingIndicator drives the SAME
// path and asserts only delivery; it is the control for the tests here. Break
// the logging wrapper and it must stay green — that is what makes these
// specific to the persistence defect rather than to the fixture.

// initTestConvo points the conversation store at a temp DB for testAgentID and
// returns a reader for the outbound rows recorded against it.
func initTestConvo(t *testing.T) func() []string {
	t.Helper()
	dir := t.TempDir()
	if err := convo.InitPerAgent([]string{testAgentID}, func(id string) string {
		return dir + "/" + id + ".db"
	}); err != nil {
		t.Fatalf("InitPerAgent: %v", err)
	}
	t.Cleanup(convo.Close)

	return func() []string {
		db, err := sql.Open("sqlite", dir+"/"+testAgentID+".db")
		if err != nil {
			t.Fatalf("open conv DB: %v", err)
		}
		defer db.Close()
		rows, err := db.Query("SELECT text FROM messages WHERE direction='sent' ORDER BY id")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, s)
		}
		return out
	}
}

// NOT COVERED HERE: an end-to-end POST /send async assertion. It was written,
// and it passes with the wrapper removed, so it guards nothing. httpTestSetup
// builds an Agent with an API Client, and on the API transport
// sharedTurnOps.LogConversationSent records FinalText unconditionally — the
// row appears via that route no matter what turnSinkForConn returns. The defect
// exists only on the DELEGATED transport, where LogConversationSent is
// overridden to a no-op (turn_delegated.go). Reproducing it end-to-end needs a
// delegated-backend harness; the choke-point test below covers the same fix
// specifically, and proves it by failing when the wrapper is removed.

// driverConn is a platform.Connection that also satisfies agent.Driver, so
// turnSinkForConn takes its Driver branch (what the app uses in production)
// rather than the SessionSink fallback the other tests exercise.
type driverConn struct {
	*stubConn
	inner *turnevent.BufferSink
}

func (c *driverConn) WrapTurn(_ context.Context, fn func() error) error { return fn() }
func (c *driverConn) NewTurnSink(_ agent.Envelope) (turnevent.Sink, func()) {
	return c.inner, nil
}
func (c *driverConn) Connection() platform.Connection { return c }

// TestTurnSinkForConn_LogsOnBothBranches pins the choke point itself: BOTH the
// Driver branch (app) and the SessionSink fallback (Telegram/Discord) must
// return a logging sink. Covering the fallback alone would leave the branch the
// reported bug was actually observed on untested.
func TestTurnSinkForConn_LogsOnBothBranches(t *testing.T) {
	for _, tc := range []struct {
		name string
		conn platform.Connection
	}{
		{"driver branch (app)", &driverConn{stubConn: &stubConn{sessionKey: testSessionKey}, inner: turnevent.NewBufferSink()}},
		{"session-sink fallback (telegram/discord)", newRecordingConn(testSessionKey)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sentRows := initTestConvo(t)
			ag := &agent.Agent{}

			sink, cleanup := turnSinkForConn(ag, tc.conn, testSessionKey, "test")
			if cleanup != nil {
				defer cleanup()
			}
			if sink == nil {
				t.Fatal("premise failed: turnSinkForConn returned no sink")
			}
			sink.Emit(context.Background(), turnevent.TextBlock{
				Text:  "persist me",
				Phase: turnevent.PhaseIntermediate,
			})

			got := sentRows()
			if len(got) != 1 || got[0] != "persist me" {
				t.Fatalf("sent rows = %v, want exactly [persist me] (#1784)", got)
			}
		})
	}
}

// TestBroadcastResponse_LogsSentText covers path C. The broadcast fan-out runs
// behind a BufferSink that emits no TextBlock, so it cannot be covered by the
// sink wrapper and records explicitly instead.
func TestBroadcastResponse_LogsSentText(t *testing.T) {
	sentRows := initTestConvo(t)

	conn := newRecordingConn(testSessionKey)
	cm := recordingConnMgr{sessionKey: testSessionKey, conn: conn}
	broadcastResponse(cm, testAgentID, testSessionKey, "broadcast me", "test")

	got := sentRows()
	if len(got) != 1 || got[0] != "broadcast me" {
		t.Fatalf("sent rows = %v, want exactly [broadcast me] (#1784)", got)
	}
}
