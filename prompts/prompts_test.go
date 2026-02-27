package prompts

import (
	"strings"
	"testing"
)

func TestEmbeddedFilesLoadNonEmpty(t *testing.T) {
	tests := []struct {
		name string
		fn   func() string
	}{
		{"BranchOrientationHeadless", BranchOrientationHeadless},
		{"BranchOrientationMultiball", BranchOrientationMultiball},
		{"CompactionSummary", CompactionSummary},
		{"CompactionHandoff", CompactionHandoff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.fn()
			if got == "" {
				t.Errorf("%s() returned empty string", tt.name)
			}
		})
	}
}

func TestBranchOrientationHeadlessHasVars(t *testing.T) {
	text := BranchOrientationHeadless()
	for _, v := range []string{"{branch_type}", "{branch_key}", "{parent_key}"} {
		if !strings.Contains(text, v) {
			t.Errorf("headless orientation missing template var %s", v)
		}
	}
}

func TestBranchOrientationMultiballHasVars(t *testing.T) {
	text := BranchOrientationMultiball()
	for _, v := range []string{"{branch_type}", "{branch_key}", "{parent_key}"} {
		if !strings.Contains(text, v) {
			t.Errorf("multiball orientation missing template var %s", v)
		}
	}
}

func TestReplaceVars(t *testing.T) {
	text := "Hello {name}, you are {role}."
	got := ReplaceVars(text, map[string]string{
		"name": "Alice",
		"role": "admin",
	})
	want := "Hello Alice, you are admin."
	if got != want {
		t.Errorf("ReplaceVars = %q, want %q", got, want)
	}
}

func TestReplaceVarsPartial(t *testing.T) {
	text := "Key: {branch_key}, Type: {branch_type}"
	got := ReplaceVars(text, map[string]string{
		"branch_key": "abc123",
	})
	want := "Key: abc123, Type: {branch_type}"
	if got != want {
		t.Errorf("ReplaceVars partial = %q, want %q", got, want)
	}
}

func TestReplaceVarsEmpty(t *testing.T) {
	text := "No vars here."
	got := ReplaceVars(text, nil)
	if got != text {
		t.Errorf("ReplaceVars with nil = %q, want %q", got, text)
	}
}
