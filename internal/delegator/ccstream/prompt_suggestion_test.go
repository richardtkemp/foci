package ccstream

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
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
