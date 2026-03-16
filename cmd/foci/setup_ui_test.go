package main

import (
	"bufio"
	"strings"
	"testing"
)

// Verifies MultiSelect returns selected indices from comma-separated input.
func TestMultiSelectComma(t *testing.T) {
	ui := &consoleUI{reader: bufio.NewReader(strings.NewReader("1,3\n"))}
	sel, back := ui.MultiSelect("Pick platforms", []string{"Discord", "Telegram", "Slack"})
	if back {
		t.Fatal("unexpected back")
	}
	if len(sel) != 2 || sel[0] != 0 || sel[1] != 2 {
		t.Errorf("selected = %v, want [0 2]", sel)
	}
}

// Verifies MultiSelect returns all indices when "all" is entered.
func TestMultiSelectAll(t *testing.T) {
	ui := &consoleUI{reader: bufio.NewReader(strings.NewReader("all\n"))}
	sel, back := ui.MultiSelect("", []string{"A", "B"})
	if back {
		t.Fatal("unexpected back")
	}
	if len(sel) != 2 || sel[0] != 0 || sel[1] != 1 {
		t.Errorf("selected = %v, want [0 1]", sel)
	}
}

// Verifies MultiSelect returns empty selection on blank input.
func TestMultiSelectNone(t *testing.T) {
	ui := &consoleUI{reader: bufio.NewReader(strings.NewReader("\n"))}
	sel, back := ui.MultiSelect("", []string{"A", "B"})
	if back {
		t.Fatal("unexpected back")
	}
	if len(sel) != 0 {
		t.Errorf("selected = %v, want empty", sel)
	}
}

// Verifies MultiSelect returns back=true when "back" is entered.
func TestMultiSelectBack(t *testing.T) {
	ui := &consoleUI{reader: bufio.NewReader(strings.NewReader("back\n"))}
	_, back := ui.MultiSelect("", []string{"A", "B"})
	if !back {
		t.Fatal("expected back")
	}
}

// Verifies MultiSelect retries on invalid input then accepts valid input.
func TestMultiSelectRetry(t *testing.T) {
	// First line is invalid (0 is out of range), second is valid.
	ui := &consoleUI{reader: bufio.NewReader(strings.NewReader("0\n2\n"))}
	sel, back := ui.MultiSelect("", []string{"A", "B"})
	if back {
		t.Fatal("unexpected back")
	}
	if len(sel) != 1 || sel[0] != 1 {
		t.Errorf("selected = %v, want [1]", sel)
	}
}

// Verifies MultiSelect deduplicates repeated indices.
func TestMultiSelectDedup(t *testing.T) {
	ui := &consoleUI{reader: bufio.NewReader(strings.NewReader("1,1,2\n"))}
	sel, back := ui.MultiSelect("", []string{"A", "B"})
	if back {
		t.Fatal("unexpected back")
	}
	if len(sel) != 2 || sel[0] != 0 || sel[1] != 1 {
		t.Errorf("selected = %v, want [0 1]", sel)
	}
}
