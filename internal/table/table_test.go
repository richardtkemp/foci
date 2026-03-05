package table

import (
	"strings"
	"testing"
)

func TestDisplayWidth(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"abc", 3},
		{"hello world", 11},
		{"中文", 4},      // 2 wide chars * 2 = 4
		{"日本語", 6},     // 3 wide chars * 2 = 6
		{"한국어", 6},     // 3 Korean chars * 2 = 6
		{"🎉", 2},       // emoji is wide
		{"a中b", 4},     // mixed: 1 + 2 + 1 = 4
		{"hello世界", 9}, // mixed: 5 + 4 = 9
		{"\t", 4},      // tab expands to 4
		{"a\tb", 5},    // a (1) + tab to 4 boundary (3 spaces) + b (1) = 5
		{"café", 4},    // é is 1 width (NFC normalized)
		{"e\u0301", 1}, // e + combining acute = 1 (combining mark is 0 width)
	}
	for _, tt := range tests {
		got := DisplayWidth(tt.in)
		if got != tt.want {
			t.Errorf("DisplayWidth(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestPadRight(t *testing.T) {
	tests := []struct {
		s       string
		width   int
		wantLen int // display width of result
	}{
		{"hello", 10, 10},
		{"hello", 5, 5},
		{"hello", 3, 5}, // already wider, no padding
		{"中文", 6, 6},    // 4 + 2 spaces = 6
		{"中文", 8, 8},    // 4 + 4 spaces = 8
	}
	for _, tt := range tests {
		got := PadRight(tt.s, tt.width)
		gotWidth := DisplayWidth(got)
		if gotWidth != tt.wantLen {
			t.Errorf("PadRight(%q, %d) = %q (width %d), want width %d", tt.s, tt.width, got, gotWidth, tt.wantLen)
		}
	}
}

func TestFormat(t *testing.T) {
	cols := []Column{
		{Header: "Name"},
		{Header: "Score", Align: AlignRight},
		{Header: "Status"},
	}
	rows := [][]string{
		{"Alice", "95", "pass"},
		{"Bob", "100", "pass"},
		{"Charlie", "82", "fail"},
	}

	got := Format(cols, rows)
	lines := strings.Split(got, "\n")

	if len(lines) != 5 { // header + sep + 3 rows
		t.Fatalf("expected 5 lines, got %d:\n%s", len(lines), got)
	}

	// Header should have "Name" left-aligned and "Score" right-aligned.
	if !strings.HasPrefix(lines[0], "Name   ") {
		t.Errorf("header should start with left-aligned Name, got: %q", lines[0])
	}
	if !strings.Contains(lines[0], "Score") {
		t.Errorf("header should contain Score, got: %q", lines[0])
	}

	// Separator should be all ─.
	for _, r := range lines[1] {
		if r != '─' {
			t.Errorf("separator should be all ─, got rune %c in: %q", r, lines[1])
			break
		}
	}

	// Score column should be right-aligned (100 not padded, 95 padded left).
	// Alice's row should have " 95" (padded).
	if !strings.Contains(lines[2], " 95") {
		t.Errorf("expected right-aligned 95, got: %q", lines[2])
	}
}

func TestFormatUnicode(t *testing.T) {
	cols := []Column{
		{Header: "Name"},
		{Header: "Value"},
	}
	rows := [][]string{
		{"日本語", "abc"},
		{"hello", "世界"},
	}

	got := Format(cols, rows)
	lines := strings.Split(got, "\n")

	// All lines (except separator) should have the same display width.
	headerW := DisplayWidth(lines[0])
	for i, line := range lines {
		if i == 1 { // separator
			continue
		}
		w := DisplayWidth(line)
		if w != headerW {
			t.Errorf("line %d display width %d != header width %d\nline: %q\nfull:\n%s",
				i, w, headerW, line, got)
		}
	}
}

func TestFormatEmptyRows(t *testing.T) {
	cols := []Column{
		{Header: "A"},
		{Header: "B"},
	}

	got := Format(cols, nil)
	lines := strings.Split(got, "\n")

	if len(lines) != 2 { // header + sep only
		t.Fatalf("expected 2 lines for empty rows, got %d:\n%s", len(lines), got)
	}
}

func TestFormatNoCols(t *testing.T) {
	got := Format(nil, [][]string{{"a", "b"}})
	if got != "" {
		t.Errorf("expected empty string for no columns, got: %q", got)
	}
}

func TestFormatSingleColumn(t *testing.T) {
	cols := []Column{{Header: "Item"}}
	rows := [][]string{{"alpha"}, {"beta"}}

	got := Format(cols, rows)
	lines := strings.Split(got, "\n")
	if len(lines) != 4 { // header + sep + 2 rows
		t.Fatalf("expected 4 lines, got %d:\n%s", len(lines), got)
	}
	if !strings.Contains(lines[0], "Item") {
		t.Errorf("header missing 'Item': %q", lines[0])
	}
}

func TestFormatMismatchedRowLengths(t *testing.T) {
	cols := []Column{{Header: "A"}, {Header: "B"}, {Header: "C"}}

	// Row shorter than columns — missing cells should render as empty
	rows := [][]string{{"x"}}
	got := Format(cols, rows)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 { // header + sep + 1 row
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), got)
	}

	// Row longer than columns — extra cells should be ignored
	rows = [][]string{{"a", "b", "c", "d", "e"}}
	got = Format(cols, rows)
	lines = strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), got)
	}
	// Extra cells should not appear
	if strings.Contains(lines[2], "d") || strings.Contains(lines[2], "e") {
		t.Errorf("extra cells should be ignored: %q", lines[2])
	}
}

