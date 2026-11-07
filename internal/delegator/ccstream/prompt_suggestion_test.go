package ccstream

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
)

// The wire shape is from a live CC 2.1.314 run (#2916).
func TestReaderDispatchPromptSuggestion(t *testing.T) {
	t.Parallel()
	line := `{"type":"prompt_suggestion","suggestion":"find guesthouses in Omalos","uuid":"u1","session_id":"s1"}` + "\n"

	h := &mockHandler{}
	NewReader(strings.NewReader(line), h).Run(context.Background())

	if len(h.suggestions) != 1 || h.suggestions[0].Suggestion != "find guesthouses in Omalos" {
		t.Fatalf("suggestions = %+v, want the one from the line", h.suggestions)
	}
}

func TestOnPromptSuggestion_ReachesSessionBetweenTurns(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	b.suggestionsOn.Store(true)
	var got []string
	b.AttachSessionEvents(&delegator.SessionEvents{OnPromptSuggestion: func(s string) { got = append(got, s) }})

	b.OnPromptSuggestion(&PromptSuggestionMessage{Suggestion: "  run the tests "})

	if len(got) != 1 || got[0] != "run the tests" {
		t.Fatalf("got %q, want the trimmed suggestion once", got)
	}
}

// A suggestion already on the pipe when the next input began a turn predicts a
// message the user has passed.
func TestOnPromptSuggestion_DroppedWhileTurnInFlight(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	b.suggestionsOn.Store(true)
	var got []string
	b.AttachSessionEvents(&delegator.SessionEvents{OnPromptSuggestion: func(s string) { got = append(got, s) }})
	b.turnMu.Lock()
	b.turnActive = true
	b.turnMu.Unlock()

	b.OnPromptSuggestion(&PromptSuggestionMessage{Suggestion: "run the tests"})

	if len(got) != 0 {
		t.Fatalf("got %q during a turn, want nothing", got)
	}
}

func TestOnPromptSuggestion_EmptyAndUnwiredAreSilent(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	b.suggestionsOn.Store(true)
	b.OnPromptSuggestion(&PromptSuggestionMessage{Suggestion: "x"}) // no events attached: no panic

	var got []string
	b.AttachSessionEvents(&delegator.SessionEvents{OnPromptSuggestion: func(s string) { got = append(got, s) }})
	b.OnPromptSuggestion(&PromptSuggestionMessage{Suggestion: "  "})
	if len(got) != 0 {
		t.Fatalf("got %q for a blank suggestion, want nothing", got)
	}
}

// startArgs launches the recorder script through Backend.Start and returns the
// argv CC would have received.
func startArgs(t *testing.T, opts delegator.StartOptions) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	out := filepath.Join(dir, "args.txt")
	script := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$*\" > "+out+".tmp\nmv "+out+".tmp "+out+"\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write recorder: %v", err)
	}
	d, err := newFromConfig(map[string]any{"binary": script})
	if err != nil {
		t.Fatalf("newFromConfig: %v", err)
	}
	be := d.(*Backend)
	opts.WorkDir, opts.AgentID = dir, "argstest"
	if err := be.Start(context.Background(), opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(out); err == nil {
			return string(data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("recorder wrote no argv")
	return ""
}

func TestStart_PromptSuggestionsFlagFollowsOption(t *testing.T) {
	if got := startArgs(t, delegator.StartOptions{PromptSuggestions: true}); !strings.Contains(got, "--prompt-suggestions true") {
		t.Errorf("with the option, argv = %q, want --prompt-suggestions true", got)
	}
	if got := startArgs(t, delegator.StartOptions{}); strings.Contains(got, "--prompt-suggestions") {
		t.Errorf("without the option, argv = %q, want no flag", got)
	}
}

// A suggestion that reaches foci while the switch is off belongs to a turn that
// was not the user's: it must not replace the chip, even when CC ignored the
// switch.
func TestOnPromptSuggestion_DroppedWhileSwitchOff(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	var got []string
	b.AttachSessionEvents(&delegator.SessionEvents{OnPromptSuggestion: func(s string) { got = append(got, s) }})

	b.OnPromptSuggestion(&PromptSuggestionMessage{Suggestion: "run the tests"})

	if len(got) != 0 {
		t.Fatalf("got %q with the switch off, want nothing", got)
	}
}

// A new process starts with the switch off, so a cold session's first turn, of
// whatever kind, costs nothing until a user's turn asks.
func TestStart_SwitchStartsOff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	out := filepath.Join(dir, "stdin.txt")
	script := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec cat > "+out+"\n"), 0o755); err != nil {
		t.Fatalf("write recorder: %v", err)
	}
	start := func(flag bool) string {
		_ = os.Remove(out)
		d, err := newFromConfig(map[string]any{"binary": script})
		if err != nil {
			t.Fatalf("newFromConfig: %v", err)
		}
		be := d.(*Backend)
		if err := be.Start(context.Background(), delegator.StartOptions{WorkDir: dir, AgentID: "switchtest", PromptSuggestions: flag}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { _ = be.Close() })
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(out)
			lines := strings.Count(string(data), "\n")
			if lines >= 2 || (!flag && lines >= 1) {
				time.Sleep(50 * time.Millisecond)
				data, _ = os.ReadFile(out)
				return string(data)
			}
			time.Sleep(20 * time.Millisecond)
		}
		data, _ := os.ReadFile(out)
		t.Fatalf("flag=%v: stdin = %q, want the initialize line and, with the flag, the switch", flag, data)
		return ""
	}
	got := start(true)
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) < 2 || summariseStdin(t, lines[1]) != "env="+promptSuggestionOff {
		t.Fatalf("with the flag, stdin = %q, want initialize then the switch off", got)
	}
	if got := start(false); strings.Contains(got, promptSuggestionEnv) {
		t.Fatalf("without the flag, stdin = %q, want no switch", got)
	}
}

