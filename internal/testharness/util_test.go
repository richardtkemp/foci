package testharness

import (
	"testing"
)

// TestParseURLForm proves the wrapper returns parsed values for valid
// URL-encoded bodies, an empty (non-nil) Values for empty input, and an
// error for malformed percent-encoding.
func TestParseURLForm(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
		key     string
		want    string
	}{
		{name: "empty body", body: "", wantErr: false},
		{name: "single pair", body: "chat_id=42", key: "chat_id", want: "42"},
		{name: "multiple pairs", body: "a=1&b=two", key: "b", want: "two"},
		{name: "bad percent encoding", body: "%zz=1", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values, err := parseURLForm([]byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseURLForm(%q) error = %v, wantErr %v", tt.body, err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if values == nil {
				t.Fatalf("parseURLForm(%q) returned nil Values", tt.body)
			}
			if tt.key != "" && values.Get(tt.key) != tt.want {
				t.Errorf("values.Get(%q) = %q, want %q", tt.key, values.Get(tt.key), tt.want)
			}
		})
	}
}

// TestParseInt64 proves valid decimal strings parse and that empty,
// non-numeric, and overflow inputs all return the 0 sentinel.
func TestParseInt64(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"42", 42},
		{"-7", -7},
		{"not-a-number", 0},
		{"99999999999999999999999", 0}, // overflows int64
		{"1.5", 0},                     // not a decimal integer
	}
	for _, tt := range tests {
		if got := parseInt64(tt.in); got != tt.want {
			t.Errorf("parseInt64(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}
