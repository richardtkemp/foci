package command

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/ratelimit"
	"foci/internal/usagehold"
	"foci/internal/usagelog"
)

func pf(v float64) *float64 { return &v }

// testHistory is a usage history in a temp dir, bound to account for agent
// "cam" on claude-code, on a fixed clock.
func testHistory(t *testing.T, account string, now time.Time) (*usagelog.Store, *usagelog.Binding) {
	t.Helper()
	st, err := usagelog.Open(filepath.Join(t.TempDir(), "usage-history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, &usagelog.Binding{Store: st, Backend: "claude-code", Account: account, AgentID: "cam", Now: func() time.Time { return now }}
}

func storeReading(t *testing.T, st *usagelog.Store, account string, received time.Time, source string, windows ...delegator.UsageReportWindow) {
	t.Helper()
	if err := st.Save(delegator.UsageReport{Source: source, ReceivedAt: received, Backend: "claude-code", Account: account, AgentID: "cam", Windows: windows}); err != nil {
		t.Fatal(err)
	}
}

func pctWindow(name string, pct float64, reset time.Time) delegator.UsageReportWindow {
	return delegator.UsageReportWindow{Window: name, Length: delegator.UsageWindowLength(name), Used: pf(pct), UsedUnit: delegator.UsageUnitPercent, ResetsAt: reset}
}

func runMana(t *testing.T, cmd *Command, args string, cc CommandContext) string {
	t.Helper()
	resp, err := cmd.Execute(context.Background(), Request{Name: "mana", Args: args}, cc)
	if err != nil {
		t.Fatalf("/mana %s: %v", args, err)
	}
	return resp.Text
}

// TestManaCached_InstantWhileReadBlocks is H1 (requirement 1): with a usage
// query that blocks forever and one stored reading, /mana cached answers
// within 1 s with the stored figures, their resets and ages; plain /mana
// still calls the query.
func TestManaCached_InstantWhileReadBlocks(t *testing.T) {
	now := time.Date(2026, 11, 13, 21, 0, 0, 0, time.UTC)
	st, hist := testHistory(t, "claude-code:A", now)
	r5, r7 := now.Add(2*time.Hour), now.Add(3*24*time.Hour)
	storeReading(t, st, "claude-code:A", now.Add(-20*time.Minute), delegator.UsageSourceScheduled,
		pctWindow("five_hour", 30, r5), pctWindow("seven_day", 20, r7))

	called := make(chan struct{}, 4)
	block := func(ctx context.Context, _ delegator.UsageTarget) (*delegator.UsageInfo, error) {
		called <- struct{}{}
		<-ctx.Done() // a read that hangs until its caller gives up
		return nil, ctx.Err()
	}
	cmd := ManaCommand(block, delegator.UsageTarget{AgentID: "cam"}, true, hist)

	done := make(chan string, 1)
	go func() { done <- runMana(t, cmd, "cached", CommandContext{}) }()
	var text string
	select {
	case text = <-done:
	case <-time.After(time.Second):
		t.Fatal("/mana cached did not answer within 1 s while the usage query blocks")
	}
	select {
	case <-called:
		t.Fatal("/mana cached called the usage query")
	default:
	}
	for _, want := range []string{"30%", "20%", formatInstant(r5), formatInstant(r7), "received 20m ago", "5h (five_hour)", "7d (seven_day)"} {
		if !strings.Contains(text, want) {
			t.Errorf("reply lacks %q:\n%s", want, text)
		}
	}
	t.Logf("/mana cached reply:\n%s", text)

	ctx, cancel := context.WithCancel(context.Background())
	freshDone := make(chan struct{})
	go func() {
		_, _ = cmd.Execute(ctx, Request{Name: "mana"}, CommandContext{})
		close(freshDone)
	}()
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("plain /mana did not call the usage query")
	}
	cancel()
	<-freshDone
}

// TestManaCached_JSON is H2 (requirement 3): the json form carries every
// requirement-2 value under the documented keys, and every key it emits is
// documented in the command's help.
func TestManaCached_JSON(t *testing.T) {
	now := time.Date(2026, 11, 13, 21, 0, 0, 0, time.UTC)
	st, hist := testHistory(t, "claude-code:A", now)
	r5 := now.Add(2 * time.Hour)
	storeReading(t, st, "claude-code:A", now.Add(-20*time.Minute), delegator.UsageSourceScheduled,
		pctWindow("five_hour", 30, r5), pctWindow("seven_day", 20, now.Add(72*time.Hour)))
	text := runMana(t, ManaCommand(nil, delegator.UsageTarget{}, false, hist), "cached json", CommandContext{})

	var got map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("json form is not JSON: %v\n%s", err, text)
	}
	ws, _ := got["windows"].([]any)
	if len(ws) != 2 {
		t.Fatalf("windows = %v", got["windows"])
	}
	w := ws[0].(map[string]any)
	if w["window"] != "five_hour" || w["length_seconds"] != float64(18000) || w["used_percent"] != float64(30) ||
		w["resets_at"] != float64(r5.Unix()) || w["used_received_at"] != float64(now.Add(-20*time.Minute).Unix()) || w["used_age_seconds"] != float64(1200) {
		t.Errorf("five_hour = %v", w)
	}
	if got["account"] != "claude-code:A" || got["has_figures"] != true || got["now"] != float64(now.Unix()) {
		t.Errorf("header = %v", got)
	}
	keys := map[string]bool{}
	for k := range got {
		keys[k] = true
	}
	for k := range w {
		keys[k] = true
	}
	for k := range keys {
		if !strings.Contains(manaCachedHelp, "`"+k+"`") {
			t.Errorf("key %q is not documented in the help", k)
		}
	}
	t.Logf("/mana cached json:\n%s", text)
}