// switchBackend is a Backend with a live writer whose stdin lines are readable,
// launched with --prompt-suggestions when flag is set.
func switchBackend(t *testing.T, flag bool) (*Backend, *stdinLines) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	b := &Backend{
		writer:            NewWriter(pw),
		pendingPerms:      make(map[string]*pendingPermission),
		outstanding:       delegator.NewOutstandingRegistry(),
		promptSuggestions: flag,
	}
	b.running = true
	lines := &stdinLines{ch: make(chan string, 16)}
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			lines.ch <- sc.Text()
		}
		close(lines.ch)
	}()
	return b, lines
}

type stdinLines struct{ ch chan string }

// next returns the next stdin line's summary: "user", or the suggestion env
// value a control request set.
func (l *stdinLines) next(t *testing.T) string {
	t.Helper()
	select {
	case line, ok := <-l.ch:
		if !ok {
			t.Fatal("stdin closed")
		}
		return summariseStdin(t, line)
	case <-time.After(5 * time.Second):
		t.Fatal("no stdin line")
	}
	return ""
}

func (l *stdinLines) none(t *testing.T) {
	t.Helper()
	select {
	case line := <-l.ch:
		t.Fatalf("unexpected stdin line: %s", summariseStdin(t, line))
	case <-time.After(100 * time.Millisecond):
	}
}

