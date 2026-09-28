package stoprule

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Transcript lines in the shapes CC 2.1.280 writes (probed live for #2089).
const (
	prevPrompt = `{"type":"user","promptId":"p0","message":{"role":"user","content":"earlier"}}`
	prevBgCall = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"old","name":"Bash","input":{"command":"sleep 99","run_in_background":true}}]}}`
	prompt     = `{"type":"user","promptId":"p1","message":{"role":"user","content":"do two things"}}`
	attachment = `{"type":"attachment","attachment":{"type":"prompt_snapshot","systemPrompt":["long"]}}`
	fgCall     = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"fg","name":"Bash","input":{"command":"ls"}}]}}`
	fgResult   = `{"type":"user","promptId":"p1","message":{"content":[{"type":"tool_result","tool_use_id":"fg","content":"x"}]},"toolUseResult":{"stdout":"x","stderr":""}}`
	bashCall   = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"sleep 5","run_in_background":true}}]}}`
	bashResult = `{"type":"user","promptId":"p1","message":{"content":[{"type":"tool_result","tool_use_id":"b1","content":"Command running in background with ID: x"}]},"toolUseResult":{"stdout":"","backgroundTaskId":"x"}}`
	// The Agent tool backgrounds by default: no run_in_background in its
	// input, only the async_launched result says so.
	agentCall   = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a1","name":"Agent","input":{"description":"d","prompt":"p"}}]}}`
	agentResult = `{"type":"user","promptId":"p1","message":{"content":[{"type":"tool_result","tool_use_id":"a1","content":[{"type":"text","text":"Async agent launched"}]}]},"toolUseResult":{"isAsync":true,"status":"async_launched","agentId":"ag"}}`
	sideBgCall  = `{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"tool_use","id":"s1","name":"Bash","input":{"command":"x","run_in_background":true}}]}}`
	finalText   = `{"type":"assistant","message":{"content":[{"type":"text","text":"Both started."}]}}`
)

func launches(t *testing.T, promptID string, lines ...string) []string {
	t.Helper()
	ids, err := TurnLaunches(strings.NewReader(strings.Join(lines, "\n")+"\n"), promptID)
	if err != nil {
		t.Fatalf("TurnLaunches: %v", err)
	}
	return ids
}

func TestTurnLaunches(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  []string
	}{
		{"nothing launched", []string{prompt, attachment, fgCall, fgResult, finalText}, nil},
		{"background bash", []string{prompt, bashCall, bashResult, finalText}, []string{"b1"}},
		{"default-background agent", []string{prompt, agentCall, agentResult, finalText}, []string{"a1"}},
		{"both", []string{prompt, bashCall, bashResult, agentCall, agentResult, finalText}, []string{"b1", "a1"}},
		{"earlier turn's launch does not count", []string{prevPrompt, prevBgCall, prompt, fgCall, fgResult, finalText}, nil},
		{"sidechain launch does not count", []string{prompt, sideBgCall, finalText}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := launches(t, "p1", tc.lines...); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("launches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTurnLaunches_TurnNotFound(t *testing.T) {
	for _, pid := range []string{"", "missing"} {
		_, err := TurnLaunches(strings.NewReader(prompt+"\n"+bashCall+"\n"), pid)
		if !errors.Is(err, ErrTurnNotFound) {
			t.Errorf("pid %q: err = %v, want ErrTurnNotFound", pid, err)
		}
	}
}

func TestMatchAndReason(t *testing.T) {
	rules := []Rule{
		{Name: "quiet", Text: []string{"never-matches"}},
		{Name: "announce", Text: []string{`(?i)\bstarting (the )?\w+ now\b`}},
	}
	h := Match(rules, "Read it all. Starting the deploy now.")
	if h == nil || h.Rule.Name != "announce" || h.Match != "Starting the deploy now" {
		t.Fatalf("hit = %+v", h)
	}
	if got := h.ReasonFor(); !strings.Contains(got, `"Starting the deploy now"`) || !strings.Contains(got, "deferred") {
		t.Errorf("default reason = %q", got)
	}
	rules[1].Reason = "You said {match}."
	if got := Match(rules, "starting the build now").ReasonFor(); got != "You said starting the build now." {
		t.Errorf("custom reason = %q", got)
	}
	if Match(rules, "The deploy finished.") != nil {
		t.Error("non-matching text matched")
	}
}

func TestExcerptKeepsUTF8(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes each
	if got := Excerpt(s, 5); got != "éé…" {
		t.Errorf("Excerpt = %q", got)
	}
	if got := tail(s, 5); got != "…éé" {
		t.Errorf("tail = %q", got)
	}
}

func TestResolveDropsDisabledAndInvalid(t *testing.T) {
	off := false
	rules, skipped := Resolve([]Rule{
		{Name: "on", Text: []string{"x"}},
		{Name: "off", Text: []string{"x"}, Enabled: &off},
		{Name: "bad", Text: []string{"("}},
	})
	if len(rules) != 1 || rules[0].Name != "on" || len(skipped) != 1 || !strings.Contains(skipped[0], "bad") {
		t.Errorf("rules=%+v skipped=%v", rules, skipped)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	in := []Rule{{Name: "n", Text: []string{"a", "b"}, Reason: "r"}}
	enc, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Decode(enc)
	if err != nil || !reflect.DeepEqual(out, in) {
		t.Errorf("round trip = %+v err=%v", out, err)
	}
}
