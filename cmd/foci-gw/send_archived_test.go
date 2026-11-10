package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/defersend"
	"foci/internal/session"
	"foci/internal/timeutil"
	"foci/internal/tools"
)

// send_to_session to a session whose app conversation is archived (#3024,
// Dick 2026-11-10: "Refuse send to archived chats, calling agent gets told the
// target has been archived").

// archivedSendTool builds send_to_session over the real gateway notifier, with
// scout/c42 an app conversation, and returns the tool, the caller's context
// and a count of reply_to=session deliveries.
func archivedSendTool(t *testing.T, f *chitFixture) (*tools.Tool, context.Context, *int) {
	t.Helper()
	if err := f.target.SessionIndex.SetChatMetadata("scout", "app", 42, "conv_id", "conv-42"); err != nil {
		t.Fatal(err)
	}
	n := newAsyncNotifier(func() *agent.Agent { return f.caller }, "clutch", f.resolver, f.ctx, f.cm, f.rec.deliver)
	sessionSends := new(int)
	tool := tools.NewSendToSessionTool(nil, n, func(string, string, *tools.AgentOrigin) { *sessionSends++ }, nil, nil, nil)
	return tool, tools.WithSessionKey(context.Background(), "clutch/c1"), sessionSends
}

func setArchived(t *testing.T, f *chitFixture, archived bool) {
	t.Helper()
	if err := f.target.SessionIndex.SetArchivedChat("scout", "app", 42, archived); err != nil {
		t.Fatal(err)
	}
}

func sendParams(replyTo string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"session_key": "scout/c42", "message": "hello?", "reply_to": replyTo})
	return b
}

// TestSendToSession_ArchivedRefusedNoTurn: both reply_to modes are refused
// with an error that names the session and says it is archived, and the
// target runs no turn.
func TestSendToSession_ArchivedRefusedNoTurn(t *testing.T) {
	f := newChitFixture(t)
	tool, ctx, sessionSends := archivedSendTool(t, f)
	setArchived(t, f, true)

	for _, replyTo := range []string{"caller", "session"} {
		_, err := tool.Execute(ctx, sendParams(replyTo))
		if err == nil {
			t.Fatalf("reply_to=%s: send to an archived session succeeded", replyTo)
		}
		if !errors.Is(err, session.ErrSessionArchived) {
			t.Errorf("reply_to=%s: error %v is not ErrSessionArchived", replyTo, err)
		}
		if msg := err.Error(); !strings.Contains(msg, "session scout/c42 has been archived") {
			t.Errorf("reply_to=%s: error %q does not name the archived session", replyTo, msg)
		}
	}
	time.Sleep(300 * time.Millisecond)
	f.targetCl.mu.Lock()
	calls := f.targetCl.calls
	f.targetCl.mu.Unlock()
	if calls != 0 {
		t.Errorf("the archived target ran %d turn(s)", calls)
	}
	if *sessionSends != 0 {
		t.Errorf("reply_to=session delivered %d time(s) to the archived session", *sessionSends)
	}
	if got := f.rec.snapshot(); len(got) != 0 {
		t.Errorf("chits %v surfaced for a refused send", got)
	}
}

// TestSendToSession_UnarchivedDelivered: the same session, unarchived, takes
// the message and runs its turn.
func TestSendToSession_UnarchivedDelivered(t *testing.T) {
	f := newChitFixture(t)
	tool, ctx, _ := archivedSendTool(t, f)
	setArchived(t, f, true)
	setArchived(t, f, false)

	res, err := tool.Execute(ctx, sendParams("caller"))
	if err != nil {
		t.Fatalf("send to an unarchived session failed: %v", err)
	}
	if !strings.Contains(res.Text, "Message sent to session scout/c42") {
		t.Errorf("result %q, want a sent receipt", res.Text)
	}
	if !waitCond(10*time.Second, func() bool {
		f.targetCl.mu.Lock()
		defer f.targetCl.mu.Unlock()
		return f.targetCl.calls > 0
	}) {
		t.Fatal("the unarchived target ran no turn")
	}
}

