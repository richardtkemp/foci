package command

import (
	"context"
	"strings"
	"sync"
	"testing"

	"foci/internal/log"
)

// gateCC builds a CommandContext whose telegram policy has operators 111 and
// permits exactly the named commands to everyone else.
func gateCC(reg *Registry, permitted []string) CommandContext {
	return CommandContext{
		Operators: OperatorPolicies{
			SourceTelegram: opTestPolicy(reg, []string{"111"}, permitted),
		},
	}
}

// restrictedReq is a telegram request from non-operator 222.
func restrictedReq(name, args string) Request {
	return Request{Name: name, Args: args, Source: SourceTelegram, UserID: "222", ChatID: 1}
}

// dispatchFromSender dispatches req as the sender it names — the
// typed-command shape, where the gate caller is the request's own sender.
func dispatchFromSender(reg *Registry, req Request, cc CommandContext) (Response, bool, error) {
	return reg.Dispatch(context.Background(), req, cc)
}

func TestDispatch_ForbiddenCommandRefused(t *testing.T) {
	// Proves the defence-in-depth layer: Registry.Dispatch itself (no
	// dispatcher involved) refuses a forbidden command — nothing executes —
	// the reply names the command, an alias is refused under its command's
	// canonical name, and a name that is not a registered command keeps
	// today's "Unknown command" reply (the gate refuses commands, not typos).
	reg := opTestRegistry()
	cc := gateCC(reg, []string{"status"})

	var runs int
	if cmd := reg.Get("tmux"); cmd != nil {
		inner := cmd.Execute
		cmd.Execute = func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			runs++
			return inner(ctx, req, cc)
		}
	}

	resp, handled, err := dispatchFromSender(reg, restrictedReq("tmux", ""), cc)
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if !handled {
		t.Fatal("refusal must be handled, not passed to the agent as text")
	}
	if resp.Text != "/tmux is only for this agent's operators." {
		t.Errorf("refusal = %q", resp.Text)
	}
	if runs != 0 {
		t.Errorf("forbidden command executed %d times, want 0", runs)
	}

	resp, _, _ = dispatchFromSender(reg, restrictedReq("usage", ""), cc)
	if resp.Text != "/mana is only for this agent's operators." {
		t.Errorf("alias refusal = %q, want the canonical /mana refusal", resp.Text)
	}

	// A name that is not a registered command keeps today's "Unknown command"
	// reply for the same restricted user — the gate refuses commands, not
	// typos.
	resp, handled, _ = dispatchFromSender(reg, restrictedReq("nope", ""), cc)
	if !handled {
		t.Fatal("unknown command must stay handled")
	}
	if resp.Text != "Unknown command /nope. Type /help to see available commands." {
		t.Errorf("unknown reply = %q", resp.Text)
	}

	// The permitted command still runs for the restricted user.
	resp, handled, _ = dispatchFromSender(reg, restrictedReq("status", ""), cc)
	if !handled || resp.Text != "ok" {
		t.Errorf("permitted dispatch = (%q, %v), want (ok, true)", resp.Text, handled)
	}
}

func TestDispatch_RefusalLogsWarn(t *testing.T) {
	// Proves a refusal is logged at WARN on the agent's logger naming the
	// user and the canonical command — for a typed command and for the
	// "/help <forbidden command>" reply alike, which go through the same
	// refusal site — and that permitted and operator calls log nothing.
	reg := opTestRegistry()
	cc := gateCC(reg, []string{"status", "help"})

	var (
		mu      sync.Mutex
		entries []struct{ level, component, msg string }
	)
	log.SetWarnHook(func(level log.Level, component, msg string) {
		mu.Lock()
		defer mu.Unlock()
		entries = append(entries, struct{ level, component, msg string }{level.String(), component, msg})
	})
	t.Cleanup(func() { log.SetWarnHook(nil) })
	// Installing the hook replays warnings buffered earlier in the test
	// binary; only refusals dispatched below count.
	mu.Lock()
	entries = nil
	mu.Unlock()

	_, _, _ = dispatchFromSender(reg, restrictedReq("tmux", ""), cc)
	_, _, _ = dispatchFromSender(reg, restrictedReq("help", "tmux"), cc)
	_, _, _ = dispatchFromSender(reg, restrictedReq("status", ""), cc)
	_, _, _ = dispatchFromSender(reg, restrictedReq("help", "status"), cc)
	// The hidden /wait alias of stop, refused under stop's canonical name.
	_, _, _ = dispatchFromSender(reg, restrictedReq("wait", ""), cc)
	_, _, _ = dispatchFromSender(reg,
		Request{Name: "tmux", Source: SourceTelegram, UserID: "111", ChatID: 1}, cc)

	mu.Lock()
	defer mu.Unlock()
	wantRefusals := map[string]int{
		"operator-only command refused: user=222 command=/tmux": 2, // typed + /help tmux
		"operator-only command refused: user=222 command=/stop": 1,
	}
	for _, e := range entries {
		if e.level != "WARN" || e.component != "agent:scout" {
			continue
		}
		if _, want := wantRefusals[e.msg]; want {
			wantRefusals[e.msg]--
		}
	}
	for msg, left := range wantRefusals {
		if left != 0 {
			t.Errorf("want the exact count of refusal WARN %q, %d missing or extra in %+v", msg, left, entries)
		}
	}
}

