package main

import (
	"strings"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/delegator/ccstream" // backend-cap:ignore: the test drives the real Claude Code notices through the gateway delivery
	"foci/internal/timeutil"
)

// TestDeliverUsageReadNotices (#3144): a usage read that reaches a new level
// is delivered like the stream's notice: under "all", once to every open chat
// of the agent and not to the primary; the same level read again is not
// delivered again; a read under 50% delivers nothing.
func TestDeliverUsageReadNotices(t *testing.T) {
	t.Parallel()
	const agentID = "t3144-gw" // its own process-wide throttle
	m, _ := newRLAllMgr()
	m.session = nil
	open := func(id string) []string {
		if id != agentID {
			return nil
		}
		return []string{"clutch/app-a", "clutch/app-b"}
	}
	reset := timeutil.Now().Add(48 * time.Hour)
	read := func(pct int) *delegator.UsageInfo {
		return &delegator.UsageInfo{
			RateLimitsAvailable: true,
			FiveHour:            delegator.UsageWindow{Percent: 1, ResetsAt: timeutil.Now().Add(time.Hour), Reported: true},
			SevenDay:            delegator.UsageWindow{Percent: pct, ResetsAt: reset, Reported: true},
		}
	}

	deliverUsageReadNotices(m, agentID, ccstream.AnnounceUsageRead, read(40), delegator.UsageSourceScheduled, config.RateLimitNotifyAll, delegator.DefaultUsageWarningLevels, open)
	for k, c := range m.others {
		if len(c.notices) != 0 {
			t.Fatalf("40%%: chat %s got %q, want nothing", k, c.notices)
		}
	}

	deliverUsageReadNotices(m, agentID, ccstream.AnnounceUsageRead, read(72), delegator.UsageSourceScheduled, config.RateLimitNotifyAll, delegator.DefaultUsageWarningLevels, open)
	deliverUsageReadNotices(m, agentID, ccstream.AnnounceUsageRead, read(75), delegator.UsageSourceScheduled, config.RateLimitNotifyAll, delegator.DefaultUsageWarningLevels, open)
	for k, c := range m.others {
		if len(c.notices) != 1 || !strings.Contains(c.notices[0], "7-day") || !strings.Contains(c.notices[0], "Usage: 72%") {
			t.Errorf("chat %s got %q, want one 7-day notice at 72%%", k, c.notices)
		}
	}
	if n := len(m.primary.notices); n != 0 {
		t.Errorf("primary got %d notices, want 0 (open chats have it)", n)
	}
}
