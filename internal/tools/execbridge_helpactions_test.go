package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// helpFlagLine returns the flags-table line whose flag column is exactly flag
// (or flag|--alias). Matching the first whitespace-delimited field keeps it
// strict: --body does not match --body-file, --query does not match --query-x.
// It fatals when no such line exists, so an assertion on a missing line can
// never pass vacuously.
func helpFlagLine(t *testing.T, help, flag string) string {
	t.Helper()
	for _, line := range strings.Split(help, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" {
			continue
		}
		field := strings.Fields(trimmed)[0]
		if field != flag && !strings.HasPrefix(field, flag+"|") {
			continue
		}
		return trimmed
	}
	t.Fatalf("no %s flags-table line in help:\n%s", flag, help)
	return ""
}

// TestRemindHelpOmitsTodoFlagActions proves #2244 on the generic help path:
// the [actions: ...] annotation is derived from foci_todo's allowlist keyed by
// flag NAME, so remind's same-named --text inherited it even though remind has
// no actions — telling the reader --text is only valid with foci_todo's.
func TestRemindHelpOmitsTodoFlagActions(t *testing.T) {
	t.Parallel()
	help := generateHelpText(testRemindTool(t))
	if line := helpFlagLine(t, help, "--text"); strings.Contains(line, "[actions:") {
		t.Errorf("remind --text line carries foci_todo's [actions: ...] annotation, but remind has no actions:\n  %s", line)
	}
	if strings.Contains(help, "[actions:") {
		t.Errorf("remind --help must not contain any [actions: ...] annotation — they belong to foci_todo alone:\n%s", help)
	}
}

// TestHTTPRequestHelpOmitsTodoFlagActions proves #2244 on the hand-rolled help
// path: http_request's --body and --query share names with foci_todo flags and
// so carried todo's action lists in `foci_http_request --help`.
func TestHTTPRequestHelpOmitsTodoFlagActions(t *testing.T) {
	t.Parallel()
	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 0 }, func() int64 { return 0 }, func() int64 { return 0 }, nil, 0640)
	help := generateHelpText(tool)
	for _, flag := range []string{"--body", "--query"} {
		if line := helpFlagLine(t, help, flag); strings.Contains(line, "[actions:") {
			t.Errorf("http_request %s line carries foci_todo's [actions: ...] annotation, but http_request has no actions:\n  %s", flag, line)
		}
	}
	if strings.Contains(help, "[actions:") {
		t.Errorf("http_request --help must not contain any [actions: ...] annotation — they belong to foci_todo alone:\n%s", help)
	}
}

// TestMemorySearchHelpOmitsTodoFlagActions proves #2244 for memory_search:
// its --sort flag ends with its own enum suffix and must not be followed by
// foci_todo's [actions: list, list-all, search] annotation.
func TestMemorySearchHelpOmitsTodoFlagActions(t *testing.T) {
	t.Parallel()
	tool, _ := testMemoryTool(t)
	help := generateHelpText(tool)
	line := helpFlagLine(t, help, "--sort")
	if !strings.HasSuffix(line, "[relevance|newest|oldest]") {
		t.Errorf("memory_search --sort line must end with its enum suffix [relevance|newest|oldest]:\n  %s", line)
	}
	if strings.Contains(line, "[actions:") {
		t.Errorf("memory_search --sort line carries foci_todo's [actions: ...] annotation, but memory_search has no actions:\n  %s", line)
	}
}

// TestGenericToolHelpOmitsTodoFlagActions proves #2244 for the synthetic worst
// case: a generic tool with an UNDOCUMENTED text parameter (no description)
// rendered a bare [actions: ...] annotation as the flag's whole help line,
// because the annotation was appended to the description before the
// description-empty check. The documented tag line is checked too.
func TestGenericToolHelpOmitsTodoFlagActions(t *testing.T) {
	t.Parallel()
	tool := &Tool{
		Name:       "x",
		ExecExport: true,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"text": {"type": "string"},
				"tag": {"type": "string", "description": "A tag"}
			}
		}`),
	}
	help := generateHelpText(tool)
	if line := helpFlagLine(t, help, "--text"); line != "--text" {
		t.Errorf("tool x: undocumented --text must render as the bare flag, no annotation; got %q", line)
	}
	if line := helpFlagLine(t, help, "--tag"); strings.Contains(line, "[actions:") {
		t.Errorf("tool x --tag line carries foci_todo's [actions: ...] annotation, but tool x has no actions:\n  %s", line)
	}
	if strings.Contains(help, "[actions:") {
		t.Errorf("tool x --help must not contain any [actions: ...] annotation — they belong to foci_todo alone:\n%s", help)
	}
}

// TestShellFuncBodiesOmitTodoFlagActions proves #2244 through the public path:
// both shell-function generators embed generateHelpText's output verbatim in
// the body's --help echo, so remind (generic path) and http_request (hand-
// rolled path) shipped the annotation to every `foci_* --help` invocation.
func TestShellFuncBodiesOmitTodoFlagActions(t *testing.T) {
	t.Parallel()
	if body := generateShellFunc(testRemindTool(t)); strings.Contains(body, "[actions:") {
		t.Errorf("foci_remind shell function (generic path) embeds foci_todo's [actions: ...] annotation in its --help text:\n%s", body)
	}
	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 0 }, func() int64 { return 0 }, func() int64 { return 0 }, nil, 0640)
	if body := generateShellFunc(tool); strings.Contains(body, "[actions:") {
		t.Errorf("foci_http_request shell function (hand-rolled path) embeds foci_todo's [actions: ...] annotation in its --help text:\n%s", body)
	}
}

// TestTodoHelpFlagActionsPinned is the characterisation half of #2244: scoping
// the annotation to foci_todo must not change foci_todo's own help. Pins the
// exact annotations on --text and --status. (The ticket also asked for
// `--body [actions: add]`, but todo's schema has no `body` property — --body
// exists only in the shell layer — so no such line exists to pin; see
// .factory/NOTES.md.) Passes before and after the fix.
func TestTodoHelpFlagActionsPinned(t *testing.T) {
	t.Parallel()
	store := newTestTodoStore(t)
	help := generateHelpText(NewTodoTool(store, "agent-help"))
	for _, pin := range []struct{ flag, want string }{
		{"--text", "[actions: add, complete, drop, edit]"},
		{"--status", "[actions: list]"},
	} {
		line := helpFlagLine(t, help, pin.flag)
		if !strings.HasSuffix(line, pin.want) {
			t.Errorf("todo %s line must still end with %s — foci_todo's own help is unchanged:\n  %s", pin.flag, pin.want, line)
		}
	}
}