func TestHelpListsOnlyPermittedCommands(t *testing.T) {
	// Proves /help lists exactly what a restricted user may run — nothing
	// more, nothing less.
	reg := opTestRegistry()
	cc := gateCC(reg, []string{"help", "reset", "stop"})

	resp, _, _ := dispatchFromSender(reg, restrictedReq("help", ""), cc)
	for _, want := range []string{"help", "reset", "stop"} {
		if !strings.Contains(resp.Text, "| /"+want+" |") {
			t.Errorf("restricted /help missing /%s:\n%s", want, resp.Text)
		}
	}
	for _, banned := range []string{"tmux", "status", "mana"} {
		if strings.Contains(resp.Text, "/"+banned) {
			t.Errorf("restricted /help must not list /%s:\n%s", banned, resp.Text)
		}
	}
}

func TestHelpDetailForbiddenSaysOperatorsOnly(t *testing.T) {
	// Proves "/help <forbidden command>" answers the operator-only reply
	// instead of describing a command the caller may not run.
	reg := opTestRegistry()
	cc := gateCC(reg, []string{"status", "help"})

	resp, _, _ := dispatchFromSender(reg, restrictedReq("help", "tmux"), cc)
	if resp.Text != "/tmux is only for this agent's operators." {
		t.Errorf("help detail for forbidden command = %q", resp.Text)
	}

	// Detail for a permitted command still works.
	resp, _, _ = dispatchFromSender(reg, restrictedReq("help", "status"), cc)
	if !strings.Contains(resp.Text, "**/status**") {
		t.Errorf("help detail for permitted command = %q", resp.Text)
	}
}

func TestHelp_ExplicitEmptyListRefusesHelp(t *testing.T) {
	// Proves an explicitly empty non_operator_commands list forbids /help too:
	// non-operators may run no command at all.
	reg := opTestRegistry()
	cc := gateCC(reg, []string{})

	resp, handled, _ := dispatchFromSender(reg, restrictedReq("help", ""), cc)
	if !handled {
		t.Fatal("refusal must be handled")
	}
	if resp.Text != "/help is only for this agent's operators." {
		t.Errorf("refusal = %q, want /help refused", resp.Text)
	}
}

func TestSuggestionsOnlyPermitted(t *testing.T) {
	// Proves "Did you mean" offers only commands the caller may run: a typo
	// of a permitted command suggests it, a typo of a forbidden one falls
	// back to the /help pointer.
	reg := opTestRegistry()
	cc := gateCC(reg, []string{"help", "reset", "stop"})

	resp, _, _ := dispatchFromSender(reg, restrictedReq("helq", ""), cc)
	if !strings.Contains(resp.Text, "Did you mean /help?") {
		t.Errorf("typo of permitted command = %q, want a /help suggestion", resp.Text)
	}

	resp, _, _ = dispatchFromSender(reg, restrictedReq("statu", ""), cc)
	if strings.Contains(resp.Text, "/status") {
		t.Errorf("typo of forbidden command suggested /status: %q", resp.Text)
	}
	if !strings.Contains(resp.Text, "Type /help to see available commands.") {
		t.Errorf("typo of forbidden command = %q, want the /help pointer", resp.Text)
	}
}

