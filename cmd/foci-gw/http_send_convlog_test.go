package main

import (
	"context"
	"database/sql"
	"testing"

	"foci/internal/agent"
	"foci/internal/convo"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/turnevent"

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

// initTestConvo opens a conversation log in a temp DB for an agent id unique
// to this run (convo.OpenAgent, beside any other test's agent logs) and
// returns a session key of that agent plus a reader for the outbound rows
// recorded against it. The store routes rows by the session key's agent and
// OpenAgent sets no fallback, so only this run's rows reach this DB and the
// test can run in parallel (#2358).
func initTestConvo(t *testing.T) (sessionKey string, sentRows func() []string) {
	t.Helper()
	agentID := runToken("convo")
	path := t.TempDir() + "/" + agentID + ".db"
	closeLog, err := convo.OpenAgent(agentID, path)
	if err != nil {
		t.Fatalf("OpenAgent: %v", err)
	}
	t.Cleanup(closeLog)

	return agentID + "/i0", func() []string {
		db, err := sql.Open("sqlite", path)
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
	t.Parallel()
	for _, tc := range []struct {
		name string
		conn func(sessionKey string) platform.Connection
	}{
		{"driver branch (app)", func(sk string) platform.Connection {
			return &driverConn{stubConn: &stubConn{sessionKey: sk}, inner: turnevent.NewBufferSink()}
		}},
		{"session-sink fallback (telegram/discord)", func(sk string) platform.Connection { return newRecordingConn(sk) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionKey, sentRows := initTestConvo(t)
			ag := &agent.Agent{}

			sink, cleanup := turnSinkForConn(ag, tc.conn(sessionKey), sessionKey, "test")
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
	t.Parallel()
	sessionKey, sentRows := initTestConvo(t)

	conn := newRecordingConn(sessionKey)
	cm := recordingConnMgr{sessionKey: sessionKey, conn: conn}
	broadcastResponse(cm, session.AgentIDFromKey(sessionKey), sessionKey, "broadcast me", "test")

	got := sentRows()
	if len(got) != 1 || got[0] != "broadcast me" {
		t.Fatalf("sent rows = %v, want exactly [broadcast me] (#1784)", got)
	}
}
