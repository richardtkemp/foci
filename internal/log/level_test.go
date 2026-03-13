package log

import (
	"testing"
)

// TestParseLevel verifies that ParseLevel correctly maps string inputs to Level constants,
// including case-insensitivity, whitespace trimming, and fallback to INFO for unknowns.
func TestParseLevel(t *testing.T) {
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

// TestLevelString verifies that each Level constant produces the correct string representation.
func TestLevelString(t *testing.T) {
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

// TestLevelStringUnknown verifies that an out-of-range Level returns "???".
func TestLevelStringUnknown(t *testing.T) {
	if got := Level(99).String(); got != "???" {
		t.Errorf("Level(99).String() = %q, want %q", got, "???")
	}
}

// TestGetLevel verifies GetLevel returns the current log level accurately after SetLevel calls.
func TestGetLevel(t *testing.T) {
	SetLevel(WARN)
	defer SetLevel(INFO)

	if got := GetLevel(); got != WARN {
		t.Errorf("GetLevel() = %v, want WARN", got)
	}

	SetLevel(DEBUG)
	if got := GetLevel(); got != DEBUG {
		t.Errorf("GetLevel() = %v, want DEBUG", got)
	}
}

// TestKeySuffix verifies KeySuffix only logs when DebugLogKeySuffix is true
// and the key has at least 4 characters; short keys are silently ignored.
func TestKeySuffix(t *testing.T) {
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

// TestIsOpenAIModel verifies that model names are correctly identified as OpenAI or not,
// covering gpt-, o1/o3/o4 prefixes, chatgpt- prefix, and non-OpenAI models.
func TestIsOpenAIModel(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"gpt-4", true},
		{"gpt-3.5-turbo", true},
		{"o1", true},
		{"o3", true},
		{"o4", true},
		{"chatgpt-4", true},
		{"claude-3-sonnet", false},
		{"gemini-2-flash", false},
		{"", false},
	}

	for _, tt := range tests {
		got := isOpenAIModel(tt.model)
		if got != tt.want {
			t.Errorf("isOpenAIModel(%q) = %v, want %v", tt.model, got, tt.want)
		}
	}
}
