package opencode

import (
	"strings"
	"testing"
)

func TestMatchModel_UniqueSubstring(t *testing.T) {
	// "glm-5.2" appears in exactly one model ID → resolved to full ID.
	lines := []string{
		"zai-coding-plan/glm-4.5-air",
		"zai-coding-plan/glm-4.7",
		"zai-coding-plan/glm-5-turbo",
		"zai-coding-plan/glm-5.1",
		"zai-coding-plan/glm-5.2",
		"zai-coding-plan/glm-5v-turbo",
	}
	got, err := matchModel("glm-5.2", lines)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "zai-coding-plan/glm-5.2" {
		t.Errorf("got %q, want zai-coding-plan/glm-5.2", got)
	}
}

func TestMatchModel_ExactMatch(t *testing.T) {
	lines := []string{
		"zai-coding-plan/glm-5.2",
		"zai-coding-plan/glm-5.1",
	}
	got, err := matchModel("zai-coding-plan/glm-5.2", lines)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "zai-coding-plan/glm-5.2" {
		t.Errorf("got %q, want zai-coding-plan/glm-5.2", got)
	}
}

func TestMatchModel_Ambiguous(t *testing.T) {
	// "glm" matches multiple model IDs → error.
	lines := []string{
		"zai-coding-plan/glm-4.7",
		"zai-coding-plan/glm-5.2",
	}
	_, err := matchModel("glm", lines)
	if err == nil {
		t.Fatal("expected error for ambiguous match, got nil")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error should mention ambiguity: %v", err)
	}
}

func TestMatchModel_NoMatch(t *testing.T) {
	lines := []string{
		"zai-coding-plan/glm-5.2",
	}
	_, err := matchModel("opus", lines)
	if err == nil {
		t.Fatal("expected error for no match, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not found: %v", err)
	}
}

func TestMatchModel_EmptyLines(t *testing.T) {
	// Output may contain trailing newlines / blank lines — these should
	// be skipped, not matched.
	lines := []string{
		"",
		"zai-coding-plan/glm-5.2",
		"",
	}
	got, err := matchModel("glm-5.2", lines)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "zai-coding-plan/glm-5.2" {
		t.Errorf("got %q", got)
	}
}
