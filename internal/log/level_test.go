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

func TestKeySuffix(t *testing.T) {
	// Verifies KeySuffix only logs when DebugLogKeySuffix is true
	// and the key has at least 4 characters; short keys are silently ignored.
	orig := DebugLogKeySuffix
	defer func() { DebugLogKeySuffix = orig }()

	// Should not panic with short key or disabled flag.
	DebugLogKeySuffix = false
	KeySuffix("test", "abc") // short key, disabled — no-op
	KeySuffix("test", "abcd") // enough chars but disabled — no-op

	DebugLogKeySuffix = true
	KeySuffix("test", "ab")     // too short — no-op
	KeySuffix("test", "")       // empty — no-op
	KeySuffix("test", "sk-1234") // long enough and enabled — logs (no crash)
}
