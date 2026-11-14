package agent

import (
	"testing"
	"time"

	"foci/internal/relogin"
)

// The #3104 phase-2 agent tests: each agent consults the login gate of its own
// Claude Code account (LoginGate; nil = relogin.G, the host account's). alpha
// is on the host account; beta and beta2 share account B; gamma is on C. The
// tests drive each agent's login through LoginGateOrDefault, the way the
// gateway does, so code that sent every agent to one gate fails them.

// accountAgents builds started agents alpha (host), beta and beta2 (account
// B) and gamma (account C), all on a Claude Code login.
func accountAgents(t *testing.T) (alpha, beta, beta2, gamma *Agent) {
	t.Helper()
	gateB, gateC := &relogin.Gate{}, &relogin.Gate{}
	mk := func(id string, g *relogin.Gate) *Agent {
		a, cancel := startedAgent(t)
		t.Cleanup(cancel)
		a.AgentID = id
		a.DelegatedManager = &DelegatedManager{}
		onCCLogin(a)
		a.LoginGate = g
		return a
	}
	alpha, beta, beta2, gamma = mk("alpha", nil), mk("beta", gateB), mk("beta2", gateB), mk("gamma", gateC)
	t.Cleanup(func() {
		for _, a := range []*Agent{beta, gamma} {
			a.LoginGateOrDefault().Release()
			a.LoginGateOrDefault().Complete()
		}
	})
	return alpha, beta, beta2, gamma
}

// TestRelogin_PerAccount_HoldsOnlyThatAccount (H3, requirement 3, Q13): while
// account B's login runs, alpha (another account) runs its messages, is not
// unavailable and its background work is not blocked; beta and beta2 (both on
// B) are held, unavailable and blocked. When B's login completes, the held
// messages run.
func TestRelogin_PerAccount_HoldsOnlyThatAccount(t *testing.T) {
	alpha, beta, beta2, _ := accountAgents(t)
	if beta.LoginGateOrDefault() != beta2.LoginGateOrDefault() || beta.LoginGateOrDefault() == alpha.LoginGateOrDefault() {
		t.Fatal("test setup: beta and beta2 must share a gate apart from alpha's")
	}
	if !beta.LoginGateOrDefault().Start() {
		t.Fatal("B's gate already active")
	}
	ds := map[*Agent]*recordingDriver{}
	for _, a := range []*Agent{alpha, beta, beta2} {
		d := &recordingDriver{}
		ds[a] = d
		a.SetTurnObserver(d.recordBatch)
		if !a.Enqueue(Envelope{SessionKey: "app/" + a.AgentID, Text: "hi", Driver: d}) {
			t.Fatalf("%s: message rejected", a.AgentID)
		}
	}
	if !waitFor(time.Second, func() bool { return ds[alpha].NumCalls() == 1 }) {
		t.Fatal("alpha (another account) was held by B's login")
	}
	if why, un := alpha.Unavailable(); un {
		t.Errorf("alpha reported unavailable during B's login: %s", why)
	}
	if blocked, why := alpha.reloginBlocksBackground(); blocked {
		t.Errorf("alpha's background work blocked by B's login: %s", why)
	}
	for _, a := range []*Agent{beta, beta2} {
		if waitFor(200*time.Millisecond, func() bool { return ds[a].NumCalls() > 0 }) {
			t.Errorf("%s (account B) dispatched during B's login", a.AgentID)
		}
		if _, un := a.Unavailable(); !un {
			t.Errorf("%s not unavailable during B's login", a.AgentID)
		}
		if blocked, _ := a.reloginBlocksBackground(); !blocked {
			t.Errorf("%s's background work not blocked during B's login", a.AgentID)
		}
	}
	beta.LoginGateOrDefault().Complete()
	beta.LoginGateOrDefault().Release()
	for _, a := range []*Agent{beta, beta2} {
		if !waitFor(time.Second, func() bool { return ds[a].NumCalls() == 1 }) {
			t.Errorf("%s's held message did not run after B's login completed", a.AgentID)
		}
	}
}

