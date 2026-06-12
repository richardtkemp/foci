package telegram

import (
	"strings"
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

// makeLongButtons creates n buttons where at least one label exceeds 14 characters.
func makeLongButtons(n int) []gotgbot.InlineKeyboardButton {
	btns := makeButtons(n)
	btns[0].Text = "this is a long label"
	return btns
}

func TestLayoutButtons_SmallCounts(t *testing.T) {
	// Verifies that 1–3 short-label buttons stay on one row.
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

func TestLayoutButtons_SplitsAt3(t *testing.T) {
	// Verifies that short-label buttons are split into rows of at most 3.
	tests := []struct {
		n       int
		wantMax int // max buttons per row
		wantMin int // min buttons in any row
	}{
		{4, 3, 1},  // [3, 1]
		{6, 3, 3},  // [3, 3]
		{8, 3, 2},  // [3, 3, 2]
		{12, 3, 3}, // [3, 3, 3, 3]
		{20, 3, 2}, // [3, 3, 3, 3, 3, 3, 2] — no row limit
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

func TestLayoutButtons_LongLabelsLimit2PerRow(t *testing.T) {
	// Verifies that when any button label exceeds 14 characters,
	// the layout drops to 2 buttons per row for readability.
	tests := []struct {
		n        int
		wantRows int
	}{
		{3, 2}, // [2, 1]
		{4, 2}, // [2, 2]
		{5, 3}, // [2, 2, 1]
		{7, 4}, // [2, 2, 2, 1]
	}
	for _, tt := range tests {
		rows := layoutButtons(makeLongButtons(tt.n))
		if len(rows) != tt.wantRows {
			t.Errorf("n=%d: got %d rows, want %d", tt.n, len(rows), tt.wantRows)
		}
		for _, row := range rows {
			if len(row) > 2 {
				t.Errorf("n=%d: row has %d buttons, want at most 2 with long labels", tt.n, len(row))
			}
		}
	}
}

func TestLayoutButtons_LongLabels2ButtonsFitOnOneRow(t *testing.T) {
	// Verifies that 2 buttons with long labels stay on a single row.
	btns := []gotgbot.InlineKeyboardButton{
		{Text: "fifteen chars!!"},
		{Text: "short"},
	}
	rows := layoutButtons(btns)
	if len(rows) != 1 {
		t.Errorf("got %d rows, want 1", len(rows))
	}
}

func TestLayoutButtons_Exactly14CharsStaysAt3(t *testing.T) {
	// Verifies that a 14-character label does NOT trigger the 2-per-row limit
	// (only labels exceeding 14 characters do).
	btns := []gotgbot.InlineKeyboardButton{
		{Text: strings.Repeat("x", 14)},
		{Text: "A"},
		{Text: "B"},
		{Text: "C"},
	}
	rows := layoutButtons(btns)
	// 4 short buttons → [3, 1]
	if len(rows) != 2 {
		t.Errorf("got %d rows, want 2", len(rows))
	}
	if len(rows[0]) != 3 {
		t.Errorf("first row has %d buttons, want 3", len(rows[0]))
	}
}

func TestBuildButtonRows_ExplicitRows(t *testing.T) {
	// Verifies that buttons with different explicit Row values
	// are kept in separate rows (not merged or reordered).
	opts := []command.KeyboardOption{
		{Label: "A", Data: "a", Row: 0},
		{Label: "B", Data: "b", Row: 0},
		{Label: "C", Data: "c", Row: 1},
		{Label: "D", Data: "d", Row: 1},
	}
	rows := buildButtonRows(opts, "cmd:")
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if len(rows[0]) != 2 {
		t.Errorf("row 0: got %d buttons, want 2", len(rows[0]))
	}
	if len(rows[1]) != 2 {
		t.Errorf("row 1: got %d buttons, want 2", len(rows[1]))
	}
}

func TestBuildButtonRows_AutoLayoutDefaultRow(t *testing.T) {
	// Verifies that 8 short-label buttons all on row 0 get auto-split
	// into rows of 3.
	opts := make([]command.KeyboardOption, 8)
	for i := range opts {
		opts[i] = command.KeyboardOption{
			Label: string(rune('A' + i)),
			Data:  string(rune('a' + i)),
		}
	}
	rows := buildButtonRows(opts, "cmd:")
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 (8 buttons at 3 per row)", len(rows))
	}
	// First two rows should have 3, last should have 2.
	if len(rows[0]) != 3 || len(rows[1]) != 3 {
		t.Errorf("first rows should have 3 buttons each")
	}
	if len(rows[2]) != 2 {
		t.Errorf("last row should have 2 buttons, got %d", len(rows[2]))
	}
}

func TestBuildButtonRows_MixedExplicitAndLargeRow(t *testing.T) {
	// Verifies that auto-layout applies per-row: explicit rows with few
	// buttons stay as-is, while a large default row gets split.
	opts := []command.KeyboardOption{
		{Label: "A", Data: "a", Row: 0},
		{Label: "B", Data: "b", Row: 0},
		{Label: "C", Data: "c", Row: 0},
		{Label: "D", Data: "d", Row: 0},
		{Label: "E", Data: "e", Row: 0},
		{Label: "F", Data: "f", Row: 1},
	}
	rows := buildButtonRows(opts, "cmd:")
	// Row 0 (5 buttons) → split into [3, 2]. Row 1 (1 button) → [1].
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
}

func TestBuildButtonRows_CallbackDataFormat(t *testing.T) {
	// Verifies callback data is correctly formatted after auto-layout.
	opts := []command.KeyboardOption{
		{Label: "X", Data: "val1"},
		{Label: "Y", Data: "val2"},
		{Label: "Z", Data: "val3"},
		{Label: "W", Data: "val4"},
	}
	rows := buildButtonRows(opts, "cmd:")
	// Collect all callback data across rows.
	var data []string
	for _, row := range rows {
		for _, btn := range row {
			data = append(data, btn.CallbackData)
		}
	}
	want := []string{"cmd:val1", "cmd:val2", "cmd:val3", "cmd:val4"}
	if len(data) != len(want) {
		t.Fatalf("got %d buttons, want %d", len(data), len(want))
	}
	for i, d := range data {
		if d != want[i] {
			t.Errorf("button %d: got %q, want %q", i, d, want[i])
		}
	}
}
