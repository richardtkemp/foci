package display

import (
	"strings"
	"testing"
	"time"
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

func TestMarkdownTable(t *testing.T) {
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

	got := MarkdownTable(cols, rows)
	lines := strings.Split(got, "\n")

	if len(lines) != 5 { // header + sep + 3 rows
		t.Fatalf("expected 5 lines, got %d:\n%s", len(lines), got)
	}

	// Header row should be pipe-delimited
	if !strings.HasPrefix(lines[0], "| Name") {
		t.Errorf("header should start with '| Name', got: %q", lines[0])
	}
	if !strings.HasSuffix(lines[0], " |") {
		t.Errorf("header should end with ' |', got: %q", lines[0])
	}

	// Separator should have pipes and dashes
	if !strings.Contains(lines[1], "---") {
		t.Errorf("separator should contain '---', got: %q", lines[1])
	}
	// Right-aligned column should have ---:
	if !strings.Contains(lines[1], "---:") {
		t.Errorf("separator should contain '---:' for right-aligned column, got: %q", lines[1])
	}

	// Data rows should be pipe-delimited
	if !strings.Contains(lines[2], "| Alice |") {
		t.Errorf("data row should contain '| Alice |', got: %q", lines[2])
	}
}

func TestMarkdownTableEmptyRows(t *testing.T) {
	cols := []Column{
		{Header: "A"},
		{Header: "B"},
	}

	got := MarkdownTable(cols, nil)
	lines := strings.Split(got, "\n")

	if len(lines) != 2 { // header + sep only
		t.Fatalf("expected 2 lines for empty rows, got %d:\n%s", len(lines), got)
	}
}

func TestMarkdownTableNoCols(t *testing.T) {
	got := MarkdownTable(nil, [][]string{{"a", "b"}})
	if got != "" {
		t.Errorf("expected empty string for no columns, got: %q", got)
	}
}

func TestMarkdownTableSingleColumn(t *testing.T) {
	cols := []Column{{Header: "Item"}}
	rows := [][]string{{"alpha"}, {"beta"}}

	got := MarkdownTable(cols, rows)
	lines := strings.Split(got, "\n")
	if len(lines) != 4 { // header + sep + 2 rows
		t.Fatalf("expected 4 lines, got %d:\n%s", len(lines), got)
	}
	if !strings.Contains(lines[0], "| Item |") {
		t.Errorf("header missing '| Item |': %q", lines[0])
	}
}

func TestMarkdownTableMismatchedRowLengths(t *testing.T) {
	cols := []Column{{Header: "A"}, {Header: "B"}, {Header: "C"}}

	// Row shorter than columns — missing cells should render as empty
	rows := [][]string{{"x"}}
	got := MarkdownTable(cols, rows)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 { // header + sep + 1 row
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), got)
	}
	// Should have 3 pipe-delimited cells
	if strings.Count(lines[2], "|") != 4 { // leading + 3 separators
		t.Errorf("row should have 4 pipes, got: %q", lines[2])
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
		{"combining 0300", "a\u0300", 1}, // a + combining grave
		{"combining 1AB0", "x\u1AB0", 1}, // x + combining mark
		{"combining 1DC0", "y\u1DC0", 1}, // y + combining mark
		{"combining 20D0", "z\u20D0", 1}, // z + combining mark
		{"combining FE20", "w\uFE20", 1}, // w + combining mark
		{"VS16 emoji", "✏\uFE0F", 2},     // pencil + VS16 (emoji presentation)
		{"VS15 text", "✏\uFE0E", 2},      // pencil + VS15 (text presentation)
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
		{"hello", 10, "hello"},       // fits
		{"hello", 5, "hello"},        // exact fit
		{"hello world", 6, "hello…"}, // truncated
		{"hello world", 1, "…"},      // minimal
		{"abcdef", 4, "abc…"},        // cut at 3 + ellipsis
		{"", 5, ""},                  // empty
	}
	for _, tt := range tests {
		got := Truncate(tt.in, tt.maxWidth)
		if got != tt.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tt.in, tt.maxWidth, got, tt.want)
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