// archivedHTTPChat makes testAgentID/c42 an app conversation in d's index,
// archived as asked.
func archivedHTTPChat(t *testing.T, d httpHandlerDeps, archived bool) string {
	t.Helper()
	const key = testAgentID + "/c42"
	d.sessionIndex.Upsert(session.SessionIndexEntry{SessionKey: key, FilePath: "x", SessionType: session.SessionTypeChat, Status: session.SessionStatusActive})
	if err := d.sessionIndex.SetChatMetadata(testAgentID, "app", 42, "conv_id", "conv-42"); err != nil {
		t.Fatal(err)
	}
	if err := d.sessionIndex.SetArchivedChat(testAgentID, "app", 42, archived); err != nil {
		t.Fatal(err)
	}
	return key
}

// TestHTTP_ArchivedSessionRefused: every HTTP entry point that names a target
// session (resolveTargetSession) refuses an archived one with 409 and the
// archived error, and no turn runs; unarchived, the same request runs.
func TestHTTP_ArchivedSessionRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ path, body string }{
		{"/send", `{"text":"hello","session":"c42","wait_none":true}`},
		{"/branch", `{"text":"branch work","session":"c42"}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			d, mock := httpTestSetup(t, httpTestOpts{})
			key := archivedHTTPChat(t, d, true)
			mux := newTestMux(d)

			w := postJSON(mux, tc.path, tc.body)
			if w.Code != 409 {
				t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "session "+key+" has been archived") {
				t.Errorf("body %q does not name the archived session", w.Body.String())
			}
			time.Sleep(200 * time.Millisecond)
			if calls := mock.snapshot(); len(calls) != 0 {
				t.Errorf("an archived session ran %d turn(s)", len(calls))
			}

			archivedHTTPChat(t, d, false)
			w = postJSON(mux, tc.path, tc.body)
			if w.Code != 200 {
				t.Fatalf("unarchived: status = %d, want 200; body: %s", w.Code, w.Body.String())
			}
			if !waitCond(5*time.Second, func() bool { return len(mock.snapshot()) == 1 }) {
				t.Errorf("unarchived: %d turn(s), want 1", len(mock.snapshot()))
			}
		})
	}
}

// TestSweep_DropsDeferredSendToArchivedSession: a send queued while its chat
// was live, then archived, is dropped at delivery with no turn; a send to a
// live chat in the same sweep is still delivered.
func TestSweep_DropsDeferredSendToArchivedSession(t *testing.T) {
	t.Parallel()
	d, mock := httpTestSetup(t, httpTestOpts{})
	mock.entered = make(chan string, 2)
	store := withDeferStore(t, &d)
	key := archivedHTTPChat(t, d, false)
	now := timeutil.Now()
	_, _, _ = store.Enqueue(defersend.Record{
		AgentID: testAgentID, SessionKey: key, Text: "to the archived chat", Policy: "fallback",
		WaitCold: "1m", CreatedAt: now, DeadlineAt: now.Add(time.Hour),
	})
	archivedHTTPChat(t, d, true)
	isU, isS := buildActivityCheckers(d)
	sw := &deferSweeper{store: store, deps: d, isUserActive: isU, isSessionActive: isS}
	sw.sweep()

	if all, _ := store.All(); len(all) != 0 {
		t.Errorf("queued=%d after the sweep, want the archived send dropped", len(all))
	}
	select {
	case text := <-mock.entered:
		t.Fatalf("delivered %q to an archived session", text)
	case <-time.After(300 * time.Millisecond):
	}

	_, _, _ = store.Enqueue(defersend.Record{
		AgentID: testAgentID, SessionKey: testSessionKey, Text: "to a live chat", Policy: "fallback",
		WaitCold: "1m", CreatedAt: now, DeadlineAt: now.Add(time.Hour),
	})
	sw.sweep()
	select {
	case text := <-mock.entered:
		if !strings.HasSuffix(text, "to a live chat") {
			t.Errorf("delivered %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a send to a live session was not delivered")
	}
}
