package main

import (
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/platform"
)

// trackingConn wraps stubConn to count SendNotification calls and report a
// fixed platform name, so tests can assert whether a notify fired.
type trackingConn struct {
	*stubConn
	platformName string
	notifyCount  int
}

func (c *trackingConn) PlatformName() string { return c.platformName }
func (c *trackingConn) SendNotification(string) {
	c.notifyCount++
}

// trackingConnMgr returns a fixed set of connections for one agent.
type trackingConnMgr struct {
	stubConnMgr
	agentID string
	conns   []platform.Connection
}

func (m *trackingConnMgr) AllForAgent(agentID string) []platform.Connection {
	if agentID != m.agentID {
		return nil
	}
	return m.conns
}

// TestWireAgentPlatformCallbacks_NotifyReadsLiveConfig proves that
// wireAgentPlatformCallbacks' TaskListNotifyFunc consults the LIVE resolved
// config on each event, not a snapshot frozen at wiring time — a live edit
// to notify.task_list_notify takes effect on the very next notification,
// without reconstructing anything.
func TestWireAgentPlatformCallbacks_NotifyReadsLiveConfig(t *testing.T) {
	conn := &trackingConn{stubConn: &stubConn{sessionKey: "x/c1"}, platformName: "test"}
	connMgr := &trackingConnMgr{agentID: "x", conns: []platform.Connection{conn}}

	acfg := config.AgentConfig{ID: "x"}
	enabledCfg := &config.Config{Platforms: []config.PlatformConfig{
		{ID: "test", Notify: config.NotifyConfig{TaskListNotify: config.Ptr(true)}},
	}}
	live := config.NewLiveValue(config.Resolve(enabledCfg, acfg))

	ag := &agent.Agent{}
	wireAgentPlatformCallbacks(ag, acfg, live, connMgr, nil)

	for _, fn := range ag.TaskListNotifyFunc {
		fn("x/c1", "todo updated")
	}
	if conn.notifyCount != 1 {
		t.Fatalf("notifyCount = %d, want 1 (notify enabled)", conn.notifyCount)
	}

	disabledCfg := &config.Config{Platforms: []config.PlatformConfig{
		{ID: "test", Notify: config.NotifyConfig{TaskListNotify: config.Ptr(false)}},
	}}
	live.Store(config.Resolve(disabledCfg, acfg))

	for _, fn := range ag.TaskListNotifyFunc {
		fn("x/c1", "todo updated again")
	}
	if conn.notifyCount != 1 {
		t.Errorf("notifyCount = %d, want still 1 (live edit disabled notify, callback used a stale snapshot)", conn.notifyCount)
	}
}
