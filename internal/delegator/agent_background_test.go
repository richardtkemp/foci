package delegator

import (
	"encoding/json"
	"testing"
)

// TestExtractAgentBackground_AbsentMeansBackground pins CC's documented default:
// the Agent tool runs a subagent in the BACKGROUND unless run_in_background is
// explicitly false. So an ABSENT field means background.
//
// Reading absence as foreground (#1934) armed every default spawn as a
// foreground transcript tail, which the Agent PostToolUse then stopped at
// launch — before CC had written the transcript — so the subagent's entire
// token usage was absorbed by its parent turn and priced at the dearer
// unobserved-TTL rate.
//
// Bash is the opposite and must stay separate: its run_in_background genuinely
// defaults to false.
func TestExtractAgentBackground_AbsentMeansBackground(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		in   string
		want bool
	}{
		{"absent means background", `{"prompt":"do a thing"}`, true},
		{"explicit true is background", `{"run_in_background":true}`, true},
		{"explicit false is foreground", `{"run_in_background":false}`, false},
		{"unparseable falls back to background", `not json`, true},
	} {
		if got := ExtractAgentBackground(json.RawMessage(c.in)); got != c.want {
			t.Errorf("%s: ExtractAgentBackground(%s) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// TestExtractBashBackground_AbsentMeansForeground is the control: the two
// helpers were aliased, and this is why they cannot be.
func TestExtractBashBackground_AbsentMeansForeground(t *testing.T) {
	t.Parallel()
	if ExtractBashBackground(json.RawMessage(`{"command":"ls"}`)) {
		t.Error("a Bash tool_use with no run_in_background must be FOREGROUND")
	}
	if !ExtractBashBackground(json.RawMessage(`{"run_in_background":true}`)) {
		t.Error("explicit run_in_background:true must be background")
	}
}
