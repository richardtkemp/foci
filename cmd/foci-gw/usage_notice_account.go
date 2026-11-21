package main

import (
	"strings"

	"foci/internal/account"
)

// usage_notice_account.go — which login a usage warning is about (#3242).
//
// Since #3186 one agent can run on several logins, each with its own usage
// windows. A usage warning (Claude Code's rate_limit_event, a scheduled
// usage read, codex's rate-limit update) is about the one login whose
// figures raised it. Dick 2026-11-21 (ruling "Same-login chats, login
// named"): "Only chats on the login that is near its limit get the warning,
// and the warning names that login."

// usageNoticeChats is the chat filter of a usage warning about acct, for
// deliverRateLimitNotice: it reports whether a chat's session is on acct.
// nil when pool has at most one account: every session of such an agent is
// on its one login, so every chat may get the warning, and the "all"
// target's fallback to the default chat stays as before.
func usageNoticeChats(pool *account.Pool, acct *account.Account) func(sessionKey string) bool {
	if len(pool.Accounts()) <= 1 {
		return nil
	}
	key := acct.Key()
	return func(sessionKey string) bool {
		// The same question the hold notices ask (Agent.postUsageHoldNotice):
		// a chat not bound yet is bound here, to the account it would take
		// at its first turn.
		return pool.For(sessionKey).Key() == key
	}
}

// nameUsageNotice names acct in a usage warning: " (account: <name>)" at the
// end of the notice's first line (its headline), so the name stays above
// the table withUsageTable adds. The notice is unchanged when the account
// has no name (account.Account.Name).
func nameUsageNotice(acct *account.Account, notice string) string {
	if acct.Name() == "" {
		return notice
	}
	head, rest, multi := strings.Cut(notice, "\n")
	head = acct.WithName(head)
	if !multi {
		return head
	}
	return head + "\n" + rest
}
