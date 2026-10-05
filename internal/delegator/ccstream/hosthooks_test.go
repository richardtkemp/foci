package ccstream

import (
	"testing"

	"foci/internal/delegator"
	"foci/internal/delegator/pretool"
	"foci/internal/delegator/stoprule"
	"foci/internal/ratelimit"
)

// TestSetHostHooks_WiresEveryCallback: ccstream honours every HostHooks field
// (#2154 Phase 3). Each one, fired through the backend's own path, must reach
// the gateway's callback; before HostHooks each was a separate setter the
// gateway called after a *ccstream.Backend type assertion.
func TestSetHostHooks_WiresEveryCallback(t *testing.T) {
	t.Parallel()
	var auth, noticeKey, notice string
	var engaged bool
	b := &Backend{}
	b.startOpts.SessionKey = "agent/c1"
	b.SetHostHooks(delegator.HostHooks{
		OnAuthFailure:     func(d string) { auth = d },
		OnRateLimitNotice: func(sk, n string) { noticeKey, notice = sk, n },
		EngageRateLimit:   func(ratelimit.Signal) { engaged = true },
		PreToolRules:      func() []pretool.Rule { return []pretool.Rule{{Name: "p"}} },
		StopRules:         func() []stoprule.Rule { return []stoprule.Rule{{Name: "s"}} },
	})

	b.fireAuthFailure("401")
	b.fireRateLimited("at 90%")
	b.fireSessionLimit(ratelimit.Signal{})
	if auth != "401" {
		t.Errorf("OnAuthFailure got %q", auth)
	}
	if noticeKey != "agent/c1" || notice != "at 90%" {
		t.Errorf("OnRateLimitNotice got (%q, %q), want the backend's session key and the notice", noticeKey, notice)
	}
	if !engaged {
		t.Error("EngageRateLimit not fired on a session limit")
	}
	if b.preToolRules == nil || b.preToolRules()[0].Name != "p" {
		t.Error("PreToolRules not installed")
	}
	if b.stopRules == nil || b.stopRules()[0].Name != "s" {
		t.Error("StopRules not installed")
	}
}

// TestAgentThrottle_SharedPerAgent: every Backend of one agent shares one
// rate-limit warning throttle (the gateway used to build and pass it), and
// agents don't share.
func TestAgentThrottle_SharedPerAgent(t *testing.T) {
	t.Parallel()
	a1, a2 := agentThrottle("throttle-test-a"), agentThrottle("throttle-test-a")
	if a1 != a2 {
		t.Error("two Backends of one agent got different throttles")
	}
	if agentThrottle("throttle-test-b") == a1 {
		t.Error("two agents share a throttle")
	}
}