// TestRelogin_PerAccount_NoCrossClear (H4, requirement 4): B's login is owed
// (its one automatic attempt failed). alpha's user turn runs to the end — on
// one shared gate that cleared the pending state — and B's next 401 starts no
// second automatic attempt. B's paused injections stay paused.
func TestRelogin_PerAccount_NoCrossClear(t *testing.T) {
	alpha, beta, _, _ := accountAgents(t)
	gB := beta.LoginGateOrDefault()
	if !gB.StartAuto() {
		t.Fatal("B's first automatic attempt")
	}
	gB.Release() // nobody signed in: B's login is still owed
	ran := make(chan struct{}, 1)
	beta.Enqueue(Envelope{SessionKey: "app/beta", Inject: &InjectMeta{Trigger: "cron", Run: func() { ran <- struct{}{} }}})

	d := &recordingDriver{}
	alpha.SetTurnObserver(d.recordBatch)
	alpha.Enqueue(Envelope{SessionKey: "app/alpha", Text: "hello", Driver: d})
	mustWait(t, "alpha's turn", func() bool { return d.NumCalls() == 1 })
	// The turn's end (ClearPendingIf) runs after the driver returns, on the
	// session's worker. A second message on that worker dispatches only
	// after it, so its dispatch is the event to wait for.
	alpha.Enqueue(Envelope{SessionKey: "app/alpha", Text: "again", Driver: d})
	mustWait(t, "alpha's second turn", func() bool { return d.NumCalls() == 2 })

	if !gB.Pending() {
		t.Fatal("alpha's successful turn cleared account B's pending login")
	}
	if gB.StartAuto() {
		t.Fatal("B's next 401 started a second automatic attempt after alpha's turn (#3006 per account)")
	}
	select {
	case <-ran:
		t.Fatal("B's paused injection ran though B's login is still owed")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestRelogin_PerAccount_CodeGoesToItsLogin (H5, requirement 5): with B's and
// C's logins both waiting for a code, a message in alpha's chat is a normal
// message (not a code, no link); gamma's code completes C's login only, and
// beta's B's.
func TestRelogin_PerAccount_CodeGoesToItsLogin(t *testing.T) {
	alpha, beta, _, gamma := accountAgents(t)
	rec := &linkRecorder{}
	for _, a := range []*Agent{alpha, beta, gamma} {
		a.ReloginLinkTarget = rec.target
	}
	gB, gC := beta.LoginGateOrDefault(), gamma.LoginGateOrDefault()
	if !gB.Start() || !gC.Start() {
		t.Fatal("start B and C")
	}
	gB.OpenCapture("beta", "app/beta", "LINK-B")
	gC.OpenCapture("gamma", "app/gamma", "LINK-C")

	d := &recordingDriver{}
	alpha.SetTurnObserver(d.recordBatch)
	alpha.Enqueue(Envelope{SessionKey: "app/alpha", Text: "just chatting", Driver: d})
	mustWait(t, "alpha's normal turn", func() bool { return d.NumCalls() == 1 })
	if got := rec.sentTo("app/alpha"); len(got) != 0 {
		t.Fatalf("alpha's chat got a sign-in link: %q", got)
	}

	if !gamma.Enqueue(Envelope{SessionKey: "app/gamma", Text: "code-C"}) {
		t.Fatal("gamma's code not consumed")
	}
	if code, ok := gC.AwaitCode(time.Second); !ok || code != "code-C" {
		t.Fatalf("C's login got %q,%v; want code-C", code, ok)
	}
	if code, ok := gB.AwaitCode(50 * time.Millisecond); ok {
		t.Fatalf("B's login took gamma's code %q", code)
	}
	if !beta.Enqueue(Envelope{SessionKey: "app/beta", Text: "code-B"}) {
		t.Fatal("beta's code not consumed")
	}
	if code, ok := gB.AwaitCode(time.Second); !ok || code != "code-B" {
		t.Fatalf("B's login got %q,%v; want code-B", code, ok)
	}
}