func TestFormatRightAlignUnicode(t *testing.T) {
	cols := []Column{
		{Header: "Name"},
		{Header: "Count", Align: AlignRight},
	}
	rows := [][]string{
		{"日本", "42"},
		{"ab", "7"},
	}

	got := Format(cols, rows)
	lines := strings.Split(got, "\n")

	// All content lines (not separator) should have the same display width
	headerW := DisplayWidth(lines[0])
	for i, line := range lines {
		if i == 1 {
			continue
		}
		w := DisplayWidth(line)
		if w != headerW {
			t.Errorf("line %d display width %d != header width %d\nline: %q", i, w, headerW, line)
		}
	}
}

func TestDisplayWidthZeroWidth(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"ZWJ", "\u200D", 0},
		{"ZWNJ", "\u200C", 0},
		{"ZWSP", "\u200B", 0},
		{"BOM", "\uFEFF", 0},
		{"combining 0300", "a\u0300", 1},  // a + combining grave
		{"combining 1AB0", "x\u1AB0", 1},  // x + combining mark
		{"combining 1DC0", "y\u1DC0", 1},  // y + combining mark
		{"combining 20D0", "z\u20D0", 1},  // z + combining mark
		{"combining FE20", "w\uFE20", 1},  // w + combining mark
		{"VS16 emoji", "✏\uFE0F", 2},  // pencil + VS16 (emoji presentation)
		{"VS15 text", "✏\uFE0E", 2},   // pencil + VS15 (text presentation)
		{"VS selector E0100", string([]rune{'A', 0xE0100}), 1},
		{"bidi LRE", "\u202A", 0},
		{"WJ", "\u2060", 0},
		{"LRI", "\u2066", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DisplayWidth(tt.in); got != tt.want {
				t.Errorf("DisplayWidth(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestDisplayWidthWideRanges(t *testing.T) {
	// One sample rune from each isWide() range — all should have display width 2
	wideRunes := []struct {
		name string
		r    rune
	}{
		{"Hangul Jamo", 0x1100},
		{"CJK Bracket", 0x2329},
		{"CJK Radicals", 0x2E80},
		{"Hiragana", 0x3040},
		{"Hangul Syllable", 0xAC00},
		{"CJK Compat Ideograph", 0xF900},
		{"Vertical Forms", 0xFE10},
		{"CJK Compat Forms", 0xFE30},
		{"Fullwidth Latin", 0xFF01},
		{"Fullwidth Cent", 0xFFE0},
		{"CJK Extension B", 0x20000},
		{"CJK Extension G", 0x30000},
		{"Dingbats 2600", 0x2600},
		{"Dingbats 2700", 0x2700},
		{"Mahjong", 0x1F000},
		{"Enclosed Alphanum", 0x1F100},
		{"Misc Symbols", 0x1F300},
		{"Emoticons", 0x1F600},
		{"Transport", 0x1F680},
		{"Supplemental Symbols", 0x1F900},
		{"Chess Symbols", 0x1FA00},
		{"Symbols Extended-A", 0x1FA70},
	}
	for _, tt := range wideRunes {
		t.Run(tt.name, func(t *testing.T) {
			s := string(tt.r)
			if got := DisplayWidth(s); got != 2 {
				t.Errorf("DisplayWidth(%q / U+%04X) = %d, want 2", s, tt.r, got)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in       string
		maxWidth int
		want     string
	}{
		{"hello", 10, "hello"},        // fits
		{"hello", 5, "hello"},         // exact fit
		{"hello world", 6, "hello…"},  // truncated
		{"hello world", 1, "…"},       // minimal
		{"abcdef", 4, "abc…"},         // cut at 3 + ellipsis
		{"", 5, ""},                   // empty
	}
	for _, tt := range tests {
		got := Truncate(tt.in, tt.maxWidth)
		if got != tt.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tt.in, tt.maxWidth, got, tt.want)
		}
	}
}

func TestFormatWidth(t *testing.T) {
	cols := []Column{
		{Header: "Name"},
		{Header: "Description"},
	}
	rows := [][]string{
		{"exec", "Execute shell commands in a sandbox"},
		{"read", "Read file contents"},
	}

	// With plenty of width, should match Format
	wide := FormatWidth(cols, rows, 200)
	normal := Format(cols, rows)
	if wide != normal {
		t.Errorf("FormatWidth with large maxWidth should match Format:\ngot:\n%s\nwant:\n%s", wide, normal)
	}

	// With narrow width, lines should not exceed maxWidth
	narrow := FormatWidth(cols, rows, 30)
	for _, line := range strings.Split(narrow, "\n") {
		w := DisplayWidth(line)
		if w > 30 {
			t.Errorf("line exceeds maxWidth 30 (width %d): %q", w, line)
		}
	}

	// Zero maxWidth delegates to Format
	zero := FormatWidth(cols, rows, 0)
	if zero != normal {
		t.Error("FormatWidth with 0 should delegate to Format")
	}

	// Separator must not overflow even when columns can't shrink further.
	tiny := FormatWidth(cols, rows, 12)
	for _, line := range strings.Split(tiny, "\n") {
		w := DisplayWidth(line)
		if w > 12 {
			t.Errorf("line exceeds maxWidth 12 (width %d): %q", w, line)
		}
	}
}

func TestFormatEmojiColumnAlignment(t *testing.T) {
	// Regression: ✏️ (U+270F + U+FE0F) was measured wider than ✅ (U+2705)
	// because variation selector U+FE0F wasn't treated as zero-width.
	cols := []Column{
		{Header: ""},
		{Header: "Prompt"},
		{Header: "Location"},
	}
	rows := [][]string{
		{"✏\uFE0F", "keepalive", "clutch/prompts/"},
		{"✅", "branch_orient", "shared/prompts/"},
	}

	got := Format(cols, rows)
	lines := strings.Split(got, "\n")

	headerW := DisplayWidth(lines[0])
	for i, line := range lines {
		if i == 1 { // separator
			continue
		}
		w := DisplayWidth(line)
		if w != headerW {
			t.Errorf("line %d display width %d != header width %d\nline: %q\nfull:\n%s",
				i, w, headerW, line, got)
		}
	}
}

func TestDisplayWidthMultipleTabs(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"\t\t", 8},        // 0→4, 4→8
		{"ab\t", 4},        // ab(2) + tab to 4 = 4
		{"abcd\t", 8},      // abcd(4) + tab to 8 = 8
		{"abc\tdef\t", 8},  // abc(3)+tab→4(+1)+def(3)=7+tab→8(+1)=8
	}
	for _, tt := range tests {
		if got := DisplayWidth(tt.in); got != tt.want {
			t.Errorf("DisplayWidth(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// TestTruncateEdgeCases tests additional Truncate edge cases
func TestTruncateEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		maxWidth int
		want     string
	}{
		{"zero width", "", 0, ""},
		{"negative width", "hello", -5, ""},
		{"maxWidth 1", "hello", 1, "…"},
		{"wide chars truncate", "中文test", 4, "中…"},
		{"zero width chars", "a\u200Db", 5, "a\u200Db"},
		{"tabs in truncation", "ab\tcd", 4, "ab…"},
		{"string with only wide chars", "中", 2, "中"},
		{"string with only wide chars overflow", "中国", 3, "中…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Truncate(tt.in, tt.maxWidth)
			if got != tt.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tt.in, tt.maxWidth, got, tt.want)
			}
		})
	}
}

