package discord

import (
	"strings"
	"testing"

	"foci/internal/platform"

	"github.com/bwmarrin/discordgo"
)

// TestBuildButtonComponents verifies that ButtonChoice slices are converted into
// Discord action rows with correct button labels, styles, and custom IDs.
func TestBuildButtonComponents(t *testing.T) {
	opts := []platform.ButtonChoice{
		{Label: "Haiku", Data: "haiku", Row: 0},
		{Label: "Opus", Data: "opus", Row: 0},
		{Label: "Cancel", Data: "cancel", Row: 1},
	}

	components := buildButtonComponents(opts, "cmd:")
	if len(components) != 2 {
		t.Fatalf("expected 2 action rows, got %d", len(components))
	}

	// Row 0 should have 2 buttons
	row0, ok := components[0].(discordgo.ActionsRow)
	if !ok {
		t.Fatal("expected ActionsRow")
	}
	if len(row0.Components) != 2 {
		t.Fatalf("expected 2 buttons in row 0, got %d", len(row0.Components))
	}

	btn, ok := row0.Components[0].(discordgo.Button)
	if !ok {
		t.Fatal("expected Button")
	}
	if btn.Label != "Haiku" {
		t.Errorf("expected label Haiku, got %q", btn.Label)
	}
	if btn.CustomID != "cmd:haiku" {
		t.Errorf("expected custom ID cmd:haiku, got %q", btn.CustomID)
	}

	// Row 1 should have 1 button
	row1, ok := components[1].(discordgo.ActionsRow)
	if !ok {
		t.Fatal("expected ActionsRow")
	}
	if len(row1.Components) != 1 {
		t.Fatalf("expected 1 button in row 1, got %d", len(row1.Components))
	}
}

// TestBuildButtonComponentsMax5PerRow verifies that rows with >5 buttons are split
// into multiple action rows (Discord's 5-button limit).
func TestBuildButtonComponentsMax5PerRow(t *testing.T) {
	opts := make([]platform.ButtonChoice, 7)
	for i := range opts {
		opts[i] = platform.ButtonChoice{Label: "B", Data: "d", Row: 0}
	}

	components := buildButtonComponents(opts, "cmd:")
	if len(components) != 2 {
		t.Fatalf("expected 2 action rows for 7 buttons, got %d", len(components))
	}

	row0, _ := components[0].(discordgo.ActionsRow)
	row1, _ := components[1].(discordgo.ActionsRow)
	if len(row0.Components) != 5 {
		t.Errorf("expected 5 buttons in first row, got %d", len(row0.Components))
	}
	if len(row1.Components) != 2 {
		t.Errorf("expected 2 buttons in second row, got %d", len(row1.Components))
	}
}

// TestBuildButtonComponentsSingleButton verifies a single-element ButtonChoice
// creates a proper action row.
func TestBuildButtonComponentsSingleButton(t *testing.T) {
	components := buildButtonComponents([]platform.ButtonChoice{{Label: "Show full", Data: "show"}}, "tc:")
	if len(components) != 1 {
		t.Fatalf("expected 1 action row, got %d", len(components))
	}

	row, ok := components[0].(discordgo.ActionsRow)
	if !ok {
		t.Fatal("expected ActionsRow")
	}
	if len(row.Components) != 1 {
		t.Fatalf("expected 1 button, got %d", len(row.Components))
	}

	btn, ok := row.Components[0].(discordgo.Button)
	if !ok {
		t.Fatal("expected Button")
	}
	if btn.Label != "Show full" {
		t.Errorf("expected label 'Show full', got %q", btn.Label)
	}
	if btn.CustomID != "tc:show" {
		t.Errorf("expected custom ID 'tc:show', got %q", btn.CustomID)
	}
	if btn.Style != discordgo.PrimaryButton {
		t.Errorf("expected PrimaryButton style, got %d", btn.Style)
	}
}

// TestFormatToolCallWithResult verifies tool call result formatting stays within limits.
func TestFormatToolCallWithResult(t *testing.T) {
	toolText := "**exec**: `ls -la`"
	result := "file1.txt\nfile2.txt"

	formatted := formatToolCallWithResult(toolText, result)
	if len(formatted) > discordMaxChars {
		t.Errorf("formatted text exceeds Discord limit: %d chars", len(formatted))
	}
	if formatted == "" {
		t.Error("expected non-empty formatted text")
	}
}

// TestFormatToolCallWithResultTruncation verifies that long results are truncated.
func TestFormatToolCallWithResultTruncation(t *testing.T) {
	toolText := "**exec**: `ls`"
	result := string(make([]byte, 3000)) // way too long

	formatted := formatToolCallWithResult(toolText, result)
	if len(formatted) > discordMaxChars {
		t.Errorf("formatted text exceeds Discord limit: %d chars", len(formatted))
	}
}

// TestFormatThinkingExpanded verifies the thinking expansion format.
func TestFormatThinkingExpanded(t *testing.T) {
	thinking := "Let me think about this..."
	response := "Here's my answer."
	formatted := formatThinkingExpanded(thinking, response, 40)

	if formatted == "" {
		t.Error("expected non-empty formatted text")
	}
	// Should contain the thinking text in italics and the response
	if !strings.Contains(formatted, thinking) {
		t.Error("expected formatted text to contain thinking")
	}
	if !strings.Contains(formatted, response) {
		t.Error("expected formatted text to contain response")
	}
}
