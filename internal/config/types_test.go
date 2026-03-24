package config

import (
	"testing"
)

func TestInjectionLevel_UnmarshalTOML(t *testing.T) {
	// Proves that InjectionLevel correctly normalises string inputs
	// to canonical values, and rejects invalid strings and non-string types.
	tests := []struct {
		name    string
		input   any
		want    InjectionLevel
		wantErr bool
	}{
		// String inputs — canonical values
		{"string all", "all", InjectionAll, false},
		{"string errors", "errors", InjectionErrors, false},
		{"string off", "off", InjectionOff, false},

		// Case insensitive
		{"string ALL", "ALL", InjectionAll, false},
		{"string Errors", "Errors", InjectionErrors, false},
		{"string OFF", "OFF", InjectionOff, false},

		// Empty string → unset (inherit)
		{"string empty", "", InjectionLevel(""), false},

		// Invalid
		{"string invalid", "warn", InjectionLevel(""), true},
		{"int type", int64(42), InjectionLevel(""), true},
		{"bool type", true, InjectionLevel(""), true},
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
