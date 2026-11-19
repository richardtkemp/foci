package codex

import (
	"strings"
	"testing"

	"foci/internal/delegator"
)

// toolEnds records each OnToolEnd's output by item id.
func toolEnds(b *Backend) map[string]string {
	got := map[string]string{}
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnToolEnd: func(id, _, output string, _ bool) { got[id] = output },
	})
	return got
}

// codex sends a command's output on item/completed (aggregatedOutput,
// codex-cli 0.145.0 schema); OnToolEnd carries it like the other backends'
// tool results, so the own-material index (#3185) sees what the session read.
func TestItemCompleted_CommandOutputReachesToolEnd(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	got := toolEnds(b)
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"commandExecution","id":"c1","status":"completed","command":"cat memory/a.md","aggregatedOutput":"line one\nline two\n"}}}`))
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"commandExecution","id":"c2","status":"declined","command":"rm x","aggregatedOutput":null}}}`))
	if got["c1"] != "line one\nline two\n" {
		t.Fatalf("output = %q", got["c1"])
	}
	if out, ok := got["c2"]; !ok || out != "" {
		t.Fatalf("null output = %q (ended %v)", out, ok)
	}
}

// A completed fileChange's result is each change's content (add) or unified
// diff (update), never a deleted file's content; a change that was not
// applied has none.
func TestItemCompleted_FileChangeOutputIsTheWrittenText(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	got := toolEnds(b)
	changes := `[{"path":"/w/memory/new.md","kind":{"type":"add"},"diff":"# New\n- added note line\n"},
		{"path":"/w/memory/a.md","kind":{"type":"update","move_path":null},"diff":"@@ -1,2 +1,3 @@\n # A\n+- an appended note line\n"},
		{"path":"/w/memory/old.md","kind":{"type":"delete"},"diff":"deleted body text\n"}]`
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"fileChange","id":"f1","status":"completed","changes":` + changes + `}}}`))
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"fileChange","id":"f2","status":"declined","changes":` + changes + `}}}`))
	out := got["f1"]
	for _, want := range []string{"add /w/memory/new.md", "- added note line", "update /w/memory/a.md", "+- an appended note line", "delete /w/memory/old.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "deleted body text") {
		t.Errorf("a deleted file's content reads as written:\n%s", out)
	}
	if got["f2"] != "" {
		t.Errorf("declined change has output %q", got["f2"])
	}
}
