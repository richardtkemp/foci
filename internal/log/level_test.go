package log

import (
	"testing"
)

func TestParseLevel(t *testing.T) {
	// TestParseLevel verifies that ParseLevel correctly maps string inputs to Level constants,
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
	// TestLevelString verifies that each Level constant produces the correct string representation.
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
	// TestLevelStringUnknown verifies that an out-of-range Level returns "???".
	if got := Level(99).String(); got != "???" {
		t.Errorf("Level(99).String() = %q, want %q", got, "???")
	}
}

func TestGetLevel(t *testing.T) {
	// TestGetLevel verifies GetLevel returns the current log level accurately after SetLevel calls.
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

func TestKeySuffix(t *testing.T) {
	// TestKeySuffix verifies KeySuffix only logs when DebugLogKeySuffix is true
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

func TestIsOpenAIModel(t *testing.T) {
	// TestIsOpenAIModel verifies that model names are correctly identified as OpenAI or not,
	// covering gpt-, o1/o3/o4 prefixes, chatgpt- prefix, and non-OpenAI models.
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