// TestManaCached_PercentlessEvent is H4 at the command (requirement 2).
func TestManaCached_PercentlessEvent(t *testing.T) {
	day := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	hm := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }

	st, hist := testHistory(t, "A", hm(21, 0))
	storeReading(t, st, "A", hm(20, 40), delegator.UsageSourceScheduled, pctWindow("five_hour", 30, hm(23, 0)))
	storeReading(t, st, "A", hm(20, 55), delegator.UsageSourceCCEvent,
		delegator.UsageReportWindow{Window: "five_hour", Length: 5 * time.Hour, ResetsAt: hm(23, 0), Status: "allowed"})
	text := runMana(t, ManaCommand(nil, delegator.UsageTarget{}, false, hist), "cached json", CommandContext{})
	var got manaCachedReply
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Windows) != 1 || got.Windows[0].UsedPercent == nil || *got.Windows[0].UsedPercent != 30 || *got.Windows[0].UsedReceivedAt != hm(20, 40).Unix() {
		t.Errorf("after a percent-less event: %s", text)
	}

	st2, hist2 := testHistory(t, "B", hm(18, 10))
	storeReading(t, st2, "B", hm(17, 50), delegator.UsageSourceScheduled, pctWindow("five_hour", 80, hm(18, 0)))
	storeReading(t, st2, "B", hm(18, 5), delegator.UsageSourceCCEvent,
		delegator.UsageReportWindow{Window: "five_hour", Length: 5 * time.Hour, ResetsAt: hm(23, 0), Status: "allowed"})
	cmd := ManaCommand(nil, delegator.UsageTarget{}, false, hist2)
	readable := runMana(t, cmd, "cached", CommandContext{})
	if strings.Contains(readable, "80%") || !strings.Contains(readable, "no percent figure held for the current window") {
		t.Errorf("figure from before the reset shown, or no 'none held' line:\n%s", readable)
	}
	text = runMana(t, cmd, "cached json", CommandContext{})
	got = manaCachedReply{}
	_ = json.Unmarshal([]byte(text), &got)
	if len(got.Windows) != 1 || got.Windows[0].UsedPercent != nil || got.Windows[0].ResetsAt == nil || *got.Windows[0].ResetsAt != hm(23, 0).Unix() {
		t.Errorf("json after a reset: %s", text)
	}
}

// TestManaCached_NothingStored is H5: with nothing stored, both forms say
// so.
func TestManaCached_NothingStored(t *testing.T) {
	_, hist := testHistory(t, "A", time.Now())
	cmd := ManaCommand(nil, delegator.UsageTarget{}, false, hist)
	if text := runMana(t, cmd, "cached", CommandContext{}); !strings.Contains(text, "holds no usage figures") {
		t.Errorf("readable: %s", text)
	}
	var got manaCachedReply
	if err := json.Unmarshal([]byte(runMana(t, cmd, "cached json", CommandContext{})), &got); err != nil || got.HasFigures || len(got.Windows) != 0 {
		t.Errorf("json: %+v %v", got, err)
	}
	// No history at all (no store): the same answer, no panic.
	if text := runMana(t, ManaCommand(nil, delegator.UsageTarget{}, false), "cached", CommandContext{}); !strings.Contains(text, "holds no usage figures") {
		t.Errorf("no store: %s", text)
	}
}

