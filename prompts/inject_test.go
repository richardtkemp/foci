package prompts

import (
	"strings"
	"testing"
	"time"
)

func TestFormatInjectedMessage(t *testing.T) {
	when := time.Date(2026, 3, 1, 14, 30, 0, 0, time.UTC)
	result := FormatInjectedMessage("SCHEDULED WAKE", when, "Time to check your tasks.")

	// Tag and timestamp in header
	if !strings.Contains(result, "[SCHEDULED WAKE @ 2026-03-01T14:30:00Z]") {
		t.Errorf("missing header, got:\n%s", result)
	}

	// Body present
	if !strings.Contains(result, "Time to check your tasks.") {
		t.Error("missing body")
	}

	// Context note
	if !strings.Contains(result, "SYSTEM INJECTION") {
		t.Error("missing context note")
	}

	// Context note mentions user cannot see it
	if !strings.Contains(result, "NOT by the user") {
		t.Error("missing user visibility note")
	}
}

func TestFormatInjectedMessageMultiline(t *testing.T) {
	when := time.Date(2026, 1, 15, 8, 0, 0, 0, time.UTC)
	body := "- warning 1\n- warning 2\n- warning 3"
	result := FormatInjectedMessage("PROACTIVE WARNINGS", when, body)

	if !strings.Contains(result, "[PROACTIVE WARNINGS @ 2026-01-15T08:00:00Z]") {
		t.Errorf("missing header, got:\n%s", result)
	}
	if !strings.Contains(result, "- warning 1\n- warning 2\n- warning 3") {
		t.Error("multi-line body not preserved")
	}
}

func TestFormatInjectedMessageEmptyBody(t *testing.T) {
	when := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	result := FormatInjectedMessage("SYSTEM RESTART", when, "")

	if !strings.Contains(result, "[SYSTEM RESTART @ 2026-03-01T12:00:00Z]") {
		t.Errorf("missing header, got:\n%s", result)
	}
	// Should NOT have a blank line between header and context note
	if strings.Contains(result, "Z]\n\n\n") {
		t.Error("empty body should not produce extra blank lines")
	}
	if !strings.Contains(result, "SYSTEM INJECTION") {
		t.Error("missing context note")
	}
}

func TestFormatInjectedMessageCustomContextNote(t *testing.T) {
	// Verifies that a custom context note replaces the default.
	when := time.Date(2026, 3, 1, 14, 30, 0, 0, time.UTC)
	custom := "[CUSTOM NOTE — reply goes elsewhere]"
	result := FormatInjectedMessage("TEST", when, "hello", custom)

	if strings.Contains(result, defaultInjectionNote) {
		t.Error("default note should not appear when custom note is provided")
	}
	if !strings.Contains(result, custom) {
		t.Errorf("custom note missing, got:\n%s", result)
	}
	if !strings.Contains(result, "[TEST @ 2026-03-01T14:30:00Z]") {
		t.Error("header missing")
	}
	if !strings.Contains(result, "hello") {
		t.Error("body missing")
	}
}

func TestFormatInjectedMessageDefaultContextNote(t *testing.T) {
	// Verifies that omitting the contextNote parameter uses the default.
	when := time.Date(2026, 3, 1, 14, 30, 0, 0, time.UTC)
	result := FormatInjectedMessage("TEST", when, "body")

	if !strings.Contains(result, defaultInjectionNote) {
		t.Error("default note should appear when no custom note is provided")
	}
}

func TestFormatInjectedMessageUTCConversion(t *testing.T) {
	// Provide a non-UTC time — should be converted to UTC in output
	loc := time.FixedZone("EST", -5*3600)
	when := time.Date(2026, 6, 15, 12, 0, 0, 0, loc) // 12:00 EST = 17:00 UTC
	result := FormatInjectedMessage("TEST", when, "body")

	if !strings.Contains(result, "2026-06-15T17:00:00Z") {
		t.Errorf("expected UTC conversion, got:\n%s", result)
	}
}
