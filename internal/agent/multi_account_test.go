package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/account"
	"foci/internal/delegator"
	"foci/internal/ratelimit"
	"foci/internal/relogin"
	"foci/internal/session"
	"foci/internal/usagehold"
)

// The #3186 phase-2 agent tests: ONE agent with two accounts, A and B. The
// session "multi/c1" is stored on A and "multi/c2" on B.

const (
	multiOnA = "multi/c1"
	multiOnB = "multi/c2"
)

// bindingMap is an account.BindingStore in memory.
type bindingMap struct {
	mu sync.Mutex
	m  map[string]string
}

func (b *bindingMap) GetSessionMetadata(key, name string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.m[key+"\x00"+name], nil
}

func (b *bindingMap) SetSessionMetadata(key, name, value string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[key+"\x00"+name] = value
	return nil
}

// twoAccountAgent puts a (a delegated agent) on a pool of two Claude Code
// accounts over reg, with multiOnA stored on A and multiOnB on B.
func twoAccountAgent(a *Agent, reg *usagehold.Registry) (accA, accB *account.Account) {
	mk := func(key string) *account.Account {
		return account.New(account.Options{Backend: "claude-code", Holds: reg, Login: &relogin.Gate{},
			Spec: delegator.AccountSpec{Key: key, Name: "/cc/" + key}})
	}
	accA, accB = mk("A"), mk("B")
	store := &bindingMap{m: map[string]string{
		multiOnA + "\x00" + session.MetaKeyAccount: "A",
		multiOnB + "\x00" + session.MetaKeyAccount: "B",
	}}
	a.DelegatedManager.Accounts = account.NewPool(store, accA, accB)
	return accA, accB
}

// TestMultiAccount_HoldOnOneAccountHoldsOnlyItsSessions (#3186 F6, F9): a
// hold on A holds the agent's sessions on A, their forks included; its
// sessions on B keep running, their periodic work, keepalive and spawns
// included.
func TestMultiAccount_HoldOnOneAccountHoldsOnlyItsSessions(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.AgentID = "multi"
	a.DelegatedManager = &DelegatedManager{BackendType: "claude-code"}
	reg := usagehold.New()
	accA, _ := twoAccountAgent(a, reg)
	// The children are forks (#3246: a fork is on its source's account).
	for _, c := range [][2]string{{multiOnA + "/b1709596800", multiOnA}, {multiOnB + "/i1709596801", multiOnB}} {
		if _, err := a.Accounts().Inherit(c[0], c[1]); err != nil {
			t.Fatal(err)
		}
	}
	a.StartUsageHold()
	defer a.StopUsageHold()
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)

	accA.ObserveUsage(usagehold.Event{Window: "seven_day", Reached: true, ResetAt: time.Now().Add(time.Hour)})
	a.Enqueue(Envelope{SessionKey: multiOnA, Text: "on A", Driver: d})
	a.Enqueue(Envelope{SessionKey: multiOnB, Text: "on B", Driver: d})
	if !waitFor(2*time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("turns run = %d, want B's message only", d.NumCalls())
	}
	if waitFor(200*time.Millisecond, func() bool { return d.NumCalls() > 1 }) {
		t.Errorf("A's message ran during A's stop")
	}
	for _, c := range []struct {
		sk   string
		held bool
	}{{multiOnA, true}, {multiOnA + "/b1709596800", true}, {multiOnB, false}, {multiOnB + "/i1709596801", false}} {
		if blocked, _ := a.UsageHoldBlocks(c.sk, true); blocked != c.held {
			t.Errorf("keepalive on %s blocked = %v, want %v", c.sk, blocked, c.held)
		}
		if blocked, _ := a.UsageHoldBlocks(c.sk, false); blocked != c.held {
			t.Errorf("periodic work on %s blocked = %v, want %v", c.sk, blocked, c.held)
		}
		if refusal := a.UsageHoldRefusal(c.sk); (refusal != "") != c.held {
			t.Errorf("spawn refusal on %s = %q, want held=%v", c.sk, refusal, c.held)
		}
	}
}

// TestMultiAccount_RateLimitGatePerAccount (#3186 F6): a usage limit engaged
// on A's gate limits the sessions on A, not those on B.
func TestMultiAccount_RateLimitGatePerAccount(t *testing.T) {
	a := holdAgent(usagehold.New(), "claude-code")
	a.AgentID = "multi"
	accA, _ := twoAccountAgent(a, usagehold.New())
	a.EngageRateLimit(accA, ratelimit.Signal{Kind: ratelimit.KindUsage, ResetAt: time.Now().Add(time.Hour)})
	if lim, _ := a.SessionRateLimited(multiOnA); !lim {
		t.Error("session on A not rate-limited")
	}
	if lim, reason := a.SessionRateLimited(multiOnB); lim {
		t.Errorf("session on B rate-limited by A's limit: %s", reason)
	}
}

// TestMultiAccount_LoginPerAccount (#3186 F6): a login owed on A is the
// login of A's sessions only.
func TestMultiAccount_LoginPerAccount(t *testing.T) {
	a := holdAgent(usagehold.New(), "claude-code")
	a.AgentID = "multi"
	accA, accB := twoAccountAgent(a, usagehold.New())
	accA.Login().StartAuto()
	if a.AccountFor(multiOnA).Login() != accA.Login() || !a.AccountFor(multiOnA).Login().Pending() {
		t.Error("session on A does not owe A's login")
	}
	if a.AccountFor(multiOnB).Login() != accB.Login() || a.AccountFor(multiOnB).Login().Pending() {
		t.Error("session on B owes a login")
	}
}