// TestManaCached_NoLift is H6 (requirement 5): stored figures showing usage
// available never lift the usage hold or the rate-limit gate; a fresh read
// of the same does (#3043, unchanged).
func TestManaCached_NoLift(t *testing.T) {
	a := &agent.Agent{AgentID: "cam", UsageHoldRegistry: usagehold.New()}
	a.DelegatedManager = &agent.DelegatedManager{BackendType: "claude-code"}
	a.ObserveUsage(usagehold.Event{Window: "five_hour", Reached: true, ResetAt: time.Now().Add(2 * time.Hour)})
	a.EngageRateLimit(ratelimit.Signal{Kind: ratelimit.KindUsage, ResetAt: time.Now().Add(2 * time.Hour)})
	gateClosed := func() bool { lim, _ := a.SessionRateLimited("agent:cam:main"); return lim }
	if a.UsageHold().Phase != usagehold.Stop || !gateClosed() {
		t.Fatalf("setup: phase %v, gate closed %v", a.UsageHold().Phase, gateClosed())
	}

	now := time.Now()
	st, hist := testHistory(t, a.UsageAccount(), now)
	storeReading(t, st, a.UsageAccount(), now.Add(-time.Minute), delegator.UsageSourceScheduled,
		pctWindow("five_hour", 5, now.Add(time.Hour)), pctWindow("seven_day", 5, now.Add(48*time.Hour)))
	read := func(context.Context, delegator.UsageTarget) (*delegator.UsageInfo, error) {
		return &delegator.UsageInfo{Backend: "Claude Code", RateLimitsAvailable: true,
			FiveHour: delegator.UsageWindow{Percent: 5, Reported: true}, SevenDay: delegator.UsageWindow{Percent: 5, Reported: true}}, nil
	}
	cmd := ManaCommand(read, delegator.UsageTarget{AgentID: "cam"}, true, hist)

	text := runMana(t, cmd, "cached", CommandContext{Agent: a})
	if a.UsageHold().Phase != usagehold.Stop || !gateClosed() || strings.Contains(text, "Lifted") {
		t.Errorf("after /mana cached: phase %v, gate closed %v; want both still on\n%s", a.UsageHold().Phase, gateClosed(), text)
	}
	runMana(t, cmd, "", CommandContext{Agent: a})
	if a.UsageHold().Phase != usagehold.Open || gateClosed() {
		t.Errorf("after plain /mana at 5%%: phase %v, gate closed %v; want both lifted", a.UsageHold().Phase, gateClosed())
	}
}

// TestMana_FreshReadIsStored (requirement 6, the /mana source): a fresh read
// is stored with source mana and its payload; a failed read stores nothing.
func TestMana_FreshReadIsStored(t *testing.T) {
	now := time.Date(2026, 11, 13, 21, 0, 0, 0, time.UTC)
	st, hist := testHistory(t, "claude-code:A", now)
	raw := json.RawMessage(`{"rate_limits":{"five_hour":{"utilization":12,"resets_at":"2026-11-13T23:00:00Z"}},"extra":"kept"}`)
	ok := func(context.Context, delegator.UsageTarget) (*delegator.UsageInfo, error) {
		return &delegator.UsageInfo{Backend: "Claude Code", Raw: raw,
			FiveHour: delegator.UsageWindow{Percent: 12, Reported: true, ResetsAt: now.Add(2 * time.Hour)}}, nil
	}
	runMana(t, ManaCommand(ok, delegator.UsageTarget{AgentID: "cam"}, false, hist), "", CommandContext{})
	failing := func(context.Context, delegator.UsageTarget) (*delegator.UsageInfo, error) {
		return nil, errors.New("boom")
	}
	if _, err := ManaCommand(failing, delegator.UsageTarget{AgentID: "cam"}, false, hist).Execute(context.Background(), Request{Name: "mana"}, CommandContext{}); err == nil {
		t.Fatal("a failed read returned no error")
	}
	reps, err := st.Reports("")
	if err != nil {
		t.Fatal(err)
	}
	if len(reps) != 1 {
		t.Fatalf("stored %d reports, want 1 (the failed read stores nothing)", len(reps))
	}
	r := reps[0]
	if r.Source != delegator.UsageSourceMana || r.Account != "claude-code:A" || r.AgentID != "cam" || r.Backend != "claude-code" ||
		!r.ReceivedAt.Equal(now) || string(r.Payload) != string(raw) || len(r.Windows) != 1 || *r.Windows[0].Used != 12 {
		t.Errorf("stored = %+v", r)
	}
}

