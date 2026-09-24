package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"foci/internal/delegator"
)

// #2013: live checks on codex's token-usage semantics. The quiet arm replays
// the real codex 0.145.0 notifications captured for #1855 (turn_usage_test.go);
// each violating arm changes exactly the one field whose meaning it guards.

func guardedBackend(t *testing.T) (*Backend, *delegator.ExpectationGuard) {
	t.Helper()
	b := newTestBackend(t)
	g := &delegator.ExpectationGuard{}
	b.expect = g
	return b, g
}

func TestTokenUsageExpectations_RealTrafficIsQuiet(t *testing.T) {
	t.Parallel()
	b, g := guardedBackend(t)
	openUsageTurn(b)
	for _, n := range []string{usageCycle1, usageCycle2, usageCycle3, usageCycle4, turnCompleted1, usageTurn2} {
		b.dispatch([]byte(n))
	}
	for _, inv := range []string{invTotalIsSumOfLast, invCachedIsSubsetOfInput} {
		if got := g.Count(expectBackend, inv); got != 0 {
			t.Errorf("%q fired %d time(s) on real codex 0.145.0 traffic", inv, got)
		}
	}
	// Premise: the notifications reached the check (it records per-thread totals).
	b.turnMu.Lock()
	_, seen := b.threadTotal["th_1"]
	b.turnMu.Unlock()
	if !seen {
		t.Fatal("no thread total recorded — the notifications never reached checkTokenUsage")
	}
}

// If codex made `last` a running per-turn figure, foci's per-turn sum of last
// would count every earlier cycle again.
func TestTokenUsageExpectations_LastNotPerCycleFires(t *testing.T) {
	t.Parallel()
	b, g := guardedBackend(t)
	openUsageTurn(b)
	b.dispatch([]byte(usageCycle1))
	// Cycle 2 as captured, but with last == the turn's running total so far.
	cumulative := strings.Replace(usageCycle2,
		`"last":{"inputTokens":15611,"cachedInputTokens":15360,"cacheWriteInputTokens":0,"outputTokens":40,"reasoningOutputTokens":0,"totalTokens":15651}`,
		`"last":{"inputTokens":31120,"cachedInputTokens":26624,"cacheWriteInputTokens":0,"outputTokens":104,"reasoningOutputTokens":0,"totalTokens":31224}`, 1)
	if cumulative == usageCycle2 {
		t.Fatal("fixture substitution did not apply")
	}
	b.dispatch([]byte(cumulative))
	if got := g.Count(expectBackend, invTotalIsSumOfLast); got != 1 {
		t.Errorf("violations = %d, want 1", got)
	}
}

// If codex made cachedInputTokens additive, onTokenUsage's subtraction would
// erase real input tokens.
func TestTokenUsageExpectations_CachedAdditiveFires(t *testing.T) {
	t.Parallel()
	b, g := guardedBackend(t)
	openUsageTurn(b)
	// Cycle 1 re-expressed with cached tokens OUTSIDE input: input 4245 +
	// cached 11264 + output 64 = total 15573.
	additive := strings.Replace(usageCycle1,
		`"last":{"inputTokens":15509,`, `"last":{"inputTokens":4245,`, 1)
	if additive == usageCycle1 {
		t.Fatal("fixture substitution did not apply")
	}
	b.dispatch([]byte(additive))
	if got := g.Count(expectBackend, invCachedIsSubsetOfInput); got != 1 {
		t.Errorf("violations = %d, want 1", got)
	}
}

func TestTotalGrowthMismatch_NotJudged(t *testing.T) {
	t.Parallel()
	u := func(in, cached, out int) tokenUsageBreakdown {
		return tokenUsageBreakdown{InputTokens: in, CachedInputTokens: cached, OutputTokens: out}
	}
	cases := []struct {
		name            string
		prev, cur, last tokenUsageBreakdown
		seen            bool
	}{
		{"first on thread (resumed history)", u(0, 0, 0), u(90000, 80000, 500), u(100, 0, 5), false},
		{"total reset", u(90000, 80000, 500), u(100, 0, 5), u(100, 0, 5), true},
		{"exact duplicate delivery", u(90743, 88448, 74), u(90743, 88448, 74), u(90743, 88448, 74), true},
	}
	for _, c := range cases {
		if got := totalGrowthMismatch(c.prev, c.cur, c.last, c.seen); got != "" {
			t.Errorf("%s: judged a mismatch: %s", c.name, got)
		}
	}
}

func TestVersionFromUserAgent(t *testing.T) {
	t.Parallel()
	// codex-cli 0.145.0's initialize response, 2026-09-24 (client name and version substituted).
	raw := json.RawMessage(`{"userAgent":"foci/0.145.0 (Linux Mint 22.0.0; x86_64) unknown (foci; 1.2.3)","codexHome":"/home/foci/.codex","platformFamily":"unix","platformOs":"linux"}`)
	b, g := guardedBackend(t)
	b.noteInitializeVersion(raw)
	if got := b.version(); got != "0.145.0" {
		t.Errorf("version = %q, want 0.145.0", got)
	}
	// And a violation now names it.
	lg := &capLogger{}
	g.Violated(lg, expectBackend, b.version(), "probe", "d")
	if !strings.Contains(lg.last, "codex 0.145.0") {
		t.Errorf("report does not name the version: %s", lg.last)
	}
	if v := versionFromUserAgent("no slash here"); v != "" {
		t.Errorf("versionFromUserAgent(garbage) = %q, want empty", v)
	}
}

type capLogger struct{ last string }

func (c *capLogger) Infof(string, ...any) {}
func (c *capLogger) Warnf(string, ...any) {}
func (c *capLogger) Errorf(format string, args ...any) {
	c.last = fmt.Sprintf(format, args...)
}
