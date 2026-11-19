package memsearch

import (
	"strings"
	"testing"
)

func has(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

const noteLine = "- Working domain from the NUC is annas-archive.pk (checked 14:50)."

// The write shapes the census found (tools/memwrite-census): a Bash heredoc
// (98%), an Edit/Write, and `echo "…" >>`. Each must yield the line the file
// gets.
func TestExtractLines_WriteShapes(t *testing.T) {
	cases := map[string]string{
		"cc bash heredoc (JSON input)": `{"command":"cat >> memory/2026-11-19.md <<'EOF'\n## Notes\n` + noteLine + `\nEOF","description":"note"}`,
		"cc edit new_string":           `{"file_path":"/x/memory/a.md","old_string":"x","new_string":"intro\n` + noteLine + `\n"}`,
		"cc write content":             `{"file_path":"/x/memory/a.md","content":"# T\n\n` + noteLine + `\n"}`,
		"echo double-quoted":           `{"command":"echo \"` + noteLine + `\" >> memory/a.md"}`,
		"echo single-quoted":           `{"command":"echo '` + noteLine + `' >> memory/a.md"}`,
		"codex raw command line":       "cat >> memory/a.md <<'EOF'\n" + noteLine + "\nEOF",
		"tool result text":             "line one\n   " + noteLine + "   \n",
	}
	for name, in := range cases {
		if got := ExtractLines(in); !has(got, noteLine) {
			t.Errorf("%s: %q lacks the note line", name, got)
		}
	}
}

func TestExtractLines_SkipsShortLines(t *testing.T) {
	got := ExtractLines("short line\n## Heading that is short\n" + strings.Repeat("x", MinLineRunes-1))
	if len(got) != 0 {
		t.Fatalf("got %q, want none", got)
	}
	if got := ExtractLines(strings.Repeat("é", MinLineRunes)); len(got) != 1 {
		t.Fatalf("a %d-rune line of 2-byte runes: %q", MinLineRunes, got)
	}
}

func TestExtractLines_EchoUnescapes(t *testing.T) {
	in := `{"command":"echo \"- the \\\"quoted\\\" word costs \\$5 in this sentence here\" >> m.md"}`
	if got := ExtractLines(in); !has(got, `- the "quoted" word costs $5 in this sentence here`) {
		t.Fatalf("got %q", got)
	}
}

func TestProbes_LongestDistinct(t *testing.T) {
	a := strings.Repeat("a", 60)
	b := strings.Repeat("b", 50)
	c := strings.Repeat("c", 45)
	d := strings.Repeat("d", 41)
	got := Probes(strings.Join([]string{d, a, "short", c, a, b}, "\n"))
	if strings.Join(got, ",") != strings.Join([]string{a, b, c}, ",") {
		t.Fatalf("probes = %q", got)
	}
	if probesNeeded(3) != 2 || probesNeeded(1) != 1 || probesNeeded(0) != 0 {
		t.Fatal("probesNeeded")
	}
}

func TestHashLine_TrimsSpace(t *testing.T) {
	if HashLine("  "+noteLine+"\t") != HashLine(noteLine) {
		t.Fatal("hash depends on surrounding space")
	}
	if HashLine(noteLine) == HashLine(noteLine+".") {
		t.Fatal("different lines share a hash")
	}
}

// A unified diff's added line counts without its "+" (a codex fileChange
// result, #3185); the "+++" header and removed lines do not.
func TestExtractLines_UnifiedDiffAddedLines(t *testing.T) {
	diff := "update /w/memory/a.md\n--- a/memory/a.md\n+++ b/memory/a.md/with/a/long/enough/path/to/pass/forty\n@@ -1,2 +1,3 @@\n # A\n+" + noteLine + "\n-- an old line that was removed from the file today\n"
	got := ExtractLines(diff)
	if !has(got, noteLine) {
		t.Fatalf("%q lacks the added line", got)
	}
	if has(got, "/w/memory/a.md/with/a/long/enough/path/to/pass/forty") || has(got, "- an old line that was removed from the file today") {
		t.Fatalf("header or removed line taken as written: %q", got)
	}
}