func TestSuggestCommandOrderDeterministic(t *testing.T) {
	// Proves the "Did you mean" list is deterministic: equal-distance
	// matches (registry iteration order is map order) are ordered
	// alphabetically, so the same typo always gets the same reply.
	reg := NewRegistry()
	for _, name := range []string{"aaa", "aab", "aac", "aad", "aae", "aaf"} {
		reg.Register(&Command{Name: name})
	}

	want := "Unknown command /aag. Did you mean /aaa, /aab, /aac?"
	for i := 0; i < 50; i++ {
		if got := reg.suggestCommand("aag", Caller{}, CommandContext{}); got != want {
			t.Fatalf("suggestCommand call %d = %q, want %q", i, got, want)
		}
	}
}

func TestSuggestCommandOrdersByDistanceThenName(t *testing.T) {
	// Proves "Did you mean" sorts by distance first, name only as the
	// tiebreak: the closer match leads the list even when it sorts later
	// alphabetically (yyy is one edit from "byy", bbb is two).
	reg := NewRegistry()
	reg.Register(&Command{Name: "bbb"})
	reg.Register(&Command{Name: "yyy"})

	got := reg.suggestCommand("byy", Caller{}, CommandContext{})
	want := "Unknown command /byy. Did you mean /yyy, /bbb?"
	if got != want {
		t.Errorf("suggestCommand = %q, want %q", got, want)
	}
}

func TestAvailableRespectsOperatorGate(t *testing.T) {
	// Proves the listing predicate itself carries the gate, so every surface
	// filtering through Available (/help, the app palette, the Telegram menu)
	// hides forbidden commands from a restricted caller.
	reg := opTestRegistry()
	cc := gateCC(reg, []string{"status"})

	req := restrictedReq("", "")
	if Available(context.Background(), reg.Get("tmux"), req, cc) {
		t.Error("tmux must not be available to a restricted caller")
	}
	if !Available(context.Background(), reg.Get("status"), req, cc) {
		t.Error("status must stay available to a restricted caller")
	}

	// The operator and an unidentified caller see everything.
	opReq := Request{Source: SourceTelegram, UserID: "111"}
	if !Available(context.Background(), reg.Get("tmux"), opReq, cc) {
		t.Error("tmux must be available to the operator")
	}
	if !Available(context.Background(), reg.Get("tmux"), Request{}, cc) {
		t.Error("tmux must stay available to an unidentified caller (the Telegram menu)")
	}
}

func TestLookupKeyboard_ForbiddenShowsNothing(t *testing.T) {
	// Proves a forbidden bare command shows no keyboard and runs none of the
	// keyboard machinery (Visible, KeyboardOptions, KeyboardHeader) — no
	// button, no data.
	reg := opTestRegistry()
	cc := gateCC(reg, []string{"status"})

	var kbOpts, kbHeader, visible int
	cmd := reg.Get("tmux")
	cmd.KeyboardOptions = func(context.Context, CommandContext) []KeyboardOption {
		kbOpts++
		return []KeyboardOption{{Label: "kill", Data: "kill"}}
	}
	cmd.KeyboardHeader = func(context.Context, Request, CommandContext) string {
		kbHeader++
		return "panes:"
	}
	cmd.Visible = func(context.Context, Request, CommandContext) bool {
		visible++
		return true
	}

	if _, _, _, ok := reg.LookupKeyboard(context.Background(), "/tmux", restrictedReq("", "").Caller(), cc); ok {
		t.Error("forbidden command must not show a keyboard")
	}
	if kbOpts != 0 || kbHeader != 0 || visible != 0 {
		t.Errorf("keyboard machinery ran (opts=%d header=%d visible=%d), want all 0", kbOpts, kbHeader, visible)
	}

	// The operator still gets the keyboard.
	if _, _, _, ok := reg.LookupKeyboard(context.Background(), "/tmux",
		Caller{Source: SourceTelegram, UserID: "111"}, cc); !ok {
		t.Error("operator must still get the keyboard")
	}
}

func TestLookupChainKeyboard_ForbiddenShowsNothing(t *testing.T) {
	// Proves a forbidden chained command (e.g. /tmux kill) shows no follow-up
	// keyboard and never runs ChainKeyboard — no pane or section listing.
	reg := opTestRegistry()
	cc := gateCC(reg, []string{"status"})

	var chains int
	cmd := reg.Get("tmux")
	cmd.ChainKeyboard = func(_ context.Context, sub string, _ CommandContext) []KeyboardOption {
		chains++
		if sub == "kill" {
			return []KeyboardOption{{Label: "sess-a", Data: "kill sess-a"}}
		}
		return nil
	}

	if _, _, ok := reg.LookupChainKeyboard(context.Background(), "/tmux kill", restrictedReq("", "").Caller(), cc); ok {
		t.Error("forbidden command must not show a chained keyboard")
	}
	if chains != 0 {
		t.Errorf("ChainKeyboard ran %d times, want 0 (no data listing)", chains)
	}

	if _, _, ok := reg.LookupChainKeyboard(context.Background(), "/tmux kill",
		Caller{Source: SourceTelegram, UserID: "111"}, cc); !ok {
		t.Error("operator must still get the chained keyboard")
	}
}

