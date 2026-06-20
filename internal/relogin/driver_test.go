package relogin

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakePane struct {
	mu       sync.Mutex
	created  bool
	killed   bool
	lines    []string // text passed to sendLine
	enters   int
	captures []string // scripted capture outputs, consumed in order (last repeats)
	capIdx   int
}

func (f *fakePane) create(context.Context) error { f.created = true; return nil }
func (f *fakePane) sendLine(_ context.Context, text string) error {
	f.mu.Lock()
	f.lines = append(f.lines, text)
	f.mu.Unlock()
	return nil
}
func (f *fakePane) enter(context.Context) error { f.enters++; return nil }
func (f *fakePane) capture(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.capIdx < len(f.captures) {
		s := f.captures[f.capIdx]
		f.capIdx++
		return s, nil
	}
	if len(f.captures) > 0 {
		return f.captures[len(f.captures)-1], nil
	}
	return "", nil
}
func (f *fakePane) kill(context.Context) error { f.killed = true; return nil }

func (f *fakePane) sentLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...)
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func newTestConfig(g *Gate, fp *fakePane, msgs *[]string, mu *sync.Mutex) Config {
	return Config{
		AgentID: "clutch",
		Gate:    g,
		SendMessage: func(text string) error {
			mu.Lock()
			*msgs = append(*msgs, text)
			mu.Unlock()
			return nil
		},
		AnchorTimeout: 50 * time.Millisecond,
		CodeTimeout:   500 * time.Millisecond,
		newPane:       func(*Config) pane { return fp },
		sleep:         func(time.Duration) {},
	}
}

func TestRunHappyPath(t *testing.T) {
	g := &Gate{}
	if !g.Start() {
		t.Fatal("Start")
	}
	urlScreen := "Use the url below to sign in\nhttps://claude.ai/oauth?code=1&state=2\nPaste code here if prompted"
	successScreen := "Login successful! You are now logged in."
	fp := &fakePane{captures: []string{urlScreen, successScreen}}
	var msgs []string
	var mu sync.Mutex
	cfg := newTestConfig(g, fp, &msgs, &mu)

	done := make(chan struct{})
	go func() { Run(context.Background(), cfg); close(done) }()

	waitUntil(t, func() bool { return g.ShouldCapture("clutch") })
	g.SubmitCode("logincode")
	<-done

	if !fp.created {
		t.Error("pane was not created")
	}
	if !fp.killed {
		t.Error("pane was not killed")
	}
	if g.Active() {
		t.Error("gate should be released after a completed login")
	}

	lines := fp.sentLines()
	if len(lines) < 2 || lines[0] != "/login" || lines[len(lines)-1] != "logincode" {
		t.Errorf("expected /login then logincode, got %v", lines)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(msgs) == 0 || msgs[len(msgs)-1] != "✅ Login completed." {
		t.Errorf("expected success message last, got %v", msgs)
	}
	// The URL should have been relayed to the user.
	foundURL := false
	for _, m := range msgs {
		if strings.Contains(m, "https://claude.ai/oauth?code=1&state=2") {
			foundURL = true
		}
	}
	if !foundURL {
		t.Errorf("login URL not relayed; msgs=%v", msgs)
	}
}

func TestRunCodeTimeoutReleasesGate(t *testing.T) {
	g := &Gate{}
	g.Start()
	urlScreen := "Use the url below to sign in\nhttps://claude.ai/x\nPaste code here if prompted"
	fp := &fakePane{captures: []string{urlScreen}}
	var msgs []string
	var mu sync.Mutex
	cfg := newTestConfig(g, fp, &msgs, &mu)
	cfg.CodeTimeout = 20 * time.Millisecond // never submit a code

	Run(context.Background(), cfg)

	if g.Active() {
		t.Error("gate must be released even when the login times out")
	}
	if !fp.killed {
		t.Error("pane must be killed on the timeout path")
	}
	mu.Lock()
	defer mu.Unlock()
	if !anyContains(msgs, "failed") {
		t.Errorf("expected a failure message, got %v", msgs)
	}
}

func TestRunNoURLReleasesGate(t *testing.T) {
	g := &Gate{}
	g.Start()
	// Pane never shows the paste anchor → waitFor times out.
	fp := &fakePane{captures: []string{"loading...\nstill loading"}}
	var msgs []string
	var mu sync.Mutex
	cfg := newTestConfig(g, fp, &msgs, &mu)

	Run(context.Background(), cfg)

	if g.Active() {
		t.Error("gate must be released when the login prompt never appears")
	}
	mu.Lock()
	defer mu.Unlock()
	if !anyContains(msgs, "failed") {
		t.Errorf("expected a failure message, got %v", msgs)
	}
}

func anyContains(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
