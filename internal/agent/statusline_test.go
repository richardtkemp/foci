package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/tools"
)

// renderTmpl is a small helper to render an arbitrary template against a bare
// (no-store) agent.
func renderTmpl(tmpl string, in statuslineInputs) string {
	a := &Agent{}
	in.agent = a
	return a.renderStatusline(context.Background(), tmpl, in)
}

// TestStatuslineDefault_FirstMessage proves the default template reproduces the
// historical first-message [meta] line exactly: no prev_cost/prev_tokens,
// and the empty [state] line is dropped.
func TestStatuslineDefault_FirstMessage(t *testing.T) {
	now := time.Date(2026, 2, 21, 5, 30, 0, 0, time.UTC)
	got := renderTmpl(DefaultStatuslineTemplate, statuslineInputs{
		now: now, model: "claude-haiku-4-5", platform: "api", sm: &sessionMeta{},
	})
	want := "[meta] time=2026-02-21T05:30:00Z gap=none model=claude-haiku-4-5 via=api"
	if got != want {
		t.Errorf("first-message statusline:\n got: %q\nwant: %q", got, want)
	}
}

// TestStatuslineDefault_FullLine proves the default [meta] line even with prior
// cost/token data present carries only time/gap/model/via — prev_cost and
// prev_tokens are deliberately not in the default template.
func TestStatuslineDefault_FullLine(t *testing.T) {
	now := time.Date(2026, 2, 21, 5, 30, 0, 0, time.UTC)
	sm := &sessionMeta{
		lastMessageTime: now.Add(-2 * time.Minute),
		prevCost:        0.043,
		prevInput:       2400,
		prevOutput:      312,
		prevCacheRead:   18000,
		prevCacheWrite:  200,
	}
	got := renderTmpl(DefaultStatuslineTemplate, statuslineInputs{
		now: now, model: "claude-opus-4-8", platform: "telegram", sm: sm,
	})
	want := "[meta] time=2026-02-21T05:30:00Z gap=2m0s model=claude-opus-4-8 via=telegram"
	if got != want {
		t.Errorf("full statusline:\n got: %q\nwant: %q", got, want)
	}
}

// TestStatuslineModelSpellingCollapses is the #1629 regression: the model value
// reaches the statusline from three sources that spell one model three ways, and
// which source wins is positional (ResolveModelEffort falls back to the
// agent-level name until the first turn completes). Observed in ONE session with
// no model change: `opus`, then `claude-opus-5`, then `claude/claude-opus-5`
// across four consecutive messages.
//
// The provider-qualified and bare forms must render identically. A bare ALIAS
// must NOT be forced to join them — foci has not yet been told what it resolved
// to, and inventing that mapping here would hardcode a guess that goes stale
// whenever an alias moves. Both halves are asserted, because a fix that collapsed
// everything would pass the first half alone while shipping a lie.
func TestStatuslineModelSpellingCollapses(t *testing.T) {
	now := time.Date(2026, 2, 21, 5, 30, 0, 0, time.UTC)
	render := func(model string) string {
		return renderTmpl("{model}", statuslineInputs{
			now: now, model: model, platform: "app", sm: &sessionMeta{},
		})
	}

	// The two spellings of a known model collapse.
	for _, tc := range []struct{ stored, want string }{
		{"claude/claude-opus-5", "claude-opus-5"},
		{"claude-opus-5", "claude-opus-5"},
		{"zai-coding-plan/glm-5.2", "glm-5.2"},
		{"glm-5.2", "glm-5.2"},
	} {
		if got := render(tc.stored); got != tc.want {
			t.Errorf("model %q rendered %q, want %q — the provider prefix must not survive to the header", tc.stored, got, tc.want)
		}
	}

	// A bare alias is left alone: it is what is actually known pre-first-turn.
	if got := render("opus"); got != "opus" {
		t.Errorf("alias rendered %q, want %q — an unresolved alias must not be guessed into a concrete id", got, "opus")
	}

	// An empty model must not acquire a value.
	if got := render(""); got != "" {
		t.Errorf("empty model rendered %q, want empty", got)
	}
}

// TestStatuslineLineDrop proves rule 3: a line whose every placeholder rendered
// empty is dropped, while a line with at least one non-empty placeholder stays.
func TestStatuslineLineDrop(t *testing.T) {
	now := time.Date(2026, 2, 21, 5, 30, 0, 0, time.UTC)
	tmpl := "[meta] {model}\n[empty] {cost}{tokens}\n[lit] no placeholders here"
	got := renderTmpl(tmpl, statuslineInputs{now: now, model: "m", platform: "api", sm: &sessionMeta{}})
	// Line 1 kept (model non-empty), line 2 dropped (all empty), line 3 kept (pure literal).
	want := "[meta] m\n[lit] no placeholders here"
	if got != want {
		t.Errorf("line-drop:\n got: %q\nwant: %q", got, want)
	}
}

