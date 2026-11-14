package relogin

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The #3104 phase-2 relogin tests: one gate, pane and expiry warner per
// Claude Code account, and messages that name the account.

// TestTmuxPane_UnsetsBareNames (H1, H2): the login pane exports each KEY=VALUE
// and unsets a bare KEY, so an agent with its own folder does not inherit the
// tmux server's empty CLAUDE_SECURESTORAGE_CONFIG_DIR.
func TestTmuxPane_UnsetsBareNames(t *testing.T) {
	var args []string
	p := &tmuxPane{
		session: "foci-cc-login-abcd",
		binary:  "/bin/claude",
		env:     []string{"CLAUDE_CONFIG_DIR=/x", "CLAUDE_SECURESTORAGE_CONFIG_DIR"},
		exec: func(_ context.Context, a ...string) (string, error) {
			args = a
			return "", nil
		},
	}
	if err := p.create(context.Background()); err != nil {
		t.Fatal(err)
	}
	cmd := args[len(args)-1]
	for _, want := range []string{"export '\\''CLAUDE_CONFIG_DIR=/x'\\''", "unset '\\''CLAUDE_SECURESTORAGE_CONFIG_DIR'\\''", "/bin/claude"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("pane command %q lacks %q", cmd, want)
		}
	}
	if strings.Contains(cmd, "export '\\''CLAUDE_SECURESTORAGE_CONFIG_DIR") {
		t.Errorf("pane command exports the secure-storage var: %q", cmd)
	}
	if args[3] != "foci-cc-login-abcd" {
		t.Errorf("tmux session %q, want the account's own", args[3])
	}
}

// TestRun_SessionPerAccount: a Config with a Session gets that tmux session;
// none gets DefaultSession, the host account's, as before.
func TestRun_SessionPerAccount(t *testing.T) {
	for _, c := range []struct{ session, want string }{{"", DefaultSession}, {"foci-cc-login-1234", "foci-cc-login-1234"}} {
		cfg := Config{Session: c.session}
		cfg.defaults()
		pn, ok := cfg.newPane(&cfg).(*tmuxPane)
		if !ok || pn.session != c.want {
			t.Errorf("Session %q: pane session %v, want %q", c.session, pn, c.want)
		}
	}
}

// TestRun_MessagesNameTheAccount (requirement 5, Q14/Q15): the link, the
// completion and the failure notices name the account, by email once known,
// else by its location.
func TestRun_MessagesNameTheAccount(t *testing.T) {
	g := &Gate{}
	g.SetName("/tmp/ccB")
	if g.Name() != "/tmp/ccB" {
		t.Fatalf("Name %q, want the location", g.Name())
	}
	g.SetEmail("b@example.com")
	g.SetEmail("")
	if g.Name() != "b@example.com" {
		t.Fatalf("Name %q, want the email", g.Name())
	}
	if !g.Start() {
		t.Fatal("Start")
	}
	urlScreen := "Use the url below to sign in\nhttps://claude.ai/oauth?code=1&state=2\nPaste code here if prompted"
	fp := &fakePane{captures: []string{urlScreen, "Login successful"}}
	var msgs []string
	var mu sync.Mutex
	cfg := newTestConfig(g, fp, &msgs, &mu)
	done := make(chan struct{})
	go func() { Run(context.Background(), cfg); close(done) }()
	waitUntil(t, func() bool { return g.ShouldCapture("clutch", "test/s") })
	g.SubmitCode("code")
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(msgs) < 2 {
		t.Fatalf("messages %q", msgs)
	}
	for _, m := range msgs {
		if !strings.Contains(m, "(account: b@example.com)") {
			t.Errorf("message does not name the account: %q", m)
		}
	}
	if !strings.Contains(failText("x", ""), "failed: x") || strings.Contains(failText("x", ""), "account") {
		t.Errorf("a nameless failure notice changed: %q", failText("x", ""))
	}
	if !strings.Contains(failText("x", "b@example.com"), "(account: b@example.com)") {
		t.Errorf("failure notice does not name the account: %q", failText("x", "b@example.com"))
	}
}

// TestGates_Independent (requirements 3, 4): two accounts' gates share
// nothing. B's running login holds only B; A's successful turn
// (ClearPendingIf with A's attempt) and A's completed login leave B's pending
// state, attempt number and one-attempt-per-expiry refusal alone.
func TestGates_Independent(t *testing.T) {
	a, b := &Gate{}, &Gate{}
	if !b.StartAuto() {
		t.Fatal("B's first automatic attempt")
	}
	if a.Active() || a.Pending() {
		t.Fatal("B's login shows on A's gate")
	}
	b.Release() // B's attempt failed: its login is still owed
	attemptB, _ := b.PendingAttempt()

	attemptA, _ := a.PendingAttempt()
	a.ClearPendingIf(attemptA)
	if !a.Start() {
		t.Fatal("A's /login")
	}
	a.Complete()
	a.Release()

	if got, pending := b.PendingAttempt(); !pending || got != attemptB {
		t.Fatalf("B after A's turn and login: attempt %d pending %v, want %d true", got, pending, attemptB)
	}
	if b.StartAuto() {
		t.Error("B's next 401 started a second automatic attempt after A's turn")
	}
}

// TestExpiryWarner_NamesAccountAndKeepsPerAccountState (H8, requirement 7):
// two accounts' warners keep separate files; B's earlier date is warned (not
// dropped as "the previous sign-in" as one shared warner did), A's stage is
// not reset by B, and each notice names its account.
func TestExpiryWarner_NamesAccountAndKeepsPerAccountState(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for _, order := range []string{"AB", "BA"} {
		wa := NewExpiryWarner(filepath.Join(dir, order+"-a.json"))
		wb := NewExpiryWarner(filepath.Join(dir, order+"-b.json"))
		wa.SetAccountName(func() string { return "a@example.com" })
		wb.SetAccountName(func() string { return "/tmp/ccB" })
		var sent []string
		send := func(n string) bool { sent = append(sent, n); return true }
		obsA := func() { wa.Observe(now.Add(9*24*time.Hour), now, send) }
		obsB := func() { wb.Observe(now.Add(2*24*time.Hour), now, send) }
		if order == "AB" {
			obsA()
			obsB()
		} else {
			obsB()
			obsA()
		}
		if len(sent) != 2 {
			t.Fatalf("%s: notices %q, want one per account", order, sent)
		}
		var gotA, gotB bool
		for _, n := range sent {
			gotA = gotA || strings.Contains(n, "(account: a@example.com)")
			gotB = gotB || strings.Contains(n, "(account: /tmp/ccB)")
		}
		if !gotA || !gotB {
			t.Errorf("%s: notices do not name their accounts: %q", order, sent)
		}
		// Neither account's report resets the other's stage.
		sent = nil
		obsA()
		obsB()
		if len(sent) != 0 {
			t.Errorf("%s: repeat notices %q", order, sent)
		}
	}
}
