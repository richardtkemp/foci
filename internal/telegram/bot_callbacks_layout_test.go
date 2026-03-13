package telegram

import (
	"testing"

	"foci/internal/command"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// makeButtons creates n buttons with sequential labels.
func makeButtons(n int) []gotgbot.InlineKeyboardButton {
	btns := make([]gotgbot.InlineKeyboardButton, n)
	for i := range btns {
		btns[i] = gotgbot.InlineKeyboardButton{Text: string(rune('A' + i))}
	}
	return btns
}

// TestLayoutButtons_SmallCounts verifies that 1–3 buttons stay on one row.
func TestLayoutButtons_SmallCounts(t *testing.T) {
	for n := 1; n <= 3; n++ {
		rows := layoutButtons(makeButtons(n))
		if len(rows) != 1 {
			t.Errorf("n=%d: got %d rows, want 1", n, len(rows))
		}
		if len(rows[0]) != n {
			t.Errorf("n=%d: got %d buttons, want %d", n, len(rows[0]), n)
		}
	}
}

// TestLayoutButtons_SplitsAt3 verifies that 4–12 buttons are split into rows
// of at most 3, respecting the 4-row maximum.
func TestLayoutButtons_SplitsAt3(t *testing.T) {
	tests := []struct {
		n       int
		wantMax int // max buttons per row
		wantMin int // min buttons in any row
	}{
		{4, 3, 1},  // [3, 1]
		{6, 3, 3},  // [3, 3]
		{8, 3, 2},  // [3, 3, 2]
		{12, 3, 3}, // [3, 3, 3, 3] — exactly 4 rows
	}
	for _, tt := range tests {
		rows := layoutButtons(makeButtons(tt.n))
		total := 0
		for _, row := range rows {
			if len(row) > tt.wantMax {
				t.Errorf("n=%d: row has %d buttons, want at most %d", tt.n, len(row), tt.wantMax)
			}
			if len(row) < tt.wantMin {
				t.Errorf("n=%d: row has %d buttons, want at least %d", tt.n, len(row), tt.wantMin)
			}
			total += len(row)
		}
		if total != tt.n {
			t.Errorf("n=%d: total buttons %d, want %d", tt.n, total, tt.n)
		}
	}
}

// TestLayoutButtons_PacksWhenTooManyRows verifies that when 3-per-row would
// exceed 4 rows, buttons are packed more densely.
func TestLayoutButtons_PacksWhenTooManyRows(t *testing.T) {
	tests := []struct {
		n          int
		wantRows   int
		wantPerRow int // expected per-row count (ceiling)
	}{
		{13, 4, 4}, // ceil(13/4)=4 per row → [4, 4, 4, 1]
		{16, 4, 4}, // [4, 4, 4, 4]
		{20, 4, 5}, // [5, 5, 5, 5]
	}
	for _, tt := range tests {
		rows := layoutButtons(makeButtons(tt.n))
		if len(rows) > tt.wantRows {
			t.Errorf("n=%d: got %d rows, want at most %d", tt.n, len(rows), tt.wantRows)
		}
		for _, row := range rows {
			if len(row) > tt.wantPerRow {
				t.Errorf("n=%d: row has %d buttons, want at most %d", tt.n, len(row), tt.wantPerRow)
			}
		}
	}
}

// TestBuildCommandKeyboard_ExplicitRows verifies that buttons with different
// explicit Row values are kept in separate rows (not merged or reordered).
func TestBuildCommandKeyboard_ExplicitRows(t *testing.T) {
	opts := []command.KeyboardOption{
		{Label: "A", Data: "a", Row: 0},
		{Label: "B", Data: "b", Row: 0},
		{Label: "C", Data: "c", Row: 1},
		{Label: "D", Data: "d", Row: 1},
	}
	kb := buildCommandKeyboard("test", opts)
	if len(kb.InlineKeyboard) != 2 {
		t.Fatalf("got %d rows, want 2", len(kb.InlineKeyboard))
	}
	if len(kb.InlineKeyboard[0]) != 2 {
		t.Errorf("row 0: got %d buttons, want 2", len(kb.InlineKeyboard[0]))
	}
	if len(kb.InlineKeyboard[1]) != 2 {
		t.Errorf("row 1: got %d buttons, want 2", len(kb.InlineKeyboard[1]))
	}
}

// TestBuildCommandKeyboard_AutoLayoutDefaultRow verifies that 8 buttons all
// on row 0 get auto-split into rows of 3.
func TestBuildCommandKeyboard_AutoLayoutDefaultRow(t *testing.T) {
	opts := make([]command.KeyboardOption, 8)
	for i := range opts {
		opts[i] = command.KeyboardOption{
			Label: string(rune('A' + i)),
			Data:  string(rune('a' + i)),
		}
	}
	kb := buildCommandKeyboard("test", opts)
	if len(kb.InlineKeyboard) != 3 {
		t.Fatalf("got %d rows, want 3 (8 buttons at 3 per row)", len(kb.InlineKeyboard))
	}
	// First two rows should have 3, last should have 2.
	if len(kb.InlineKeyboard[0]) != 3 || len(kb.InlineKeyboard[1]) != 3 {
		t.Errorf("first rows should have 3 buttons each")
	}
	if len(kb.InlineKeyboard[2]) != 2 {
		t.Errorf("last row should have 2 buttons, got %d", len(kb.InlineKeyboard[2]))
	}
}

// TestBuildCommandKeyboard_MixedExplicitAndLargeRow verifies that auto-layout
// applies per-row: explicit rows with few buttons stay as-is, while a large
// default row gets split.
func TestBuildCommandKeyboard_MixedExplicitAndLargeRow(t *testing.T) {
	opts := []command.KeyboardOption{
		{Label: "A", Data: "a", Row: 0},
		{Label: "B", Data: "b", Row: 0},
		{Label: "C", Data: "c", Row: 0},
		{Label: "D", Data: "d", Row: 0},
		{Label: "E", Data: "e", Row: 0},
		{Label: "F", Data: "f", Row: 1},
	}
	kb := buildCommandKeyboard("test", opts)
	// Row 0 (5 buttons) → split into [3, 2]. Row 1 (1 button) → [1].
	if len(kb.InlineKeyboard) != 3 {
		t.Fatalf("got %d rows, want 3", len(kb.InlineKeyboard))
	}
}

// TestBuildCommandKeyboard_CallbackDataFormat verifies callback data is
// correctly formatted after auto-layout.
func TestBuildCommandKeyboard_CallbackDataFormat(t *testing.T) {
	opts := []command.KeyboardOption{
		{Label: "X", Data: "val1"},
		{Label: "Y", Data: "val2"},
		{Label: "Z", Data: "val3"},
		{Label: "W", Data: "val4"},
	}
	kb := buildCommandKeyboard("cmd", opts)
	// Collect all callback data across rows.
	var data []string
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			data = append(data, btn.CallbackData)
		}
	}
	want := []string{"cmd:/cmd val1", "cmd:/cmd val2", "cmd:/cmd val3", "cmd:/cmd val4"}
	if len(data) != len(want) {
		t.Fatalf("got %d buttons, want %d", len(data), len(want))
	}
	for i, d := range data {
		if d != want[i] {
			t.Errorf("button %d: got %q, want %q", i, d, want[i])
		}
	}
}

