package telegram

import (
	"testing"
)

// TestTruncate_Short verifies that short strings are not truncated.
func TestTruncate_Short(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

// TestTruncate_Exact verifies that strings at exact limit are not truncated.
func TestTruncate_Exact(t *testing.T) {
	if got := truncate("hello", 5); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

// TestTruncate_Long verifies that long strings are truncated with ellipsis.
func TestTruncate_Long(t *testing.T) {
	if got := truncate("hello world", 5); got != "hello..." {
		t.Errorf("got %q, want %q", got, "hello...")
	}
}
