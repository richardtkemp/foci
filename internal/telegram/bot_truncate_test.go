package telegram

import (
	"testing"
)

func TestTruncate_Short(t *testing.T) {
	// TestTruncate_Short verifies that short strings are not truncated.
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestTruncate_Exact(t *testing.T) {
	// TestTruncate_Exact verifies that strings at exact limit are not truncated.
	if got := truncate("hello", 5); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestTruncate_Long(t *testing.T) {
	// TestTruncate_Long verifies that long strings are truncated with ellipsis.
	if got := truncate("hello world", 5); got != "hello..." {
		t.Errorf("got %q, want %q", got, "hello...")
	}
}
