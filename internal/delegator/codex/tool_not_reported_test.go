package codex

import (
	"testing"

	"foci/internal/delegator"
)

// TestToolEnd_NotReportedFlaggedPerSite (#3188): each codex item whose result
// foci does not read ends through OnToolEndNotReported, so the app card and
// the Langfuse span say "not reported"; an item whose output foci does read
// ends through OnToolEnd, even when that output is empty (a command that
// printed nothing), and a tool with no output by nature (sleep) is not
// flagged either.
func TestToolEnd_NotReportedFlaggedPerSite(t *testing.T) {
	t.Parallel()
	cases := []struct {
		site, item, name string
		notReported      bool
	}{
		{"mcp tool", `{"type":"mcpToolCall","id":"x","status":"completed","server":"s","tool":"t","arguments":{}}`, "mcp:s.t", true},
		{"dynamic tool", `{"type":"dynamicToolCall","id":"x","status":"completed","namespace":"ns","tool":"t","success":true}`, "ns.t", true},
		{"web search", `{"type":"webSearch","id":"x","query":"q"}`, "web_search", true},
		{"image generation", `{"type":"imageGeneration","id":"x","status":"completed"}`, "image_gen", true},
		{"unmapped tool", `{"type":"brandNewThing","id":"x","status":"completed"}`, "brandNewThing", true},
		{"command with empty output", `{"type":"commandExecution","id":"x","status":"completed","command":"true","aggregatedOutput":""}`, "bash", false},
		{"sleep", `{"type":"sleep","id":"x","durationMs":1000}`, sleepToolName, false},
	}
	for _, tc := range cases {
		t.Run(tc.site, func(t *testing.T) {
			t.Parallel()
			b := newTestBackend(t)
			b.itemCache = make(map[string]itemEnvelope)
			var ends, notReported []string
			b.sessionEvents.Store(&delegator.SessionEvents{
				OnToolEnd: func(_, name, output string, _ bool) {
					ends = append(ends, name)
					if output != "" {
						t.Errorf("unexpected output %q", output)
					}
				},
				OnToolEndNotReported: func(_, name string, _ bool) { notReported = append(notReported, name) },
			})
			b.dispatch([]byte(`{"method":"item/started","params":{"threadId":"th_1","turnId":"tu_1","item":` + tc.item + `}}`))
			b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":` + tc.item + `}}`))

			got, other := ends, notReported
			if tc.notReported {
				got, other = notReported, ends
			}
			if len(got) != 1 || got[0] != tc.name || len(other) != 0 {
				t.Errorf("ends = %q, not-reported ends = %q; want one %s end for %q", ends, notReported,
					map[bool]string{true: "not-reported", false: "plain"}[tc.notReported], tc.name)
			}
		})
	}
}