// TestManaCached_AfterRestart is H8 (requirement 10): a reopened store
// answers /mana cached with the stored figures, aged from their original
// received times, before any new read.
func TestManaCached_AfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-history.db")
	received := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	st, err := usagelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	storeReading(t, st, "A", received, delegator.UsageSourceScheduled, pctWindow("five_hour", 44, received.Add(3*time.Hour)))
	_ = st.Close()

	st2, err := usagelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	now := received.Add(90 * time.Minute)
	hist := &usagelog.Binding{Store: st2, Backend: "claude-code", Account: "A", AgentID: "cam", Now: func() time.Time { return now }}
	never := func(context.Context, delegator.UsageTarget) (*delegator.UsageInfo, error) {
		t.Error("a read was made")
		return nil, errors.New("no")
	}
	text := runMana(t, ManaCommand(never, delegator.UsageTarget{}, false, hist), "cached", CommandContext{})
	if !strings.Contains(text, "44%") || !strings.Contains(text, "received 1h 30m ago") {
		t.Errorf("after reopen:\n%s", text)
	}
}

// TestManaCached_CodexAccountsDoNotMix is H10 (requirement 7) at the
// command, for codex: two agents, two accounts; each reply shows only its
// own figures, each bucket's windows with their lengths.
func TestManaCached_CodexAccountsDoNotMix(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	st, err := usagelog.Open(filepath.Join(t.TempDir(), "usage-history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	bind := func(agentID string) *usagelog.Binding {
		return &usagelog.Binding{Store: st, Backend: "codex", Account: usagelog.AccountKey("codex", agentID, ""), AgentID: agentID, Now: func() time.Time { return now }}
	}
	x, y := bind("x"), bind("y")
	x.Record(delegator.UsageReport{Source: delegator.UsageSourceCodexUpdate, Windows: []delegator.UsageReportWindow{
		{Window: "codex/5h", Length: 5 * time.Hour, Used: pf(34), UsedUnit: delegator.UsageUnitPercent, ResetsAt: now.Add(time.Hour)},
		{Window: "codex/7d", Length: 7 * 24 * time.Hour, Used: pf(12), UsedUnit: delegator.UsageUnitPercent, ResetsAt: now.Add(48 * time.Hour)},
	}})
	y.Record(delegator.UsageReport{Source: delegator.UsageSourceCodexUpdate, Windows: []delegator.UsageReportWindow{
		{Window: "codex/5h", Length: 5 * time.Hour, Used: pf(91), UsedUnit: delegator.UsageUnitPercent, ResetsAt: now.Add(time.Hour)},
	}})
	var gx, gy manaCachedReply
	xJSON := runMana(t, ManaCommand(nil, delegator.UsageTarget{}, false, x), "cached json", CommandContext{})
	t.Logf("codex agent x, /mana cached json:\n%s", xJSON)
	_ = json.Unmarshal([]byte(xJSON), &gx)
	_ = json.Unmarshal([]byte(runMana(t, ManaCommand(nil, delegator.UsageTarget{}, false, y), "cached json", CommandContext{})), &gy)
	if len(gx.Windows) != 2 || *gx.Windows[0].UsedPercent != 34 || gx.Windows[0].LengthSeconds != 18000 || gx.Windows[1].LengthSeconds != 604800 {
		t.Errorf("x = %+v", gx)
	}
	if len(gy.Windows) != 1 || *gy.Windows[0].UsedPercent != 91 {
		t.Errorf("y = %+v", gy)
	}
	reps, _ := st.Reports(y.Account)
	for _, r := range reps {
		if r.AgentID != "y" {
			t.Errorf("a report of agent %s is stored under y's account", r.AgentID)
		}
	}
}

// TestManaHelp_DocumentsCached (requirement 11): /help mana describes
// /mana cached and its keys.
func TestManaHelp_DocumentsCached(t *testing.T) {
	r := NewRegistry()
	r.Register(ManaCommand(nil, delegator.UsageTarget{}, false))
	help := HelpCommand(r)
	resp, err := help.Execute(context.Background(), Request{Name: "help", Args: "mana"}, CommandContext{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cached", "/mana cached json", "`used_percent`", "`resets_at`", "`used_received_at`", "`length_seconds`"} {
		if !strings.Contains(resp.Text, want) {
			t.Errorf("/help mana lacks %q:\n%s", want, resp.Text)
		}
	}
}
