package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/platform"
	"foci/internal/relogin"
)

// notifyChanConn hands each notification to a channel, for a test that waits on
// delivery from another goroutine.
type notifyChanConn struct {
	*rlConn
	got chan string
}

func (c *notifyChanConn) SendNotification(s string) { c.got <- s }

// notifyChanMgr returns conn as every agent's primary and records who asked.
type notifyChanMgr struct {
	*rlConnMgr
	conn  platform.Connection
	mu    sync.Mutex
	agent string
}

func (m *notifyChanMgr) Primary(agentID string) platform.Connection {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.agent = agentID
	return m.conn
}

func (m *notifyChanMgr) lastAgent() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.agent
}

// TestDeliverLoginExpiryNotice: the notice goes to the primary chat; no chat, or
// a connection that drops agent-wide sends, counts as not delivered.
func TestDeliverLoginExpiryNotice(t *testing.T) {
	t.Parallel()
	const notice = "the sign-in ends"

	conn := newRLConn("app", "clutch/main")
	if !deliverLoginExpiryNotice(&rlConnMgr{primary: conn}, "clutch", notice) {
		t.Error("delivery to a primary reported false")
	}
	if len(conn.notices) != 1 || conn.notices[0] != notice {
		t.Errorf("notices = %q", conn.notices)
	}

	if deliverLoginExpiryNotice(&rlConnMgr{}, "clutch", notice) {
		t.Error("no connection reported as delivered")
	}

	dropper := newRLConn("slack", "")
	dropper.drops = true
	if deliverLoginExpiryNotice(&rlConnMgr{primary: dropper}, "clutch", notice) {
		t.Error("dropped notice reported as delivered")
	}
}

// TestLoginExpiryTarget: the master agent's chat when there is one.
func TestLoginExpiryTarget(t *testing.T) {
	t.Parallel()
	if got := loginExpiryTarget("clutch", "fotini"); got != "clutch" {
		t.Errorf("with a master: %q", got)
	}
	if got := loginExpiryTarget("", "fotini"); got != "fotini" {
		t.Errorf("no master: %q", got)
	}
}

// TestLoginExpiryHook_DeliversOutsideTheCaller: the real hook, from a reported
// date to a notice in the chat, without blocking the caller.
func TestLoginExpiryHook_DeliversOutsideTheCaller(t *testing.T) {
	t.Parallel()
	got := make(chan string, 1)
	conn := &notifyChanConn{rlConn: newRLConn("app", "clutch/main"), got: got}
	mgr := &notifyChanMgr{rlConnMgr: &rlConnMgr{}, conn: conn}
	statePath := filepath.Join(t.TempDir(), "state.json")
	warner := relogin.NewExpiryWarner(statePath)
	hook := loginExpiryHook(warner, mgr, "clutch", "fotini", relogin.G.Pending)

	hook(time.Now().Add(9 * 24 * time.Hour))

	select {
	case n := <-got:
		if !strings.Contains(n, "/login") {
			t.Errorf("notice: %s", n)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no notice delivered")
	}
	if mgr.lastAgent() != "clutch" {
		t.Errorf("delivered via %q, want the master agent", mgr.lastAgent())
	}

	// The record is written after the send: wait for it, as the temp dir goes
	// when the test ends.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(statePath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delivered notice was not recorded")
		}
	}
}

// TestObserveLoginExpiry_SkipsWhileLoginPending: no warning while a sign-in is
// already being asked for, and the stage is still due once it is not.
func TestObserveLoginExpiry_SkipsWhileLoginPending(t *testing.T) {
	t.Parallel()
	conn := newRLConn("app", "clutch/main")
	mgr := &rlConnMgr{primary: conn}
	warner := relogin.NewExpiryWarner(filepath.Join(t.TempDir(), "state.json"))
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	exp := now.Add(9 * 24 * time.Hour)

	observeLoginExpiry(warner, mgr, "clutch", func() bool { return true }, exp, now)
	if len(conn.notices) != 0 {
		t.Fatalf("notice while a login is pending: %q", conn.notices)
	}
	observeLoginExpiry(warner, mgr, "clutch", func() bool { return false }, exp, now)
	if len(conn.notices) != 1 {
		t.Fatalf("notices = %q, want 1 after the login cleared", conn.notices)
	}
}