func TestWrapText(t *testing.T) {
	tests := []struct {
		name     string
		s        string
		maxWidth int
		maxLines int
		want     []string
	}{
		{
			name:     "fits within width",
			s:        "hello world",
			maxWidth: 20,
			maxLines: 5,
			want:     []string{"hello world"},
		},
		{
			name:     "simple word wrap",
			s:        "the quick brown fox",
			maxWidth: 10,
			maxLines: 5,
			want:     []string{"the quick", "brown fox"},
		},
		{
			name:     "max lines cap with truncation",
			s:        "one two three four five six",
			maxWidth: 8,
			maxLines: 2,
			want:     []string{"one two", "three…"},
		},
		{
			name:     "long word hard-break",
			s:        "abcdefghijklmnop",
			maxWidth: 5,
			maxLines: 0,
			want:     []string{"abcde", "fghij", "klmno", "p"},
		},
		{
			name:     "wide characters CJK",
			s:        "中文 测试 数据",
			maxWidth: 6,
			maxLines: 0,
			want:     []string{"中文", "测试", "数据"},
		},
		{
			name:     "empty string",
			s:        "",
			maxWidth: 10,
			maxLines: 5,
			want:     []string{""},
		},
		{
			name:     "zero maxLines unlimited",
			s:        "a b c d e f",
			maxWidth: 3,
			maxLines: 0,
			want:     []string{"a b", "c d", "e f"},
		},
		{
			name:     "single word fits exactly",
			s:        "hello",
			maxWidth: 5,
			maxLines: 5,
			want:     []string{"hello"},
		},
		{
			name:     "mixed CJK and ASCII",
			s:        "hello 世界 test",
			maxWidth: 8,
			maxLines: 0,
			want:     []string{"hello", "世界", "test"},
		},
		{
			name:     "hard-break CJK word",
			s:        "中文测試",
			maxWidth: 5,
			maxLines: 0,
			want:     []string{"中文", "测試"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WrapText(tt.s, tt.maxWidth, tt.maxLines)
			if len(got) != len(tt.want) {
				t.Fatalf("WrapText(%q, %d, %d) returned %d lines, want %d\n  got:  %q\n  want: %q",
					tt.s, tt.maxWidth, tt.maxLines, len(got), len(tt.want), got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("WrapText(%q, %d, %d) line %d = %q, want %q",
						tt.s, tt.maxWidth, tt.maxLines, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestFormatWidthEdgeCases tests additional FormatWidth edge cases
func TestFormatWidthEdgeCases(t *testing.T) {
	cols := []Column{
		{Header: "A"},
		{Header: "B"},
	}
	rows := [][]string{
		{"x", "y"},
		{"p", "q"},
	}

	// Very small maxWidth should still produce valid output
	result := FormatWidth(cols, rows, 5)
	lines := strings.Split(result, "\n")
	if len(lines) == 0 {
		t.Error("FormatWidth should produce output even with tiny maxWidth")
	}

	// Each line should not exceed maxWidth (may be less due to min column sizes)
	for _, line := range lines {
		w := DisplayWidth(line)
		if w > 5+2 { // Allow small margin for formatting
			t.Errorf("line width %d exceeds maxWidth 5: %q", w, line)
		}
	}
}
