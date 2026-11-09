package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rulesFixture is shaped like foci.toml's pretool_rules: an inline array of
// inline tables, one per line, with comments between them and a multi-line
// string holding '#', '[' and ',' inside one element.
const rulesFixture = `[[agents]]
id = "main"
backend_config.pretool_rules = [
  { name = "first", tool = "Bash", command = 'a( |$)', reason = "one" },
  # about second
  { name = "second", tool = "Bash", when = """
# not a comment [x], {y}
exit 0
""", reason = "two" },
  # Dick 2026-09-27: about third
  { name = "third", tool = "Edit", reason = "three" },
  { name = "fourth", tool = "Write", reason = "four" },
]
# after the array
model = "opus"
`

func mustPlan(t *testing.T) func(string, error) string {
	return func(next string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return next
	}
}

func ruleNames(t *testing.T, content string) []string {
	t.Helper()
	v, ok, err := LookupPath(content, mustPath(t, "agents[main].backend_config.pretool_rules"))
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v\n%s", ok, err, content)
	}
	arr, _ := asArray(v)
	var out []string
	for _, e := range arr {
		out = append(out, e.(map[string]any)["name"].(string))
	}
	return out
}

// TestElement_RemoveByName proves `unset arr[name]` removes exactly that
// element's lines and the comment directly above it, and leaves every other
// line of the file as it was.
func TestElement_RemoveByName(t *testing.T) {
	next := mustPlan(t)(PlanUnsetPath(rulesFixture, mustPath(t, "agents[main].backend_config.pretool_rules[third]")))
	want := strings.Replace(rulesFixture, "  # Dick 2026-09-27: about third\n  { name = \"third\", tool = \"Edit\", reason = \"three\" },\n", "", 1)
	if next != want {
		t.Errorf("got:\n%s\nwant:\n%s", next, want)
	}
	if got := DroppedComments(rulesFixture, next); len(got) != 1 || got[0] != "# Dick 2026-09-27: about third" {
		t.Errorf("DroppedComments = %q", got)
	}
}

// TestElement_RemoveMultiLineElement removes the element whose string holds
// header-like and comment-like lines; the string's '#' line is not a comment.
func TestElement_RemoveMultiLineElement(t *testing.T) {
	next := mustPlan(t)(PlanUnsetPath(rulesFixture, mustPath(t, "agents[main].backend_config.pretool_rules[name=second]")))
	if strings.Contains(next, "not a comment") || strings.Contains(next, "# about second") {
		t.Errorf("element or its comment survives:\n%s", next)
	}
	if got := strings.Join(ruleNames(t, next), ","); got != "first,third,fourth" {
		t.Errorf("names = %s", got)
	}
	for _, c := range []string{"# Dick 2026-09-27: about third", "# after the array"} {
		if !strings.Contains(next, c) {
			t.Errorf("unrelated comment %q lost:\n%s", c, next)
		}
	}
}

// TestElement_ReplaceAndAdd proves replace changes only the element's own
// text, and add appends a line in the array's style.
func TestElement_ReplaceAndAdd(t *testing.T) {
	next := mustPlan(t)(PlanSetPath(rulesFixture, mustPath(t, "agents[main].backend_config.pretool_rules[3]"), `{ name = "fourth", tool = "Write", reason = "FOUR" }`))
	want := strings.Replace(rulesFixture, `reason = "four"`, `reason = "FOUR"`, 1)
	if next != want {
		t.Errorf("replace got:\n%s", next)
	}
	next = mustPlan(t)(PlanAddElement(rulesFixture, mustPath(t, "agents[main].backend_config.pretool_rules"), `{ name = "fifth", tool = "Read", reason = "five" }`))
	want = strings.Replace(rulesFixture, "four\" },\n]", "four\" },\n  { name = \"fifth\", tool = \"Read\", reason = \"five\" },\n]", 1)
	if next != want {
		t.Errorf("add got:\n%s", next)
	}
}

