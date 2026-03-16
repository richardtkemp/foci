package log

import (
	"testing"
)

func TestParseLevel(t *testing.T) {
	// Verifies that ParseLevel correctly maps string inputs to Level constants,
	// including case-insensitivity, whitespace trimming, and fallback to INFO for unknowns.
	tests := []struct {
		input string
		want  Level
	}{
		{"DEBUG", DEBUG},
		{"debug", DEBUG},
		{"INFO", INFO},
		{"info", INFO},
		{"WARN", WARN},
		{"warn", WARN},
		{"ERROR", ERROR},
		{"error", ERROR},
		{"  INFO  ", INFO},
		{"unknown", INFO},
		{"", INFO},
	}
	for _, tt := range tests {
		got := ParseLevel(tt.input)
		if got != tt.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestLevelString(t *testing.T) {
	// Verifies that each Level constant produces the correct string representation.
	tests := []struct {
		level Level
		want  string
	}{
		{DEBUG, "DEBUG"},
		{INFO, "INFO"},
		{WARN, "WARN"},
		{ERROR, "ERROR"},
	}
	for _, tt := range tests {
		if got := tt.level.String(); got != tt.want {
			t.Errorf("Level(%d).String() = %q, want %q", tt.level, got, tt.want)
		}
	}
}

func TestLevelStringUnknown(t *testing.T) {
	// Verifies that an out-of-range Level returns "???".
	if got := Level(99).String(); got != "???" {
		t.Errorf("Level(99).String() = %q, want %q", got, "???")
	}
}

func TestGetLevel(t *testing.T) {
	// Verifies getLevel returns the current log level accurately after setLevel calls.
	setLevel(WARN)
	defer setLevel(INFO)

	if got := getLevel(); got != WARN {
		t.Errorf("getLevel() = %v, want WARN", got)
	}

	setLevel(DEBUG)
	if got := getLevel(); got != DEBUG {
		t.Errorf("getLevel() = %v, want DEBUG", got)
	}
}

func TestFormatKeySuffix(t *testing.T) {
	// Verifies FormatKeySuffix returns a suffix only when DebugLogKeySuffix
	// is true and the key has at least 4 characters.
	orig := DebugLogKeySuffix
	defer func() { DebugLogKeySuffix = orig }()

	DebugLogKeySuffix = false
	if got := FormatKeySuffix("abcd"); got != "" {
		t.Errorf("disabled: got %q, want empty", got)
	}

	DebugLogKeySuffix = true
	if got := FormatKeySuffix("ab"); got != "" {
		t.Errorf("short key: got %q, want empty", got)
	}
	if got := FormatKeySuffix(""); got != "" {
		t.Errorf("empty key: got %q, want empty", got)
	}
	if got := FormatKeySuffix("sk-1234"); got != "...1234" {
		t.Errorf("valid key: got %q, want %q", got, "...1234")
	}
}
