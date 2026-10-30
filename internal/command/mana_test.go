package command

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

func TestFormatUsage(t *testing.T) {
	info := &delegator.UsageInfo{
		Backend:          "Claude Code",
		SubscriptionType: "max",
		FiveHour:         delegator.UsageWindow{Percent: 94, ResetsAt: time.Date(2026, 7, 17, 20, 29, 59, 0, time.UTC)},
		SevenDay:         delegator.UsageWindow{Percent: 43, ResetsAt: time.Date(2026, 7, 19, 22, 59, 59, 0, time.UTC)},
		SessionCostUSD:   0.0065691,
		Day: delegator.UsageBehaviorWindow{
			RequestCount: 4152, SessionCount: 97,
			Top: []delegator.UsageBehaviorItem{{Key: "long_context", Pct: 89, Count: 2838}},
		},
		Week: delegator.UsageBehaviorWindow{
			RequestCount: 26660, SessionCount: 464,
			Top: []delegator.UsageBehaviorItem{{Key: "long_context", Pct: 85, Count: 16990}},
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

	// #2658 must leave Claude Code's reply byte-for-byte as it was.
	want := "📊 *Claude Code usage* (max)\n\n" +
		"Session (5h): 94% · resets " + formatResetTime(info.FiveHour) + "\n" +
		"Week (7d): 43% · resets " + formatResetTime(info.SevenDay) + "\n" +
		"\nThis check's session cost so far: $0.0066\n" +
		"\nLast 24h · 4152 requests · 97 sessions\n" +
		"  89% long_context\n" +
		"\nLast 7d · 26660 requests · 464 sessions\n" +
		"  85% long_context"
	if text != want {
		t.Errorf("Claude Code reply changed:\n got: %q\nwant: %q", text, want)
	}
}

// TestFormatUsage_Allowance pins the Codex reply (#2658): it names Codex,
// shows each bucket's windows and the limit-reached and credit state, and none
// of the Claude Code fields.
func TestFormatUsage_Allowance(t *testing.T) {
	r5 := time.Date(2026, 10, 30, 15, 4, 0, 0, time.UTC)
	r7 := time.Date(2026, 11, 4, 10, 7, 0, 0, time.UTC)
	info := &delegator.UsageInfo{
		Backend: "Codex",
		Allowance: &delegator.Allowance{
			PlanType: "team",
			Buckets: []delegator.AllowanceBucket{
				{Windows: []delegator.AllowanceWindow{
					{Label: "5h", Percent: 34, ResetsAt: r5},
					{Label: "7d", Percent: 12, ResetsAt: r7},
				}},
				{Name: "premium", Credits: "0", LimitReached: "workspace out of credits"},
			},
			Fresh: true,
		},
	}
	text := formatUsage(info)
	want := "📊 *Codex usage* (team)\n\n" +
		"5h: 34% · resets " + formatInstant(r5) + "\n" +
		"7d: 12% · resets " + formatInstant(r7) + "\n" +
		"\npremium: credits 0 · 🛑 limit reached: workspace out of credits\n" +
		"\nRead from Codex just now."
	if text != want {
		t.Errorf("codex reply:\n got: %q\nwant: %q", text, want)
	}
	for _, claude := range []string{"Claude", "Session (5h)", "Week (7d)", "requests", "session cost"} {
		if strings.Contains(text, claude) {
			t.Errorf("codex reply carries Claude Code field %q", claude)
		}
	}
}

// TestFormatUsage_AllowanceStale pins the reply when no fresh read was made:
// it says when the numbers arrived and why they are not fresh, and with no
// numbers at all it says so instead of showing an empty report.
func TestFormatUsage_AllowanceStale(t *testing.T) {
	asOf := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	text := formatUsage(&delegator.UsageInfo{Backend: "Codex", Allowance: &delegator.Allowance{
		Buckets: []delegator.AllowanceBucket{{Windows: []delegator.AllowanceWindow{{Label: "5h", Percent: 80}}}},
		AsOf:    asOf,
		Note:    "no codex process is running",
	}})
	if !strings.Contains(text, "5h: 80% · resets unknown") || !strings.Contains(text, "Last reported by Codex at "+formatInstant(asOf)+" (no fresh read: no codex process is running).") {
		t.Errorf("stale reply:\n%s", text)
	}

	text = formatUsage(&delegator.UsageInfo{Backend: "Codex", Allowance: &delegator.Allowance{Note: "no codex process is running"}})
	if !strings.Contains(text, "Codex has not reported an allowance since foci started.") || !strings.Contains(text, "No fresh read: no codex process is running.") {
		t.Errorf("empty reply:\n%s", text)
	}
}

func TestFormatUsage_UnknownResetTime(t *testing.T) {
	info := &delegator.UsageInfo{Backend: "Claude Code", SubscriptionType: "max"}
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
	mana := ManaCommand(nil, "a")
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
	r.Register(ManaCommand(nil, "a"))

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

// TestManaCommand_ExecuteUsesInjectedQuery pins that /mana reports what the
// agent's backend query returns (#1543), rather than always asking Claude Code,
// and asks it about this agent (#2658: codex usage is per agent).
func TestManaCommand_ExecuteUsesInjectedQuery(t *testing.T) {
	called := ""
	cmd := ManaCommand(func(_ context.Context, agentID string) (*delegator.UsageInfo, error) {
		called = agentID
		return &delegator.UsageInfo{Backend: "Claude Code", SubscriptionType: "pro", FiveHour: delegator.UsageWindow{Percent: 12}}, nil
	}, "agent-7")
	resp, err := cmd.Execute(context.Background(), Request{}, CommandContext{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if called != "agent-7" {
		t.Fatalf("injected usage query got agent %q, want agent-7", called)
	}
	if !strings.Contains(resp.Text, "pro") || !strings.Contains(resp.Text, "12%") {
		t.Errorf("reply does not render the query's result; got:\n%s", resp.Text)
	}

	boom := errors.New("boom")
	cmd = ManaCommand(func(context.Context, string) (*delegator.UsageInfo, error) { return nil, boom }, "a")
	if _, err := cmd.Execute(context.Background(), Request{}, CommandContext{}); !errors.Is(err, boom) {
		t.Errorf("Execute error = %v, want it to wrap the query error", err)
	}
}