// TestMultiAccount_HoldNoticeOnlyInChatsOnTheAccount (#3186 F7, RULINGS 4
// Q6: "Only chats whose session is on that account"; RULING 7: one rule for
// every agent): a hold of A is posted in the chats on A, and not in the
// default chat when its session is on B.
func TestMultiAccount_HoldNoticeOnlyInChatsOnTheAccount(t *testing.T) {
	reg := usagehold.New()
	a := holdAgent(reg, "claude-code")
	a.AgentID = "multi"
	accA, _ := twoAccountAgent(a, reg)
	const otherOnA = "multi/c3"
	if a.AccountFor(otherOnA) != accA { // a new chat takes the pick: A, the first
		t.Fatal("setup: new chat not on A")
	}
	var mu sync.Mutex
	got := map[string][]string{}
	a.UsageHoldNotifyFunc = func(sk, text string) { mu.Lock(); got[sk] = append(got[sk], text); mu.Unlock() }
	a.ChatSessionsFunc = func() []string { return []string{multiOnB, multiOnA, otherOnA, multiOnA} } // default chat first, on B
	a.StartUsageHold()
	defer a.StopUsageHold()

	accA.ObserveUsage(fiveHour(0.99))
	accA.ObserveUsage(fiveHour(0.10))
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, v := range got {
			n += len(v)
		}
		return n
	}
	if !waitFor(2*time.Second, func() bool { return count() == 4 }) {
		t.Fatalf("notices %v, want a start and an end in each of A's two chats", got)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got[multiOnB]) != 0 {
		t.Errorf("default chat on B got %q", got[multiOnB])
	}
	for _, sk := range []string{multiOnA, otherOnA} {
		if n := got[sk]; len(n) != 2 || !strings.Contains(n[0], "Usage hold") || !strings.Contains(n[1], "ended") {
			t.Errorf("%s notices %q, want one start and one end", sk, n)
		}
	}
}

// TestMultiAccount_NewSessionTakesThePick (#3186 F2, F4, #3246): a new
// session goes to an open account while another holds, a fresh child of a
// session on the held account included.
func TestMultiAccount_NewSessionTakesThePick(t *testing.T) {
	reg := usagehold.New()
	a := holdAgent(reg, "claude-code")
	a.AgentID = "multi"
	accA, accB := twoAccountAgent(a, reg)
	accA.ObserveUsage(fiveHour(0.99))
	if got := a.AccountFor("multi/c9"); got != accB {
		t.Fatalf("new session on %s while A holds, want B", got.Key())
	}
	if got := a.AccountFor(multiOnA + "/b1709596800"); got != accB {
		t.Errorf("fresh child of a session on A on %s, want the pick B", got.Key())
	}
}

// TestMultiAccount_AccountLeftStartsFreshAndTells (#3186 trap c): a session
// stored on an account the agent no longer lists starts on the pick with no
// resume, and its chat is told once.
func TestMultiAccount_AccountLeftStartsFreshAndTells(t *testing.T) {
	t.Parallel()
	idx := newTestSessionIndex(t)
	mgr, _ := newTestManager(t, idx)
	var mu sync.Mutex
	var notices []string
	mgr.SystemNoticeFunc = func(sk, text string) { mu.Lock(); notices = append(notices, sk+": "+text); mu.Unlock() }
	var resumes []string
	mgr.NewBackend = func() (delegator.Delegator, error) {
		return &resumeRecorder{mockBackendDM: &mockBackendDM{running: true}, resumes: &resumes, mu: &mu}, nil
	}
	const key = "test-agent/c1"
	for k, v := range map[string]string{session.MetaKeyAccount: "gone", session.MetaKeyCCResumeID: "old-sid"} {
		if err := idx.SetSessionMetadata(key, k, v); err != nil {
			t.Fatal(err)
		}
	}
	acct := account.New(account.Options{Backend: "claude-code", Spec: delegator.AccountSpec{Key: "A", Name: "/cc/A"}})
	mgr.Accounts = account.NewPool(idx, acct)
	mgr.Accounts.OnLeft = mgr.AccountLeft

	if _, err := mgr.Get(context.Background(), key); err != nil {
		t.Fatalf("Get: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(resumes) != 1 || resumes[0] != "" {
		t.Errorf("starts = %q, want one fresh start (no --resume of the other account's id)", resumes)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "gone") || !strings.Contains(notices[0], "/cc/A") || !strings.HasPrefix(notices[0], key) {
		t.Errorf("notices = %q, want one in the chat naming both accounts", notices)
	}
	if v, _ := idx.GetSessionMetadata(key, session.MetaKeyAccount); v != "A" {
		t.Errorf("stored account %q, want A", v)
	}
}

// resumeRecorder records the resume id of every Start.
type resumeRecorder struct {
	*mockBackendDM
	mu      *sync.Mutex
	resumes *[]string
}

func (r *resumeRecorder) Start(ctx context.Context, opts delegator.StartOptions) error {
	r.mu.Lock()
	*r.resumes = append(*r.resumes, opts.ResumeSessionID)
	r.mu.Unlock()
	return r.mockBackendDM.Start(ctx, opts)
}