// TestStatuslinePausedAsk proves the default template's [ask] line renders only
// while an ask is paused, names the ask id, and is dropped (rule 3) otherwise.
func TestStatuslinePausedAsk(t *testing.T) {
	now := time.Date(2026, 2, 21, 5, 30, 0, 0, time.UTC)
	paused := true
	a := &Agent{AskRouter: &tools.AskRouter{
		IsPaused:          func(string) bool { return paused },
		PendingForSession: func(string) string { return "ask-test-7" },
	}}
	in := statuslineInputs{now: now, model: "m", platform: "api", sm: &sessionMeta{}, agent: a, sessionKey: "s"}

	got := a.renderStatusline(context.Background(), DefaultStatuslineTemplate, in)
	if !strings.Contains(got, "[ask] ") || !strings.Contains(got, "ask-test-7") {
		t.Errorf("paused statusline should carry an [ask] line naming the ask id:\n%s", got)
	}

	paused = false
	got = a.renderStatusline(context.Background(), DefaultStatuslineTemplate, in)
	if strings.Contains(got, "[ask]") {
		t.Errorf("[ask] line should be dropped when no ask is paused:\n%s", got)
	}
}

// TestStatuslineUnknownField proves an unknown {field} is left verbatim (so
// typos are visible) and counts as a literal, not a placeholder (line kept).
func TestStatuslineUnknownField(t *testing.T) {
	now := time.Date(2026, 2, 21, 5, 30, 0, 0, time.UTC)
	got := renderTmpl("x={nope}", statuslineInputs{now: now, sm: &sessionMeta{}})
	if got != "x={nope}" {
		t.Errorf("unknown field should be verbatim: got %q", got)
	}
}

// TestStatuslineBareFields proves the bare/raw fields always render (no
// self-omission, no label) even on the first message.
func TestStatuslineBareFields(t *testing.T) {
	now := time.Date(2026, 2, 21, 5, 30, 0, 0, time.UTC)
	sm := &sessionMeta{prevInput: 7, prevOutput: 3, prevCacheRead: 100, prevCacheWrite: 9}
	got := renderTmpl("in={tokens_in} out={tokens_out} cR={cache_read} cW={cache_write} c={cost_raw}",
		statuslineInputs{now: now, sm: sm})
	want := "in=7 out=3 cR=100 cW=9 c=$0"
	if got != want {
		t.Errorf("bare fields:\n got: %q\nwant: %q", got, want)
	}
}

// TestStatuslineCommandEmbedding proves ${...} embeds stdout, flattens newlines,
// and that a failing command embeds nothing (and drops an all-empty line).
func TestStatuslineCommandEmbedding(t *testing.T) {
	now := time.Date(2026, 2, 21, 5, 30, 0, 0, time.UTC)
	in := statuslineInputs{now: now, sm: &sessionMeta{}}

	if got := renderTmpl("out=${printf 'a\\nb'}", in); got != "out=a b" {
		t.Errorf("command embedding: got %q, want %q", got, "out=a b")
	}
	// Failing command → empty; the line is all-placeholder-empty → dropped.
	if got := renderTmpl("${exit 1}", in); got != "" {
		t.Errorf("failing command line should drop: got %q", got)
	}
	// Failing command beside a literal+field → line kept, command contributes "".
	if got := renderTmpl("v={model}${exit 1}", statuslineInputs{now: now, model: "x", sm: &sessionMeta{}}); got != "v=x" {
		t.Errorf("mixed line: got %q, want %q", got, "v=x")
	}
}

// TestStatuslineCommandTimeout proves a command that exceeds the timeout embeds
// nothing (rather than blocking the turn); a present field keeps the line.
func TestStatuslineCommandTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skips the multi-second timeout in -short")
	}
	now := time.Date(2026, 2, 21, 5, 30, 0, 0, time.UTC)
	got := renderTmpl("v={model} x=${sleep 10}", statuslineInputs{now: now, model: "m", sm: &sessionMeta{}})
	if got != "v=m x=" {
		t.Errorf("timed-out command should embed nothing: got %q", got)
	}
}

func TestCollapseStatuslineSpaces(t *testing.T) {
	cases := map[string]string{
		"a  b":      "a b",
		"  x  y  ":  "x y",
		"no change": "no change",
		"a 🟢 b":     "a 🟢 b",
		"":          "",
	}
	for in, want := range cases {
		if got := collapseStatuslineSpaces(in); got != want {
			t.Errorf("collapse(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestStatuslineCommandWaitDelaySet is a guard that the per-turn command runner
// sets WaitDelay (the backstop for a child that outlives the kill).
func TestStatuslineCommandWaitDelaySet(t *testing.T) {
	if statuslineCmdWaitKill <= 0 {
		t.Fatal("statuslineCmdWaitKill must be positive to bound a hung child")
	}
	if !strings.Contains(DefaultStatuslineTemplate, "{state}") {
		t.Error("default template should include {state} for the dashboard line")
	}
}