// operatorlessCC is a policy entry with NO operators configured: it must
// restrict nobody, whatever the command list says.
func operatorlessCC(reg *Registry) CommandContext {
	return CommandContext{Operators: OperatorPolicies{SourceTelegram: opTestPolicy(reg, nil, nil)}}
}

func TestHelp_NoOperatorsListingUnchanged(t *testing.T) {
	// Characterises the unset case: with no operators configured the /help
	// table is byte-identical for the very user who would be restricted once
	// operators exist.
	reg := opTestRegistry()

	plainResp, _, _ := dispatchFromSender(reg, restrictedReq("help", ""), CommandContext{})
	gatedResp, _, _ := dispatchFromSender(reg, restrictedReq("help", ""), operatorlessCC(reg))
	if plainResp.Text != gatedResp.Text {
		t.Errorf("help output changed by an operator-less policy:\n%s\n---\n%s", plainResp.Text, gatedResp.Text)
	}
}

func TestSuggestions_NoOperatorsUnchanged(t *testing.T) {
	// Characterises the unset case: "Did you mean" text is identical with no
	// operators configured.
	reg := opTestRegistry()

	plainResp, _, _ := dispatchFromSender(reg, restrictedReq("statu", ""), CommandContext{})
	gatedResp, _, _ := dispatchFromSender(reg, restrictedReq("statu", ""), operatorlessCC(reg))
	if plainResp.Text != gatedResp.Text {
		t.Errorf("suggestion text changed by an operator-less policy:\n%q vs %q", plainResp.Text, gatedResp.Text)
	}
}

func TestLookupKeyboards_NoOperatorsUnchanged(t *testing.T) {
	// Characterises the unset case: keyboards and chained keyboards are
	// shown exactly as before when nothing is configured.
	reg := opTestRegistry()
	gated := operatorlessCC(reg)

	if _, _, _, ok := reg.LookupKeyboard(context.Background(), "/tmux", restrictedReq("", "").Caller(), CommandContext{}); !ok {
		t.Fatal("baseline: keyboard expected for /tmux")
	}
	if _, _, _, ok := reg.LookupKeyboard(context.Background(), "/tmux", restrictedReq("", "").Caller(), gated); !ok {
		t.Error("operator-less policy must not hide keyboards")
	}
	if _, _, ok := reg.LookupChainKeyboard(context.Background(), "/tmux kill", restrictedReq("", "").Caller(), gated); !ok {
		t.Error("operator-less policy must not hide chained keyboards")
	}
}

func TestDispatch_NoOperatorsRunsEverything(t *testing.T) {
	// Characterises the unset case: a nil/empty/operator-less cc.Operators
	// lets every command run exactly as before.
	reg := opTestRegistry()
	for _, cc := range []CommandContext{
		{},
		{Operators: OperatorPolicies{}},
		{Operators: OperatorPolicies{SourceTelegram: nil}},
		operatorlessCC(reg),
	} {
		resp, handled, _ := dispatchFromSender(reg, restrictedReq("tmux", ""), cc)
		if !handled || resp.Text != "tmux ran" {
			t.Errorf("dispatch under operators=%v = (%q, %v), want the command to run", cc.Operators, resp.Text, handled)
		}
	}
}

func TestDispatch_OtherSourcesNotGated(t *testing.T) {
	// Characterises the exempt surfaces: the same restricted user's ID on
	// the http, app and unidentified sources still runs every command —
	// /send, /command, /status, the deferred sweep, the suggestion buttons
	// and the app transport are not gated by a chat-platform policy.
	reg := opTestRegistry()
	cc := gateCC(reg, []string{"status"})
	for _, src := range []string{SourceHTTP, "app", ""} {
		req := Request{Name: "tmux", Source: src, UserID: "222"}
		resp, handled, _ := dispatchFromSender(reg, req, cc)
		if !handled || resp.Text != "tmux ran" {
			t.Errorf("source %q dispatch = (%q, %v), want the command to run", src, resp.Text, handled)
		}
	}
}
