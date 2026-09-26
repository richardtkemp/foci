package main

// #2043: a platform still connecting in the background at startup must not
// lose the restart notice or the restart turn.

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	flog "foci/internal/log"
	"foci/internal/platform"
	"foci/internal/startup"
)

// startupConn is a Connection that names its platform and records notices.
type startupConn struct {
	*stubConn
	platform, username string
	notices            []string
}

func (c *startupConn) PlatformName() string      { return c.platform }
func (c *startupConn) Username() string          { return c.username }
func (c *startupConn) SendNotification(s string) { c.notices = append(c.notices, s) }

// restartFixture is one agent on telegram (startup_notify on) whose telegram
// connection has NOT attached yet: the ConnectionManager has nothing live, and
// whenConnected parks callbacks until the test attaches the platform.
func restartFixture(t *testing.T) (agents map[string]*agentInstance, whenConnected func(string, func(platform.Connection)), attach func(platform.Connection)) {
	t.Helper()
	on := true
	cfg := &config.Config{Platforms: []config.PlatformConfig{{ID: "telegram", Notify: config.NotifyConfig{StartupNotify: &on}}}}
	acfg := config.AgentConfig{ID: "a"}
	agents = map[string]*agentInstance{"a": {
		id:        "a",
		agentCfg:  acfg,
		resolved:  config.NewLiveValue(config.Resolve(cfg, acfg)),
		platforms: []string{"telegram"},
	}}
	var mu sync.Mutex
	var parked []func(platform.Connection)
	whenConnected = func(agentID string, fn func(platform.Connection)) {
		mu.Lock()
		defer mu.Unlock()
		if agentID == "a" {
			parked = append(parked, fn)
		}
	}
	attach = func(c platform.Connection) {
		mu.Lock()
		fns := parked
		parked = nil
		mu.Unlock()
		for _, fn := range fns {
			fn(c)
		}
	}
	return agents, whenConnected, attach
}

func crashDiagnosis() *startup.DiagnosisResult {
	return &startup.DiagnosisResult{Class: startup.ClassCrash, Summary: "unexpected restart (gap 10m)"}
}

// The "restarted at" notice goes to a platform when it attaches, not only to
// the platforms that happened to be connected at the notification point.
func TestHandleRestart_NoticeReachesPlatformThatAttachesLate(t *testing.T) {
	agents, whenConnected, attach := restartFixture(t)
	handleRestartAndFirstRun(agents, []string{"a"}, nil, &config.Config{}, context.Background(),
		stubConnMgr{}, whenConnected, crashDiagnosis())

	tg := &startupConn{stubConn: &stubConn{}, platform: "telegram", username: "scoutbot"}
	attach(tg)
	if len(tg.notices) != 1 || !strings.HasPrefix(tg.notices[0], "scoutbot restarted at ") {
		t.Fatalf("notices = %q, want one restart notice on attach", tg.notices)
	}
}

// The restart TURN is gated on the platforms the agent is on, not the ones
// live at that instant: with telegram still connecting, the injection must
// still be attempted (here it stops at "no active session", ag being nil).
func TestHandleRestart_InjectionNotSkippedWhilePlatformConnects(t *testing.T) {
	var buf syncBuf
	flog.SetOutput(&buf)
	flog.SetLevel(flog.DEBUG)
	t.Cleanup(func() { flog.SetOutput(os.Stderr); flog.SetLevel(flog.INFO) })

	agents, whenConnected, _ := restartFixture(t)
	handleRestartAndFirstRun(agents, []string{"a"}, nil, &config.Config{}, context.Background(),
		stubConnMgr{}, whenConnected, crashDiagnosis())

	deadline := time.Now().Add(10 * time.Second) // hang guard; the goroutine logs at once
	for !strings.Contains(buf.String(), "no active session for restart injection") {
		if time.Now().After(deadline) {
			t.Fatalf("restart injection was skipped: a still-connecting platform did not count as one the agent is on\nlog:\n%s", buf.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