func TestDisplayWidthMultipleTabs(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"\t\t", 8},       // 0→4, 4→8
		{"ab\t", 4},       // ab(2) + tab to 4 = 4
		{"abcd\t", 8},     // abcd(4) + tab to 8 = 8
		{"abc\tdef\t", 8}, // abc(3)+tab→4(+1)+def(3)=7+tab→8(+1)=8
	}
	for _, tt := range tests {
		if got := DisplayWidth(tt.in); got != tt.want {
			t.Errorf("DisplayWidth(%q) = %d, want %d", tt.in, got, tt.want)
		}
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

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{38 * time.Second, "38s"},
		{90 * time.Second, "1m30s"},
		{3*time.Hour + 12*time.Minute, "3h12m"},
		{49*time.Hour + 30*time.Minute, "2d1h"},
		{-5 * time.Second, "5s"}, // negative duration
	}
	for _, tt := range tests {
		got := FormatDuration(tt.d)
		if got != tt.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestFormatCommas(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{32793, "32,793"},
		{200000, "200,000"},
		{1234567, "1,234,567"},
	}
	for _, tt := range tests {
		got := FormatCommas(tt.n)
		if got != tt.want {
			t.Errorf("FormatCommas(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
	}
	for _, tt := range tests {
		got := FormatBytes(tt.n)
		if got != tt.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

// Verifies CompactRelativeTime returns short labels without " ago" suffix,
// suitable for tight table columns.
func TestCompactRelativeTime(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"now", now.Add(-10 * time.Second), "now"},
		{"1m", now.Add(-90 * time.Second), "1m"},
		{"5m", now.Add(-5 * time.Minute), "5m"},
		{"1h", now.Add(-90 * time.Minute), "1h"},
		{"3h", now.Add(-3 * time.Hour), "3h"},
		{"1d", now.Add(-36 * time.Hour), "1d"},
		{"3d", now.Add(-72 * time.Hour), "3d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CompactRelativeTime(tt.t)
			if got != tt.want {
				t.Errorf("CompactRelativeTime() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRelativeTime(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"just now", now.Add(-10 * time.Second), "just now"},
		{"1m ago", now.Add(-90 * time.Second), "1m ago"},
		{"5m ago", now.Add(-5 * time.Minute), "5m ago"},
		{"1h ago", now.Add(-90 * time.Minute), "1h ago"},
		{"3h ago", now.Add(-3 * time.Hour), "3h ago"},
		{"1d ago", now.Add(-36 * time.Hour), "1d ago"},
		{"3d ago", now.Add(-72 * time.Hour), "3d ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RelativeTime(tt.t)
			if got != tt.want {
				t.Errorf("RelativeTime() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDetectTables verifies table detection in mixed markdown content.
func TestDetectTables(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		blocks int
		starts []int
	}{
		{
			name:   "single table",
			input:  "| A | B |\n|---|---|\n| 1 | 2 |",
			blocks: 1,
			starts: []int{0},
		},
		{
			name:   "table with surrounding text",
			input:  "hello\n| A | B |\n|---|---|\n| x | y |\nworld",
			blocks: 1,
			starts: []int{1},
		},
		{
			name:   "no table without separator",
			input:  "| A | B |\n| 1 | 2 |",
			blocks: 0,
		},
		{
			name:   "two tables",
			input:  "| A |\n|---|\n| 1 |\n\ntext\n\n| B |\n|---|\n| 2 |",
			blocks: 2,
			starts: []int{0, 6},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectTables(tt.input)
			if len(got) != tt.blocks {
				t.Fatalf("DetectTables: got %d blocks, want %d", len(got), tt.blocks)
			}
			for i, b := range got {
				if i < len(tt.starts) && b.StartLine != tt.starts[i] {
					t.Errorf("block %d: StartLine = %d, want %d", i, b.StartLine, tt.starts[i])
				}
			}
		})
	}
}

// TestRenderTable verifies both pretty and markdown rendering styles.
func TestRenderTable(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		opts  RenderOpts
		want  string
	}{
		{
			name:  "pretty style default",
			lines: []string{"| A | B |", "|---|---|", "| x | y |"},
			opts:  RenderOpts{},
			want:  "A    B  \n────────\nx    y  ",
		},
		{
			name:  "markdown style",
			lines: []string{"| A | B |", "|---|---|", "| x | y |"},
			opts:  RenderOpts{Style: "markdown"},
			want:  "| A   | B   |\n| --- | --- |\n| x   | y   |",
		},
		{
			name:  "pretty with max width",
			lines: []string{"| Name | Description |", "|------|-------------|", "| test | a long description here |"},
			opts:  RenderOpts{MaxWidth: 20, WrapLines: 3},
			want:  "Name  Description   \n────────────────────\ntest  a long        \n      description   \n      here          ",
		},
		{
			name:  "markdown truncation",
			lines: []string{"| Col |", "|-----|", "| abcdefghij |"},
			opts:  RenderOpts{MaxWidth: 10, WrapLines: 0, Style: "markdown"},
			want:  "| Col    |\n| ------ |\n| abcde… |",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RenderTable(tt.lines, tt.opts)
			if got != tt.want {
				t.Errorf("RenderTable\n  got  = %q\n  want = %q", got, tt.want)
			}
		})
	}
}

// TestParseCells verifies pipe-table cell parsing.
func TestParseCells(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"| a | b |", []string{"a", "b"}},
		{"| a | b ", []string{"a", "b"}},
		{" a | b |", []string{"a", "b"}},
		{"| one |", []string{"one"}},
	}
	for _, tt := range tests {
		got := ParseCells(tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("ParseCells(%q): got %d cells, want %d", tt.in, len(got), len(tt.want))
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("ParseCells(%q)[%d] = %q, want %q", tt.in, i, got[i], tt.want[i])
			}
		}
	}
}
