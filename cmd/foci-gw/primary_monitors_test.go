package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/fap"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/warnings"
)

// noticeConn records every notification it is asked to send.
type noticeConn struct {
	stubConn
	mu   sync.Mutex
	sent []string
}

func (c *noticeConn) SendNotification(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, text)
}

func (c *noticeConn) notices() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sent...)
}

// swappablePrimaryMgr answers Primary from a connection a test can bring up
// part way through, the way a platform connects after startup.
type swappablePrimaryMgr struct {
	stubConnMgr
	mu   sync.Mutex
	conn platform.Connection
}

func (m *swappablePrimaryMgr) Primary(string) platform.Connection {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conn
}

func (m *swappablePrimaryMgr) set(c platform.Connection) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conn = c
}

// TestChatWarningDispatcher_NoConnectionKeepsWarnings (#2769, audit C4): while
// the agent has no connection a chat warning stays queued, and goes out once
// one exists. It used to be drained and thrown away on a nil Primary.
func TestChatWarningDispatcher_NoConnectionKeepsWarnings(t *testing.T) {
	queue := warnings.NewQueue(0, 0)
	queue.Push("ERROR", "test", "disk on fire")
	mgr := &swappablePrimaryMgr{}
	d := warnings.NewDispatcher(chatWarningConfig(mgr, "a", queue, nil))

	d.MaybeFire()
	if !queue.Pending() {
		t.Fatal("warning was consumed with no connection to send it through")
	}

	conn := &noticeConn{}
	mgr.set(conn)
	d.MaybeFire()

	deadline := time.Now().Add(5 * time.Second)
	for len(conn.notices()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("held warning was not sent once a connection existed")
		}
		time.Sleep(time.Millisecond)
	}
	got := conn.notices()
	if len(got) != 1 || !strings.Contains(got[0], "disk on fire") {
		t.Errorf("notices = %q, want the one held warning", got)
	}
	if !strings.HasPrefix(got[0], "[system diagnostics]\n") {
		t.Errorf("notice %q lost its diagnostics header", got[0])
	}
	if queue.Pending() {
		t.Error("warning still queued after it was sent")
	}
}

// TestDeliverTmuxMemoryNotice covers each answer to "no connection" (#2769,
// audit C5). The notice fires once per threshold crossing, so a skipped one is
// never sent again.
func TestDeliverTmuxMemoryNotice(t *testing.T) {
	const msg = "🔴 tmux memory KILL: killed tmux server"

	t.Run("live connection gets it directly", func(t *testing.T) {
		conn := &noticeConn{}
		mgr := &swappablePrimaryMgr{conn: conn}
		q := warnings.NewQueue(0, 0)
		deliverTmuxMemoryNotice(mgr, "a", q, msg)
		if got := conn.notices(); len(got) != 1 || got[0] != msg {
			t.Errorf("notices = %q, want the bare message", got)
		}
		if q.Pending() {
			t.Error("a delivered notice was also queued")
		}
	})

	t.Run("no connection queues it for the chat dispatcher", func(t *testing.T) {
		q := warnings.NewQueue(0, 0)
		deliverTmuxMemoryNotice(&swappablePrimaryMgr{}, "a", q, msg)
		if got := strings.Join(q.Drain(), "\n"); !strings.Contains(got, msg) {
			t.Errorf("queue = %q, want it to hold the notice", got)
		}
	})

	t.Run("errors-only chat queue still takes it", func(t *testing.T) {
		q := warnings.NewQueue(0, 0)
		q.Configure(true, true, 0, 0)
		deliverTmuxMemoryNotice(&swappablePrimaryMgr{}, "a", q, msg)
		if !q.Pending() {
			t.Error("errors-only queue dropped the notice; it had no such filter when sent directly")
		}
	})

	for name, q := range map[string]*warnings.Queue{"nil queue": nil, "disabled queue": disabledQueue()} {
		t.Run(name+" logs the undelivered notice", func(t *testing.T) {
			var logged []string
			log.SetWarnHook(func(_ log.Level, _, m string) { logged = append(logged, m) })
			t.Cleanup(func() { log.SetWarnHook(nil) })
			deliverTmuxMemoryNotice(&swappablePrimaryMgr{}, "a", q, msg)
			if got := strings.Join(logged, "\n"); !strings.Contains(got, msg) || !strings.Contains(got, "not delivered") {
				t.Errorf("warnings logged = %q, want the notice marked not delivered", got)
			}
		})
	}
}

func disabledQueue() *warnings.Queue {
	q := warnings.NewQueue(0, 0)
	q.Configure(false, false, 0, 0)
	return q
}

// fakeInvokerConn is an app-like connection: it can run device tools.
type fakeInvokerConn struct{ stubConn }

func (c *fakeInvokerConn) InvokeTool(context.Context, string, string, json.RawMessage) (fap.ToolResult, error) {
	return fap.ToolResult{}, nil
}

// allConnsMgr has a chosen Primary and a chosen full connection set.
type allConnsMgr struct {
	stubConnMgr
	primary platform.Connection
	all     []platform.Connection
}

func (m allConnsMgr) Primary(string) platform.Connection       { return m.primary }
func (m allConnsMgr) AllForAgent(string) []platform.Connection { return m.all }

// TestAppInvokerFor (#2769, audit A10): the client tool must find the app's
// connection whichever platform Primary prefers. default_platform or the most
// recently active session can make Primary a non-app connection, which used to
// read as "no device connected" with a device attached.
func TestAppInvokerFor(t *testing.T) {
	tg := &stubConn{}
	app := &fakeInvokerConn{}

	for name, tc := range map[string]struct {
		mgr    allConnsMgr
		wantOK bool
	}{
		"app behind a non-app primary": {allConnsMgr{primary: tg, all: []platform.Connection{tg, app}}, true},
		"app is the primary":           {allConnsMgr{primary: app, all: []platform.Connection{app}}, true},
		"no app connection":            {allConnsMgr{primary: tg, all: []platform.Connection{tg}}, false},
		"no connections":               {allConnsMgr{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			inv, ok := appInvokerFor(tc.mgr, "a")
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && inv != app {
				t.Errorf("invoker = %v, want the app connection", inv)
			}
		})
	}
}
