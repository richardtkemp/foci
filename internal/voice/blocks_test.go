package voice

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const blocksMsg = "Here is the flow.\n\n```mermaid\ngraph TD\n  A-->B\n```\n\nAnd the numbers:\n\n| plan | price |\n|---|---|\n| a | 10 |\n| b | 20 |\n\nThe code:\n\n```go\nfunc main() {}\n```\nDone."

// Each block is replaced, in place, by its summary; prose is kept in order.
func TestSpeakBlocks_SummariesInPlace(t *testing.T) {
	sum := func(_ context.Context, b Block) (string, error) {
		switch b.Kind {
		case BlockDiagram:
			if !strings.Contains(b.Source, "A-->B") {
				t.Errorf("diagram source = %q", b.Source)
			}
			return "A flowchart of two steps", nil
		case BlockTable:
			if !strings.HasPrefix(b.Source, "| plan | price |") || !strings.Contains(b.Source, "| b | 20 |") {
				t.Errorf("table source = %q", b.Source)
			}
			return "A table of plan prices.", nil
		default:
			if b.Lang != "go" || b.Source != "func main() {}" {
				t.Errorf("code block = %+v", b)
			}
			return "An empty Go program", nil
		}
	}
	got := SpeakBlocks(context.Background(), blocksMsg, sum, 0)
	want := "Here is the flow.\n\nA flowchart of two steps.\n\nAnd the numbers:\n\nA table of plan prices.\n\nThe code:\n\nAn empty Go program.\nDone."
	if got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
}

// No summariser, a failing one, or an empty answer: the marker, never the
// source.
func TestSpeakBlocks_MarkersOnFailure(t *testing.T) {
	want := "Here is the flow.\n\nDiagram omitted.\n\nAnd the numbers:\n\nTable omitted.\n\nThe code:\n\nCode omitted.\nDone."
	for name, sum := range map[string]BlockSummariser{
		"nil":   nil,
		"error": func(context.Context, Block) (string, error) { return "x", errors.New("down") },
		"empty": func(context.Context, Block) (string, error) { return "  ", nil },
	} {
		t.Run(name, func(t *testing.T) {
			if got := SpeakBlocks(context.Background(), blocksMsg, sum, 0); got != want {
				t.Errorf("got\n%q\nwant\n%q", got, want)
			}
		})
	}
}

// A summariser slower than the bound, even one that ignores its context, does
// not hold the bubble: the markers are spoken once the bound passes.
func TestSpeakBlocks_BoundFallsBackToMarker(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	slow := func(context.Context, Block) (string, error) {
		<-release
		return "too late", nil
	}
	start := time.Now()
	got := SpeakBlocks(context.Background(), "x\n\n| a | b |\n|---|---|\n| 1 | 2 |", slow, 50*time.Millisecond)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s, want about the 50ms bound", d)
	}
	if got != "x\n\nTable omitted." {
		t.Errorf("got %q", got)
	}
}

// Text with no block is returned as it is, with no model call.
func TestSpeakBlocks_NoBlockNoCall(t *testing.T) {
	var calls atomic.Int32
	sum := func(context.Context, Block) (string, error) { calls.Add(1); return "s", nil }
	for _, text := range []string{
		"plain words",
		"a | b is a pipe in prose\n---", // a rule under a pipe line: no delimiter pipe, not a table
		"inline `code` and **bold**",
		"",
	} {
		if got := SpeakBlocks(context.Background(), text, sum, 0); got != text {
			t.Errorf("%q -> %q, want unchanged", text, got)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d summariser calls, want 0", n)
	}
}

func TestSplitSpeechSegments(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		kinds string // per segment: p (prose), d, t, c
	}{
		{"unclosed fence runs to the end", "a\n```python\nx = 1\nmore", "pc"},
		{"tilde fence, mermaid any case", "~~~Mermaid\ngraph\n~~~\nafter", "dp"},
		{"longer closing fence closes", "````\ncode\n`````\nafter", "cp"},
		{"shorter fence inside does not close", "````\n```\nstill code\n````", "c"},
		{"table without outer pipes", "x\na | b\n--- | ---\n1 | 2\n\ny", "ptp"},
		{"table ends at a line without a pipe", "| a |\n|:-:|\n| 1 |\nprose", "tp"},
		{"bare fence is code", "```\nls\n```", "c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got strings.Builder
			for _, s := range splitSpeechSegments(tc.text) {
				if s.block == nil {
					got.WriteString("p")
					continue
				}
				got.WriteString(string(s.block.Kind)[:1])
			}
			if got.String() != tc.kinds {
				t.Errorf("segments = %q, want %q", got.String(), tc.kinds)
			}
		})
	}
}
