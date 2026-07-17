package command

import (
	"strings"
	"testing"
	"time"

	"foci/internal/delegator/ccstream"
)

func TestFormatUsage(t *testing.T) {
	info := &ccstream.UsageInfo{
		SubscriptionType: "max",
		FiveHour:         ccstream.UsageWindow{Percent: 94, ResetsAt: time.Date(2026, 7, 17, 20, 29, 59, 0, time.UTC)},
		SevenDay:         ccstream.UsageWindow{Percent: 43, ResetsAt: time.Date(2026, 7, 19, 22, 59, 59, 0, time.UTC)},
		SessionCostUSD:   0.0065691,
		Day: ccstream.UsageBehaviorWindow{
			RequestCount: 4152, SessionCount: 97,
			Top: []ccstream.UsageBehaviorItem{{Key: "long_context", Pct: 89, Count: 2838}},
		},
		Week: ccstream.UsageBehaviorWindow{
			RequestCount: 26660, SessionCount: 464,
			Top: []ccstream.UsageBehaviorItem{{Key: "long_context", Pct: 85, Count: 16990}},
		},
	}

	text := formatUsage(info)

	// The reason this feature exists: a comfortably-below-threshold weekly
	// percentage must render, not just a near-limit one.
	for _, want := range []string{"94%", "43%", "max", "4152 requests", "97 sessions", "long_context", "89%"} {
		if !strings.Contains(text, want) {
			t.Errorf("formatUsage output missing %q; got:\n%s", want, text)
		}
	}
}

func TestFormatUsage_UnknownResetTime(t *testing.T) {
	info := &ccstream.UsageInfo{SubscriptionType: "max"}
	text := formatUsage(info)
	if !strings.Contains(text, "unknown") {
		t.Errorf("expected 'unknown' reset time when ResetsAt is zero; got:\n%s", text)
	}
}

// TestManaCommand_UsageIsAlias pins the shape: /usage dispatches to the exact
// same *Command as /mana (not a lookalike copy), and /mana itself stays
// visible (not Hidden — only a duplicate LISTING would be wrong, not the
// command itself).
func TestManaCommand_UsageIsAlias(t *testing.T) {
	mana := ManaCommand()
	if mana.Hidden {
		t.Error("ManaCommand must be visible (Hidden=false)")
	}
	if mana.Name != "mana" {
		t.Errorf("ManaCommand.Name = %q, want %q", mana.Name, "mana")
	}
	if len(mana.Aliases) != 1 || mana.Aliases[0] != "usage" {
		t.Errorf("ManaCommand.Aliases = %v, want [usage]", mana.Aliases)
	}
	if mana.Execute == nil {
		t.Fatal("Execute must be non-nil")
	}
}

// TestRegistry_ManaUsageAlias_DispatchesToSamePointerAndListsOnce is the
// real regression test: this is exactly the shape (PairKeyCommand's
// Aliases: []string{"pairkey", "pair-key"}) that, before Registry.All()
// deduplicated by pointer identity, made a single command appear THREE
// TIMES in /help (once per registered key) — verified live against
// PairKeyCommand while diagnosing this. /mana + its /usage alias is the
// same shape with 2 keys instead of 3: without the fix this asserts 2, not 1.
func TestRegistry_ManaUsageAlias_DispatchesToSamePointerAndListsOnce(t *testing.T) {
	r := NewRegistry()
	r.Register(ManaCommand())

	mana, ok := r.commands["mana"]
	if !ok {
		t.Fatal("/mana not registered")
	}
	usage, ok := r.commands["usage"]
	if !ok {
		t.Fatal("/usage not registered")
	}
	if mana != usage {
		t.Fatal("/usage must dispatch to the identical *Command as /mana, not a copy")
	}

	var manaCount int
	for _, c := range r.All() {
		if c.Name == "mana" {
			manaCount++
		}
	}
	if manaCount != 1 {
		t.Errorf("mana appears %d times in Registry.All(), want 1 (alias dedup regression)", manaCount)
	}
}