func summariseStdin(t *testing.T, line string) string {
	t.Helper()
	var env struct {
		Type    string `json:"type"`
		Request struct {
			Subtype  string `json:"subtype"`
			Settings struct {
				Env map[string]string `json:"env"`
			} `json:"settings"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("stdin line %q: %v", line, err)
	}
	if env.Type == "control_request" && env.Request.Subtype == "apply_flag_settings" {
		v, ok := env.Request.Settings.Env[promptSuggestionEnv]
		if !ok {
			t.Fatalf("control without the suggestion env: %s", line)
		}
		return "env=" + v
	}
	return env.Type
}

// The switch goes ahead of the user message, so CC reads it before the turn
// ends, and is written only when it changes.
func TestBeginTurn_SwitchFollowsInject(t *testing.T) {
	t.Parallel()
	b, lines := switchBackend(t, true)
	ctx := context.Background()
	turn := func(want bool) error {
		return b.ImmediateInject(ctx, delegator.Inject{Source: delegator.SourceUser, Text: "x", Turn: &delegator.TurnEvents{}, PromptSuggestion: want})
	}
	end := func() { b.turnMu.Lock(); b.turnActive = false; b.turnMu.Unlock() }

	if err := turn(true); err != nil {
		t.Fatal(err)
	}
	if got := lines.next(t); got != "env="+promptSuggestionOn {
		t.Fatalf("first line = %q, want the switch on ahead of the message", got)
	}
	if got := lines.next(t); got != "user" {
		t.Fatalf("second line = %q, want the user message", got)
	}
	end()

	if err := turn(true); err != nil { // no change: no control
		t.Fatal(err)
	}
	if got := lines.next(t); got != "user" {
		t.Fatalf("line = %q, want only the user message when nothing changed", got)
	}
	end()

	if err := turn(false); err != nil {
		t.Fatal(err)
	}
	if got := lines.next(t); got != "env="+promptSuggestionOff {
		t.Fatalf("line = %q, want the switch off", got)
	}
	if got := lines.next(t); got != "user" {
		t.Fatalf("line = %q, want the user message", got)
	}
}

// A system turn refused because a turn runs must leave that turn's switch alone.
func TestBeginTurn_RefusedSystemTurnKeepsSwitch(t *testing.T) {
	t.Parallel()
	b, lines := switchBackend(t, true)
	b.suggestionsOn.Store(true)
	b.turnMu.Lock()
	b.turnActive = true
	b.turnMu.Unlock()

	err := b.ImmediateInject(context.Background(), delegator.Inject{Source: delegator.SourceSystem, Text: "cron", Turn: &delegator.TurnEvents{}})
	if err == nil {
		t.Fatal("system inject during a turn: want a refusal")
	}
	lines.none(t)
	if !b.suggestionsOn.Load() {
		t.Fatal("a refused system turn turned the switch off")
	}
}

// User input folding into a running run turns the switch on (the run may be an
// adopted autonomous one, which runs with it off); input that is not the user's
// on a surface leaves it alone.
func TestFold_SwitchOnlyRises(t *testing.T) {
	t.Parallel()
	b, lines := switchBackend(t, true)
	ctx := context.Background()
	b.turnMu.Lock()
	b.turnActive = true
	b.turnMu.Unlock()

	if err := b.ImmediateInject(ctx, delegator.Inject{Source: delegator.SourceSteer, Text: "spawn steer"}); err != nil {
		t.Fatal(err)
	}
	if got := lines.next(t); got != "user" {
		t.Fatalf("line = %q, want the fold alone", got)
	}
	if err := b.ImmediateInject(ctx, delegator.Inject{Source: delegator.SourceUser, Text: "hello", PromptSuggestion: true}); err != nil {
		t.Fatal(err)
	}
	if got := lines.next(t); got != "env="+promptSuggestionOn {
		t.Fatalf("line = %q, want the switch on", got)
	}
	if got := lines.next(t); got != "user" {
		t.Fatalf("line = %q, want the fold", got)
	}
	if err := b.ImmediateInject(ctx, delegator.Inject{Source: delegator.SourceSteer, Text: "spawn steer"}); err != nil {
		t.Fatal(err)
	}
	if got := lines.next(t); got != "user" {
		t.Fatalf("line = %q, want the fold alone: a fold never lowers the switch", got)
	}
}

// A session launched without the flag has no switch to work: foci writes nothing
// CC would not otherwise see.
func TestSwitch_NoFlagWritesNothing(t *testing.T) {
	t.Parallel()
	b, lines := switchBackend(t, false)
	if err := b.ImmediateInject(context.Background(), delegator.Inject{Source: delegator.SourceUser, Text: "x", Turn: &delegator.TurnEvents{}, PromptSuggestion: true}); err != nil {
		t.Fatal(err)
	}
	if got := lines.next(t); got != "user" {
		t.Fatalf("line = %q, want only the user message without the flag", got)
	}
}

// A run CC began by itself is not the user's: the switch goes off at its running
// edge, ahead of the run's end.
func TestAutonomousRun_TurnsSwitchOff(t *testing.T) {
	t.Parallel()
	b, lines := switchBackend(t, true)
	b.suggestionsOn.Store(true)
	opened := make(chan struct{}, 1)
	b.SetOnAutonomousOpen(func([]string) { opened <- struct{}{} })

	b.OnSystem("session_state_changed", []byte(`{"type":"system","subtype":"session_state_changed","state":"running","session_id":"s"}`))

	if got := lines.next(t); got != "env="+promptSuggestionOff {
		t.Fatalf("line = %q, want the switch off", got)
	}
	select {
	case <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("the run was not adopted")
	}
}

// The on value is "true", not empty (#2931). CC 2.1.314 skips a suggestion
// whenever the account's rate-limit status is not "allowed", unless the env
// value is exactly true, which lets allowed_warning through. Live on 7 Nov the
// seven-day window sat at allowed_warning (76-78%) for days, and an empty on
// value got no fork in 5 turns while "true" forked after every turn.
func TestSwitchOnValuePassesRateLimitWarning(t *testing.T) {
	t.Parallel()
	b, lines := switchBackend(t, true)
	b.setPromptSuggestions(true)
	if got := lines.next(t); got != "env=true" {
		t.Fatalf("switch on wrote %q, want env=true", got)
	}
}

// The switch, CC's answer to it, each suggestion and each drop are in the log,
// so "why no suggestion?" can be answered from foci.log (#2931).
// Not parallel: the logger is process-global.
func TestPromptSuggestion_SwitchAndDropsAreLogged(t *testing.T) {
	out := captureLog(t)
	log.SetLevel(log.DEBUG)
	t.Cleanup(func() { log.SetLevel(log.INFO) })
	b, lines := switchBackend(t, true)
	b.startOpts.SessionKey = "clutch/c1"

	b.setPromptSuggestions(true)
	_ = lines.next(t)
	b.pendingControlMu.Lock()
	var reqID string
	for id := range b.suggestionControls {
		reqID = id
	}
	b.pendingControlMu.Unlock()
	if reqID == "" {
		t.Fatal("the switch write left no request to match CC's answer to")
	}
	b.OnControlResponse(json.RawMessage(`{"type":"control_response","response":{"subtype":"success","request_id":"` + reqID + `"}}`))
	b.OnResult(&ResultMessage{Subtype: "success"})
	b.OnPromptSuggestion(&PromptSuggestionMessage{Suggestion: "run the tests"})
	// Through OnRateLimit, so its wiring to the gate is covered. Off a
	// warning boundary, so no notice fires.
	util := 0.77
	b.OnRateLimit(&RateLimitEvent{RateLimitInfo: RateLimitInfo{Status: "rejected", RateLimitType: "seven_day", Utilization: &util}})
	if got := lines.next(t); got != "env="+promptSuggestionOff {
		t.Fatalf("a rejected status wrote %q, want the switch off", got)
	}
	b.OnPromptSuggestion(&PromptSuggestionMessage{Suggestion: "late"})

	got := out.String()
	for _, want := range []string{
		"prompt suggestion switch on=true sent",
		"prompt suggestion switch on=true: CC answered success",
		"prompt suggestion: turn ended with the switch on (usage 0% (unknown), limit 90%, rate limit status unknown)",
		"prompt suggestion received (13 bytes)",
		"prompt suggestion held off: usage 77% (seven_day), limit 90%, rate limit status rejected",
		"prompt suggestion switch on=false sent",
		"prompt suggestion dropped: the switch is off",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q\nlog:\n%s", want, got)
		}
	}
}

// The usage gate (#2931, Dick's ruling Q24): the switch is on below the turn's
// limit and off at or above it, judged on the highest window CC reported, and
// it follows rate-limit events that arrive between turns or mid-turn.
func TestPromptSuggestion_UsageGate(t *testing.T) {
	t.Parallel()
	b, lines := switchBackend(t, true)
	reading := func(window, status string, util float64, resetsAt float64) {
		info := RateLimitInfo{Status: status, RateLimitType: window}
		if util >= 0 {
			info.Utilization = &util
		}
		if resetsAt != 0 {
			info.ResetsAt = &resetsAt
		}
		b.noteRateLimitForSuggestions(info)
	}
	future := float64(time.Now().Add(time.Hour).Unix())

	b.setPromptSuggestionsLimit(true, 90)
	if got := lines.next(t); got != "env=true" {
		t.Fatalf("on with no reading: wrote %q, want env=true", got)
	}
	reading("seven_day", "allowed_warning", 0.89, future)
	lines.none(t) // 89% < 90%: stays on
	reading("five_hour", "allowed_warning", 0.90, future)
	if got := lines.next(t); got != "env=false" {
		t.Fatalf("a window at the limit: wrote %q, want env=false", got)
	}
	reading("five_hour", "allowed", -1, future) // no utilization: below CC's warning level
	if got := lines.next(t); got != "env=true" {
		t.Fatalf("the highest window back at 89%%: wrote %q, want env=true", got)
	}

	// A lower limit on the next turn applies at its begin.
	b.setPromptSuggestionsLimit(true, 80)
	if got := lines.next(t); got != "env=false" {
		t.Fatalf("limit 80 with a window at 89%%: wrote %q, want env=false", got)
	}
	// A window past its reset no longer counts.
	reading("seven_day", "allowed_warning", 0.89, float64(time.Now().Add(-time.Minute).Unix()))
	if got := lines.next(t); got != "env=true" {
		t.Fatalf("the only high window has reset: wrote %q, want env=true", got)
	}
	// A turn that does not want a prediction turns it off whatever the usage.
	b.setPromptSuggestions(false)
	if got := lines.next(t); got != "env=false" {
		t.Fatalf("an unwanted turn: wrote %q, want env=false", got)
	}
	// A reading with nobody wanting a prediction writes nothing.
	reading("five_hour", "allowed", -1, future)
	lines.none(t)
	// Overage is not a plan window: a high one does not stop predictions.
	b.setPromptSuggestionsLimit(true, 90)
	if got := lines.next(t); got != "env=true" {
		t.Fatalf("on again: wrote %q, want env=true", got)
	}
	reading("overage", "allowed", 0.99, future)
	lines.none(t)
}
