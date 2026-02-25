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
