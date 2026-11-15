package agent

import (
	"strings"
	"testing"
	"time"

	"foci/internal/modelinfo"
	"foci/internal/provider"
)

// nearFullAgent is an Agent with a fixed 200k context window whose near-full
// notices land on the returned channel.
func nearFullAgent(t *testing.T) (*Agent, chan string) {
	t.Helper()
	got := make(chan string, 8)
	ag := &Agent{Model: "openrouter/acme/nonexistent-turbo-x"}
	ag.ModelMetaFn = func(string) modelinfo.ModelMeta { return modelinfo.ModelMeta{ContextWindow: 200_000} }
	ag.ContextNearFullNoticeFunc = func(sk, text string) { got <- sk + "|" + text }
	return ag, got
}

// turnAt is a finished turn on sk whose last cycle held fill context tokens,
// split across the three input fields the way a real response reports them.
func turnAt(ag *Agent, sk string, fill int) *TurnState {
	return &TurnState{
		SessionKey:  sk,
		SessionMeta: ag.getSessionMeta(sk),
		FinalUsage: &provider.Usage{
			InputTokens:              10,
			CacheCreationInputTokens: 90,
			CacheReadInputTokens:     fill - 100,
		},
	}
}

func expectNotice(t *testing.T, got chan string) string {
	t.Helper()
	select {
	case s := <-got:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no near-full notice was sent")
		return ""
	}
}

func expectNoNotice(t *testing.T, got chan string) {
	t.Helper()
	select {
	case s := <-got:
		t.Fatalf("unexpected near-full notice: %q", s)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestContextNearFull_WarnsAtNinetyFivePercentOfWindow proves the #1306
// ruling: the warning fires when the turn's context reaches 95% of the FULL
// window (not of the compaction threshold), and not below it.
func TestContextNearFull_WarnsAtNinetyFivePercentOfWindow(t *testing.T) {
	t.Parallel()
	ag, got := nearFullAgent(t)

	ag.warnIfContextNearFull(turnAt(ag, "x/c1", 189_999)) // 94.99%
	expectNoNotice(t, got)

	ag.warnIfContextNearFull(turnAt(ag, "x/c1", 190_000)) // exactly 95%
	s := expectNotice(t, got)
	if !strings.HasPrefix(s, "x/c1|") {
		t.Errorf("notice went to %q, want session x/c1", s)
	}
	if !strings.Contains(s, "95%") || !strings.Contains(s, "190,000") || !strings.Contains(s, "200,000") {
		t.Errorf("notice %q should state 95%% and 190,000 of 200,000 tokens", s)
	}
}

// TestContextNearFull_OncePerCrossing proves the warning fires once when the
// context crosses 95%, not on every later turn above it, and fires again after
// the context has dropped below (e.g. a compaction) and crossed once more.
func TestContextNearFull_OncePerCrossing(t *testing.T) {
	t.Parallel()
	ag, got := nearFullAgent(t)

	ag.warnIfContextNearFull(turnAt(ag, "x/c1", 191_000))
	expectNotice(t, got)
	ag.warnIfContextNearFull(turnAt(ag, "x/c1", 195_000))
	expectNoNotice(t, got)

	ag.warnIfContextNearFull(turnAt(ag, "x/c1", 40_000)) // compacted
	expectNoNotice(t, got)
	ag.warnIfContextNearFull(turnAt(ag, "x/c1", 192_000))
	expectNotice(t, got)
}

// TestContextNearFull_OnlyUserChats proves that only a root chat session (the
// conversation a human reads) is warned: a spawn, branch or batch child, or an
// independent session, has no chat of its own, and a notice for it would land
// in the agent's default chat.
func TestContextNearFull_OnlyUserChats(t *testing.T) {
	t.Parallel()
	ag, got := nearFullAgent(t)
	for _, sk := range []string{"x/c1/b1794759504", "x/iwork"} {
		ag.warnIfContextNearFull(turnAt(ag, sk, 199_000))
	}
	expectNoNotice(t, got)
}

// TestContextNearFull_NoUsageNoNotice: a steered follow-up's post-turn has no
// FinalUsage and must not warn (nor panic).
func TestContextNearFull_NoUsageNoNotice(t *testing.T) {
	t.Parallel()
	ag, got := nearFullAgent(t)
	ag.warnIfContextNearFull(&TurnState{SessionKey: "x/c1", SessionMeta: ag.getSessionMeta("x/c1")})
	expectNoNotice(t, got)

	// A nil hook (tests, no platform) is a silent no-op.
	ag.ContextNearFullNoticeFunc = nil
	ag.warnIfContextNearFull(turnAt(ag, "x/c1", 199_000))
}