// TestElement_WholeArraySetKeepsComments proves a whole-array set that drops
// one element and edits another keeps every other element's text and the
// comments between them: the value text given has no comments at all.
func TestElement_WholeArraySetKeepsComments(t *testing.T) {
	value := `[{ name = "first", tool = "Bash", command = 'a( |$)', reason = "one" }, { name = "second", tool = "Bash", when = "# not a comment [x], {y}\nexit 0\n", reason = "two" }, { name = "fourth", tool = "Write", reason = "4" }]`
	next := mustPlan(t)(PlanSetPath(rulesFixture, mustPath(t, "agents[main].backend_config.pretool_rules"), value))
	for _, keep := range []string{"# about second", `"""`, "# after the array", `{ name = "first", tool = "Bash", command = 'a( |$)', reason = "one" },`} {
		if !strings.Contains(next, keep) {
			t.Errorf("lost %q:\n%s", keep, next)
		}
	}
	if strings.Contains(next, "third") || !strings.Contains(next, `reason = "4"`) {
		t.Errorf("edit not applied:\n%s", next)
	}
}

// TestElement_SingleLineArrays covers remove at each position, add and
// string-element selection on a one-line array.
func TestElement_SingleLineArrays(t *testing.T) {
	const c = "[a]\nlist = [\"x\", \"y\", \"z\"] # note\n"
	cases := []struct{ path, want string }{
		{"a.list[x]", "[a]\nlist = [\"y\", \"z\"] # note\n"},
		{"a.list[1]", "[a]\nlist = [\"x\", \"z\"] # note\n"},
		{"a.list[\"z\"]", "[a]\nlist = [\"x\", \"y\"] # note\n"},
	}
	for _, tc := range cases {
		if got := mustPlan(t)(PlanUnsetPath(c, mustPath(t, tc.path))); got != tc.want {
			t.Errorf("unset %s:\n got %q\nwant %q", tc.path, got, tc.want)
		}
	}
	if got := mustPlan(t)(PlanAddElement(c, mustPath(t, "a.list"), `"w"`)); got != "[a]\nlist = [\"x\", \"y\", \"z\", \"w\"] # note\n" {
		t.Errorf("add: %q", got)
	}
	if got := mustPlan(t)(PlanAddElement("[a]\nlist = []\n", mustPath(t, "a.list"), `"w"`)); got != "[a]\nlist = [\"w\"]\n" {
		t.Errorf("add to empty: %q", got)
	}
	if got := mustPlan(t)(PlanAddElement("[a]\nk = 1\n", mustPath(t, "a.list"), `"w"`)); !strings.Contains(got, `list = ["w"]`) {
		t.Errorf("add to absent: %q", got)
	}
	if got := mustPlan(t)(PlanUnsetPath("[a]\nlist = [\"x\"]\n", mustPath(t, "a.list[x]"))); got != "[a]\nlist = []\n" {
		t.Errorf("remove only element: %q", got)
	}
}

// aotFixture is an array of tables with comments between and inside blocks.
const aotFixture = `# blocked paths
[[blocked_paths]]
name = "etc"
path = "/etc" # system

# root's home
[[blocked_paths]]
name = "root"
# keep me
path = "/root"

[[blocked_paths]]
name = "srv"
path = "/srv"

[other]
k = 1
`

func TestElement_ArrayOfTables(t *testing.T) {
	next := mustPlan(t)(PlanUnsetPath(aotFixture, mustPath(t, "blocked_paths[root]")))
	want := strings.Replace(aotFixture, "# root's home\n[[blocked_paths]]\nname = \"root\"\n# keep me\npath = \"/root\"\n\n", "", 1)
	if next != want {
		t.Errorf("remove got:\n%s", next)
	}

	next = mustPlan(t)(PlanSetPath(aotFixture, mustPath(t, "blocked_paths[root]"), `{name = "root", path = "/root2", mode = "ro"}`))
	want = strings.Replace(aotFixture, "path = \"/root\"\n", "path = \"/root2\"\nmode = \"ro\"\n", 1)
	if next != want {
		t.Errorf("replace got:\n%s", next)
	}

	next = mustPlan(t)(PlanAddElement(aotFixture, mustPath(t, "blocked_paths"), `{name = "tmp", path = "/tmp"}`))
	want = strings.Replace(aotFixture, "path = \"/srv\"\n", "path = \"/srv\"\n\n[[blocked_paths]]\nname = \"tmp\"\npath = \"/tmp\"\n", 1)
	if next != want {
		t.Errorf("add got:\n%s", next)
	}

	// A whole-array set keeps the blocks and comments it does not change.
	next = mustPlan(t)(PlanSetPath(aotFixture, mustPath(t, "blocked_paths"), `[{name = "etc", path = "/etc"}, {name = "root", path = "/root"}]`))
	want = strings.Replace(aotFixture, "[[blocked_paths]]\nname = \"srv\"\npath = \"/srv\"\n\n", "", 1)
	if next != want {
		t.Errorf("whole set got:\n%s", next)
	}
}

