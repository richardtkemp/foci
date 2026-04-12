package config

import (
	"testing"
)

func TestToolCallDisplay_UnmarshalTOML(t *testing.T) {
	// Proves that ToolCallDisplay accepts canonical values, aliases (false→off, true→full),
	// bool inputs, case-insensitive strings, and rejects invalid inputs.
	tests := []struct {
		name    string
		input   any
		want    ToolCallDisplay
		wantErr bool
	}{
		{"off", "off", ToolCallOff, false},
		{"preview", "preview", ToolCallPreview, false},
		{"full", "full", ToolCallFull, false},
		{"false alias", "false", ToolCallOff, false},
		{"true alias", "true", ToolCallFull, false},
		{"medium alias", "medium", ToolCallPreview, false},
		{"case insensitive", "Full", ToolCallFull, false},
		{"bool true", true, ToolCallFull, false},
		{"bool false", false, ToolCallOff, false},
		{"invalid", "bogus", ToolCallDisplay(""), true},
		{"int type", int64(1), ToolCallDisplay(""), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got ToolCallDisplay
			err := got.UnmarshalTOML(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("UnmarshalTOML(%v) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("UnmarshalTOML(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestShowThinking_UnmarshalTOML(t *testing.T) {
	// Proves that ShowThinking accepts canonical values, aliases (false→off, full→true),
	// bool inputs, case-insensitive strings, and rejects invalid inputs.
	tests := []struct {
		name    string
		input   any
		want    ShowThinking
		wantErr bool
	}{
		{"off", "off", ShowThinkingOff, false},
		{"compact", "compact", ShowThinkingCompact, false},
		{"true", "true", ShowThinkingTrue, false},
		{"full alias", "full", ShowThinkingTrue, false},
		{"false alias", "false", ShowThinkingOff, false},
		{"medium alias", "medium", ShowThinkingCompact, false},
		{"case insensitive", "Compact", ShowThinkingCompact, false},
		{"bool true", true, ShowThinkingTrue, false},
		{"bool false", false, ShowThinkingOff, false},
		{"invalid", "bogus", ShowThinking(""), true},
		{"int type", int64(1), ShowThinking(""), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got ShowThinking
			err := got.UnmarshalTOML(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("UnmarshalTOML(%v) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("UnmarshalTOML(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestInjectionLevel_UnmarshalTOML(t *testing.T) {
	// Proves that InjectionLevel accepts canonical values, aliases (false→off, true/full→all),
	// bool inputs, case-insensitive strings, empty string, and rejects invalid inputs.
	tests := []struct {
		name    string
		input   any
		want    InjectionLevel
		wantErr bool
	}{
		// Canonical values
		{"all", "all", InjectionAll, false},
		{"errors", "errors", InjectionErrors, false},
		{"off", "off", InjectionOff, false},

		// Aliases
		{"false alias", "false", InjectionOff, false},
		{"true alias", "true", InjectionAll, false},
		{"full alias", "full", InjectionAll, false},
		{"medium alias", "medium", InjectionErrors, false},

		// Case insensitive
		{"ALL", "ALL", InjectionAll, false},
		{"Errors", "Errors", InjectionErrors, false},
		{"OFF", "OFF", InjectionOff, false},

		// Bool
		{"bool true", true, InjectionAll, false},
		{"bool false", false, InjectionOff, false},

		// Empty string → unset (inherit)
		{"empty", "", InjectionLevel(""), false},

		// Invalid
		{"invalid", "warn", InjectionLevel(""), true},
		{"int type", int64(42), InjectionLevel(""), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got InjectionLevel
			err := got.UnmarshalTOML(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("UnmarshalTOML(%v) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("UnmarshalTOML(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestInjectionLevel_Enabled(t *testing.T) {
	// Proves that Enabled() returns true only for "all" and "errors".
	tests := []struct {
		level InjectionLevel
		want  bool
	}{
		{InjectionAll, true},
		{InjectionErrors, true},
		{InjectionOff, false},
		{InjectionLevel(""), false},
	}
	for _, tt := range tests {
		if got := tt.level.Enabled(); got != tt.want {
			t.Errorf("InjectionLevel(%q).Enabled() = %v, want %v", tt.level, got, tt.want)
		}
	}
}

func TestInjectionLevel_IncludeWarnings(t *testing.T) {
	// Proves that IncludeWarnings() returns true only for "all".
	tests := []struct {
		level InjectionLevel
		want  bool
	}{
		{InjectionAll, true},
		{InjectionErrors, false},
		{InjectionOff, false},
		{InjectionLevel(""), false},
	}
	for _, tt := range tests {
		if got := tt.level.IncludeWarnings(); got != tt.want {
			t.Errorf("InjectionLevel(%q).IncludeWarnings() = %v, want %v", tt.level, got, tt.want)
		}
	}
}

func TestThinkingMode_UnmarshalTOML(t *testing.T) {
	// Proves that ThinkingMode accepts canonical values, aliases (false→off, true→adaptive),
	// bool inputs, case-insensitive strings, empty string, and rejects invalid inputs.
	tests := []struct {
		name    string
		input   any
		want    ThinkingMode
		wantErr bool
	}{
		{"adaptive", "adaptive", ThinkingMode("adaptive"), false},
		{"off", "off", ThinkingMode("off"), false},
		{"true alias", "true", ThinkingMode("adaptive"), false},
		{"false alias", "false", ThinkingMode("off"), false},
		{"case insensitive", "Adaptive", ThinkingMode("adaptive"), false},
		{"bool true", true, ThinkingMode("adaptive"), false},
		{"bool false", false, ThinkingMode("off"), false},
		{"empty", "", ThinkingMode(""), false},
		{"invalid", "bogus", ThinkingMode(""), true},
		{"int type", int64(1), ThinkingMode(""), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got ThinkingMode
			err := got.UnmarshalTOML(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("UnmarshalTOML(%v) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("UnmarshalTOML(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
