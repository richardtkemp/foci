package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// todoAddParams runs `foci_todo add ...` through the generated bash with
// foci-call stubbed, and returns the params object it would have sent. A
// non-zero exit is returned with a nil map and the combined output.
func todoAddParams(t *testing.T, invocation string) (map[string]any, string, int) {
	t.Helper()
	body := generateShellFunc(NewTodoTool(nil, "agent-shell"))
	out, code := runShellFunc(t, body, invocation)
	if code != 0 {
		return nil, out, code
	}
	_, payload, ok := strings.Cut(out, "FOCI_CALL_REACHED:")
	if !ok {
		t.Fatalf("foci-call not reached\n%s", out)
	}
	var call struct {
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &call); err != nil {
		t.Fatalf("decode foci-call payload: %v\n%s", err, out)
	}
	return call.Params, out, code
}

// TestTodoAddShellPositionalTitleWithBody is the #2075 repro: `add "<title>"
// --body "<text>"` dropped the positional because --body overwrote text, so
// #2072/#2073 were created with no title and list showed the body's first line.
// With --body present, the bare argument is the title.
func TestTodoAddShellPositionalTitleWithBody(t *testing.T) {
	t.Parallel()
	p, out, code := todoAddParams(t, `foci_todo add "Fix the thing" --priority low --tag foci --body "Details here."`)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if p["title"] != "Fix the thing" || p["text"] != "Details here." {
		t.Errorf("want title=%q text=%q, got %v", "Fix the thing", "Details here.", p)
	}
}

// TestTodoAddShellForms pins the other add shapes, so the #2075 change cannot
// quietly move where their words land.
func TestTodoAddShellForms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, args  string
		title, text any // nil = key absent
	}{
		{"bare words are the text", `fix the thing`, nil, "fix the thing"},
		{"--text", `--text "the text"`, nil, "the text"},
		{"--body alone", `--body "the text"`, nil, "the text"},
		{"--title with --body", `--title T --body B`, "T", "B"},
		{"--title with bare words", `--title T some words`, "T", "some words"},
		{"--title alone", `--title T`, "T", nil},
		{"multi-word positional title with --body", `fix the thing --body B`, "fix the thing", "B"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p, out, code := todoAddParams(t, "foci_todo add "+c.args)
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			if p["title"] != c.title || p["text"] != c.text {
				t.Errorf("want title=%v text=%v, got %v", c.title, c.text, p)
			}
		})
	}
}

// TestTodoAddShellRejectsAmbiguousText: combinations where one of the values
// used to be dropped without a word are now refused before foci-call.
func TestTodoAddShellRejectsAmbiguousText(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, args, want string }{
		{"--text and --body", `--text A --body B`, "--text OR --body"},
		{"bare words, --body and --title", `words --title T --body B`, "already given as --title"},
		{"bare words and --text", `words --text A`, "--text OR bare words"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, out, code := todoAddParams(t, "foci_todo add "+c.args)
			if code == 0 {
				t.Fatalf("expected a non-zero exit\n%s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("error does not mention %q\n%s", c.want, out)
			}
			if strings.Contains(out, "FOCI_CALL_REACHED") {
				t.Errorf("a rejected add still reached foci-call\n%s", out)
			}
		})
	}
}

// TestTodoAddToolHonoursTitle: the tool schema advertises title on add, but
// todoAdd ignored it, so a direct tool call with a title stored the text alone
// (#2075's tool-side twin). It must compose the same "*Title*" headline the
// shell used to, which is the form splitTodoTitle and retitleItemText read.
func TestTodoAddToolHonoursTitle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		p     map[string]any
		want  string
		title string
	}{
		{"title and text", map[string]any{"title": "Headline", "text": "Body text."}, "*Headline*\n\nBody text.", "Headline"},
		{"title only", map[string]any{"title": "Headline"}, "*Headline*", "Headline"},
		{"text only", map[string]any{"text": "Just text"}, "Just text", "Just text"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			store := newTestTodoStore(t)
			tool := NewTodoTool(store, "agent-title")
			c.p["action"] = "add"
			if _, err := executeTodoTool(tool, c.p); err != nil {
				t.Fatalf("add: %v", err)
			}
			items, _ := store.List("agent-title", "", nil, "", "", false, 0)
			if len(items) != 1 {
				t.Fatalf("want one item, got %+v", items)
			}
			if items[0].Text != c.want {
				t.Errorf("stored text = %q, want %q", items[0].Text, c.want)
			}
			if got, _ := splitTodoTitle(items[0].Text); got != c.title {
				t.Errorf("title = %q, want %q", got, c.title)
			}
		})
	}
}

// TestTodoAddToolRequiresTextOrTitle: an add with neither is still refused.
func TestTodoAddToolRequiresTextOrTitle(t *testing.T) {
	t.Parallel()
	tool := NewTodoTool(newTestTodoStore(t), "agent-empty")
	if _, err := executeTodoTool(tool, map[string]any{"action": "add"}); err == nil {
		t.Error("add with neither text nor title succeeded")
	}
}