// TestElement_Refusals covers the selector errors a user can hit.
func TestElement_Refusals(t *testing.T) {
	dup := "[a]\nlist = [{name = \"x\"}, {name = \"x\"}]\n"
	for _, tc := range []struct{ content, path, want string }{
		{dup, "a.list[x]", "2 elements"},
		{dup, "a.list[y]", "no element"},
		{dup, "a.list[5]", "no element 5"},
		{"[a]\nlist = 3\n", "a.list[0]", "not an array"},
		{"[a]\nk = 1\n", "a.list[0]", "not set"},
	} {
		_, err := PlanUnsetPath(tc.content, mustPath(t, tc.path))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.path, err, tc.want)
		}
	}
	if _, err := ParseKeyPath("a.list[0].name[1]"); err == nil {
		t.Error("a second selector parsed")
	}
	if _, err := PlanAddElement(dup, mustPath(t, "a.list[x]"), `{name = "z"}`); err == nil {
		t.Error("add with a selector was accepted")
	}
}

func TestParseKeyPath_Selectors(t *testing.T) {
	for in, want := range map[string]string{
		"a.list[3]":                   "a.list[3]",
		"a.list[name=x]":              "a.list[name=x]",
		`a.list[ name = "a b" ]`:      `a.list[name="a b"]`,
		`a.list["/tmp/x"]`:            `a.list["/tmp/x"]`,
		"agents[main].rules[foo]":     "agents[main].rules[foo]",
		`agents[main].rules["a]b"]`:   `agents[main].rules["a]b"]`,
		`agents[main].rules[k='v w']`: `agents[main].rules[k="v w"]`,
	} {
		p, err := ParseKeyPath(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if p.String() != want {
			t.Errorf("%s -> %s, want %s", in, p.String(), want)
		}
		if back, err := ParseKeyPath(p.String()); err != nil || back.String() != want {
			t.Errorf("%s does not round-trip: %v", want, err)
		}
	}
}

// TestSetInFile_KeepsCommentsInsideArrayAndTrailing proves the /config set
// and app-editor writer (SetInFile) keeps a scalar's trailing comment and
// the comments inside an array it rewrites.
func TestSetInFile_KeepsCommentsInsideArrayAndTrailing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foci.toml")
	content := "[agent_loop]\nmax_tool_loops = 10 # keep trailing\nlist = [\n  \"a\",\n  # about b\n  \"b\",\n  \"c\",\n]\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SetInFile(path, SetTarget{Section: "agent_loop", Key: "max_tool_loops"}, "20", 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := SetInFile(path, SetTarget{Section: "agent_loop", Key: "list"}, `["a", "b", "d"]`, 0o640); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	want := "[agent_loop]\nmax_tool_loops = 20 # keep trailing\nlist = [\n  \"a\",\n  # about b\n  \"b\",\n  \"d\",\n]\n"
	if string(data) != want {
		t.Errorf("got:\n%s\nwant:\n%s", data, want)
	}
}

// TestPlanSetTableArray_KeepsUnchangedBlocks proves the app editor's
// object-list writer keeps the comments of blocks it does not change.
func TestPlanSetTableArray_KeepsUnchangedBlocks(t *testing.T) {
	entries := []map[string]any{
		{"name": "etc", "path": "/etc"},
		{"name": "root", "path": "/root"},
		{"name": "srv", "path": "/srv2"},
	}
	next, err := PlanSetTableArray(aotFixture, "blocked_paths", entries)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(aotFixture, `path = "/srv"`, `path = "/srv2"`, 1)
	if next != want {
		t.Errorf("got:\n%s\nwant:\n%s", next, want)
	}
}

func TestDroppedComments_IgnoresStrings(t *testing.T) {
	cur := "a = \"# not\" # yes\nb = '''\n# not either\n'''\n# gone\n"
	next := "a = \"# not\" # yes\n"
	got := DroppedComments(cur, next)
	if len(got) != 1 || got[0] != "# gone" {
		t.Errorf("DroppedComments = %q", got)
	}
}
