package main

import (
	"slices"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/usagehold"
)

// TestUsageHoldThreshold_LiveEdit is #2491's hot threshold: the applier for
// usage_hold.threshold sets the process registry, and the next usage event
// uses it — 0.96 holds after an edit to 95.
func TestUsageHoldThreshold_LiveEdit(t *testing.T) {
	if !slices.Contains(liveApplyUsageHoldAddrs, "usage_hold.threshold") {
		t.Fatal("usage_hold.threshold has no live applier")
	}
	t.Cleanup(func() { usagehold.Default.SetThresholdPercent(usagehold.DefaultThresholdPercent) })
	applyUsageHoldThreshold(&config.Config{})
	acct := "test-" + t.Name()
	now := time.Now()
	ev := usagehold.Event{Window: "seven_day", Util: 0.96, HasUtil: true, ResetAt: now.Add(time.Hour)}
	if st := usagehold.Default.Observe(acct, ev, now); st.Phase != usagehold.Open {
		t.Fatalf("0.96 at the default: %v, want open", st.Phase)
	}
	n := 95
	applyUsageHoldThreshold(&config.Config{UsageHold: config.UsageHoldConfig{Threshold: &n}})
	if st := usagehold.Default.Observe(acct, ev, now); st.Phase != usagehold.Hold {
		t.Errorf("0.96 after a live edit to 95: %v, want hold", st.Phase)
	}
}

// TestUsageHoldDriver_IsTheSessionsPlatform: the driver of the usage hold's
// resume line and restored held input reports the session's own platform
// connection, so the turn takes a user turn's trigger and via (#2491 review:
// "simulate a user message"), where httpDriver reports none (an external
// send); and it renders to that connection.
func TestUsageHoldDriver_IsTheSessionsPlatform(t *testing.T) {
	const sk = "a1/c7"
	cm := sessionConnMgr{stubConnMgr: stubConnMgr{agentID: "a1", sessionKey: sk}, conn: &stubConn{sessionKey: sk}}
	d := &usageHoldDriver{agentID: "a1", sessionKey: sk, connMgr: cm}
	conn := d.Connection()
	if conn == nil || conn.PlatformName() != "test" || conn.SessionKey() != sk {
		t.Fatalf("Connection() = %v, want the session's own (test) connection", conn)
	}
	if sink, _ := d.NewTurnSink(agent.Envelope{SessionKey: sk}); sink == nil {
		t.Error("NewTurnSink returned no sink for a session with a connection")
	}
	if (&httpDriver{inst: &agentInstance{id: "a1"}, connMgr: cm}).Connection() != nil {
		t.Error("httpDriver has a connection: the contrast this test draws no longer holds")
	}
	if (&usageHoldDriver{agentID: "a1", sessionKey: sk}).Connection() != nil {
		t.Error("no connection manager: want no connection")
	}
}

// sessionConnMgr is stubConnMgr with one live connection for its session.
type sessionConnMgr struct {
	stubConnMgr
	conn platform.Connection
}

func (m sessionConnMgr) ForSession(sk string) platform.Connection {
	if sk == m.sessionKey {
		return m.conn
	}
	return nil
}
