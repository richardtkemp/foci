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

// An exact model id wins over longer ids that merely contain it: once
// glm-5.3-flash and glm-5.3-highspeed joined the catalogue, the bare
// "glm-5.3" (what opencode reports back as the session's model, and what a
// batch turn then launches with) stopped resolving (2026-10-08, arnix).
func TestMatchModel_ExactIDBeatsLongerSubstrings(t *testing.T) {
	lines := []string{
		"zai-coding-plan/glm-5.3",
		"zai-coding-plan/glm-5.3-flash",
		"zai-coding-plan/glm-5.3-highspeed",
	}
	for _, in := range []string{"glm-5.3", "zai-coding-plan/glm-5.3"} {
		got, err := matchModel(in, lines)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", in, err)
		}
		if got != "zai-coding-plan/glm-5.3" {
			t.Errorf("%q: got %q, want zai-coding-plan/glm-5.3", in, got)
		}
	}
}

// The same bare id under two providers is still ambiguous.
func TestMatchModel_ExactIDUnderTwoProvidersIsAmbiguous(t *testing.T) {
	lines := []string{
		"zai-coding-plan/glm-5.3",
		"openrouter/glm-5.3",
	}
	_, err := matchModel("glm-5.3", lines)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("want an ambiguity error, got %v", err)
	}
}
