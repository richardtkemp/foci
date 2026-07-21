package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/platform"
)

// recordingConn wraps stubConn (agents_notify_test.go) — reusing its ~25
// no-op Connection methods — and records SetTyping/SendToSession calls under
// a mutex, so a test can observe the SHAPE of async delivery (does a typing
// indicator toggle on/off? is text delivered incrementally or in one shot?)
// without a live Telegram/Discord/app connection. stubConnMgr's
// ForSession/ForSessionOrPrimary are hardcoded nil (it only serves
// AllForAgent, used by the notify tests' broadcast paths), so route.ConnFor
// never resolves a connection through it — recordingConnMgr below fixes
// that so the async-delivery path under test actually reaches a connection.
type recordingConn struct {
	*stubConn
	mu        sync.Mutex
	typingSeq []bool
	sentTexts []string
}

func newRecordingConn(sessionKey string) *recordingConn {
	return &recordingConn{stubConn: &stubConn{sessionKey: sessionKey}}
}

func (c *recordingConn) SetTyping(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.typingSeq = append(c.typingSeq, v)
}

func (c *recordingConn) SendToSession(_ string, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sentTexts = append(c.sentTexts, text)
	return nil
}

// snapshot returns copies of the recorded typing toggles and sent texts.
func (c *recordingConn) snapshot() (typing []bool, texts []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bool(nil), c.typingSeq...), append([]string(nil), c.sentTexts...)
}

// recordingConnMgr is a platform.ConnectionManager whose ForSession actually
// resolves to a connection (unlike stubConnMgr, whose ForSession/
// ForSessionOrPrimary always return nil) — so route.ConnFor, called from the
// async-delivery path, resolves conn instead of DeliveryNone.
type recordingConnMgr struct {
	sessionKey string
	conn       *recordingConn
}

func (m recordingConnMgr) Primary(string) platform.Connection { return m.conn }
func (m recordingConnMgr) AllForAgent(string) []platform.Connection {
	return []platform.Connection{m.conn}
}
func (m recordingConnMgr) ForSession(key string) platform.Connection {
	if key == m.sessionKey {
		return m.conn
	}
	return nil
}
func (m recordingConnMgr) ForSessionOrPrimary(string, string) platform.Connection { return m.conn }
func (m recordingConnMgr) AcquireFacet(string) (platform.Connection, bool)        { return nil, false }
func (m recordingConnMgr) HasFacet(string) bool                                   { return false }
func (m recordingConnMgr) StartAll(context.Context)                               {}
func (m recordingConnMgr) Wait()                                                  {}

// TestSend_AsyncDeliveryTogglesTypingIndicator is the regression test for
// #1385 ("agent response to injected prompts seems to go through a basic
// sink... no activity indicator, no streaming"). Before the fix, the async
// /send path buffered the whole turn behind a bare turnevent.BufferSink
// (internal/agent/turnevent/sinks.go) — which discards every event except
// TurnComplete — then did exactly one flat SendToSession call at the very
// end: SetTyping is never invoked, so this test fails red against the
// pre-fix code (typingSeq stays empty). After the fix, the async chat-
// delivering path attaches a turnSinkForConn-selected sink (the same one
// deliverToSessionChat/agents_notify.go already uses for wakes and
// send_to_session), so typing toggles true→false around the turn.
func TestSend_AsyncDeliveryTogglesTypingIndicator(t *testing.T) {
	d, mock := httpTestSetup(t, httpTestOpts{})
	mock.entered = make(chan string, 1)
	conn := newRecordingConn(testSessionKey)
	d.connMgr = recordingConnMgr{sessionKey: testSessionKey, conn: conn}
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"stream me","async":true}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}

	select {
	case <-mock.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("202 returned but the queued turn never reached the backend")
	}

	// Poll for the turn to finish delivering (typing goes true then false).
	deadline := time.Now().Add(5 * time.Second)
	for {
		typing, texts := conn.snapshot()
		if len(typing) >= 2 && typing[0] && !typing[len(typing)-1] {
			if len(texts) == 0 || !strings.Contains(texts[len(texts)-1], mockReply) {
				t.Fatalf("delivered texts = %v, want one containing %q", texts, mockReply)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("typing indicator never toggled true→false within deadline: typingSeq=%v texts=%v", typing, texts)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
