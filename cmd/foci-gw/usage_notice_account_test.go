package main

import (
	"strings"
	"testing"
	"time"

	"foci/internal/account"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/delegator/ccstream" // backend-cap:ignore: the test drives the real Claude Code notices through the gateway delivery
	"foci/internal/session"
	"foci/internal/timeutil"
)

// #3242 (Dick 2026-11-21, "Same-login chats, login named"): "Only chats on
// the login that is near its limit get the warning, and the warning names
// that login."

// memBindings is an in-memory account.BindingStore.
type memBindings map[string]string

func (m memBindings) GetSessionMetadata(sessionKey, key string) (string, error) {
	return m[sessionKey+"\x00"+key], nil
}

func (m memBindings) SetSessionMetadata(sessionKey, key, value string) error {
	m[sessionKey+"\x00"+key] = value
	return nil
}

func namedAccount(key, name string) *account.Account {
	return account.New(account.Options{Backend: "claude-code", Spec: delegator.AccountSpec{Key: key, Name: name}})
}

// twoLoginPool is a pool of two named logins with clutch/app-a and the
// triggering session on the first and clutch/app-b on the second.
func twoLoginPool(m *rlConnMgr) (*account.Pool, *account.Account, *account.Account) {
	dick := namedAccount("claude-code:dick", "dick@example.com")
	stuart := namedAccount("claude-code:stuart", "stuart@example.com")
	store := memBindings{}
	_ = store.SetSessionMetadata("clutch/app-a", session.MetaKeyAccount, dick.Key())
	_ = store.SetSessionMetadata(m.sessionKey, session.MetaKeyAccount, dick.Key())
	_ = store.SetSessionMetadata("clutch/app-b", session.MetaKeyAccount, stuart.Key())
	return account.NewPool(store, dick, stuart), dick, stuart
}

func TestDeliverRateLimitNoticeAll_OnlyChatsOnTheAccount(t *testing.T) {
	t.Parallel()
	m, open := newRLAllMgr()
	pool, _, stuart := twoLoginPool(m)

	// A read of stuart's login (no triggering session): only app-b is on it.
	deliverRateLimitNotice(m, "clutch", "", rlNotice, config.RateLimitNotifyAll, open, usageNoticeChats(pool, stuart))
	if got := len(m.others["clutch/app-b"].notices); got != 1 {
		t.Errorf("chat on the warning login got %d notices, want 1", got)
	}
	if got := len(m.others["clutch/app-a"].notices); got != 0 {
		t.Errorf("chat on the other login got %d notices, want 0", got)
	}
	if got := len(m.primary.notices); got != 0 {
		t.Errorf("primary got %d notices, want 0", got)
	}
}

// With several logins there is no fallback to the default chat: it may be on
// another login.
func TestDeliverRateLimitNoticeAll_NoChatOnTheAccountSendsNothing(t *testing.T) {
	t.Parallel()
	m, _ := newRLAllMgr()
	pool, _, stuart := twoLoginPool(m)
	onlyA := func(string) []string { return []string{"clutch/app-a"} }
	deliverRateLimitNotice(m, "clutch", "", rlNotice, config.RateLimitNotifyAll, onlyA, usageNoticeChats(pool, stuart))
	for k, c := range m.others {
		if len(c.notices) != 0 {
			t.Errorf("chat %s got %q, want nothing", k, c.notices)
		}
	}
	if got := len(m.primary.notices); got != 0 {
		t.Errorf("primary got %d notices, want 0", got)
	}
}

// An agent with one login keeps every chat and the default-chat fallback.
func TestUsageNoticeChats_OneLoginIsEveryChat(t *testing.T) {
	t.Parallel()
	one := namedAccount("claude-code:only", "only@example.com")
	if f := usageNoticeChats(account.NewPool(nil, one), one); f != nil {
		t.Error("one-login pool: got a filter, want nil (every chat)")
	}
	if f := usageNoticeChats(nil, nil); f != nil {
		t.Error("no pool: got a filter, want nil")
	}
}

func TestNameUsageNotice(t *testing.T) {
	t.Parallel()
	acct := namedAccount("claude-code:stuart", "stuart@example.com")
	got := nameUsageNotice(acct, "⚠️ Approaching Anthropic 5-hour rate limit.\nUsage: 97%")
	if want := "⚠️ Approaching Anthropic 5-hour rate limit. (account: stuart@example.com)\nUsage: 97%"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := nameUsageNotice(acct, "🛑 one line"); got != "🛑 one line (account: stuart@example.com)" {
		t.Errorf("one line: got %q", got)
	}
	unnamed := account.New(account.Options{Backend: "codex", Spec: delegator.AccountSpec{Key: "codex"}})
	if got := nameUsageNotice(unnamed, "x\ny"); got != "x\ny" {
		t.Errorf("unnamed account: got %q, want the notice unchanged", got)
	}
}

// The scheduled read path end to end: the read of stuart's login reaches
// only the chat on it, and names the login.
func TestDeliverUsageReadNotices_SameLoginChatsNamed(t *testing.T) {
	t.Parallel()
	const agentID = "clutch" // open chats are listed for this id
	m, open := newRLAllMgr()
	m.session = nil
	now := timeutil.Now()
	read := &delegator.UsageInfo{
		RateLimitsAvailable: true,
		FiveHour:            delegator.UsageWindow{Percent: 97, ResetsAt: now.Add(time.Hour), Reported: true},
		SevenDay:            delegator.UsageWindow{Percent: 17, ResetsAt: now.Add(120 * time.Hour), Reported: true},
	}
	// Its own throttle: a key no other test uses.
	acct := namedAccount("claude-code:stuart-t3242-read", "stuart@example.com")
	store := memBindings{}
	_ = store.SetSessionMetadata("clutch/app-a", session.MetaKeyAccount, "claude-code:dick")
	_ = store.SetSessionMetadata("clutch/app-b", session.MetaKeyAccount, acct.Key())
	pool := account.NewPool(store, namedAccount("claude-code:dick", "dick@example.com"), acct)

	deliverUsageReadNotices(m, agentID, acct, pool, ccstream.AnnounceUsageRead, read, delegator.UsageSourceScheduled, config.RateLimitNotifyAll, delegator.DefaultUsageWarningLevels, open, nil)
	if got := m.others["clutch/app-a"].notices; len(got) != 0 {
		t.Errorf("chat on the other login got %q, want nothing", got)
	}
	got := m.others["clutch/app-b"].notices
	if len(got) != 1 || !strings.Contains(strings.SplitN(got[0], "\n", 2)[0], "(account: stuart@example.com)") {
		t.Errorf("chat on the warning login got %q, want one notice naming stuart@example.com in its headline", got)
	}
}
