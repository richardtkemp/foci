package platform

import (
	"testing"
)

func TestIsConvertibleDocMIME(t *testing.T) {
	// Verifies that IsConvertibleDocMIME correctly
	// identifies document types that can be converted to text.
	tests := []struct {
		mime string
		want bool
	}{
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", true},
		{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", true},
		{"application/vnd.openxmlformats-officedocument.presentationml.presentation", true},
		{"text/html", true},
		{"text/csv", true},
		{"text/plain", true},
		{"application/pdf", false},
		{"image/jpeg", false},
		{"application/zip", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsConvertibleDocMIME(tt.mime); got != tt.want {
			t.Errorf("IsConvertibleDocMIME(%q) = %v, want %v", tt.mime, got, tt.want)
		}
	}
}

func TestNormalizeMIME(t *testing.T) {
	// Verifies that NormalizeMIME strips parameters after ';' and maps
	// legacy MIME types to their modern equivalents.
	tests := []struct {
		input string
		want  string
	}{
		// No-op for already-canonical types
		{"text/html", "text/html"},
		{"application/pdf", "application/pdf"},
		{"image/jpeg", "image/jpeg"},

		// Strip parameters
		{"text/html; charset=utf-8", "text/html"},
		{"text/plain; charset=us-ascii", "text/plain"},
		{"text/csv; header=present", "text/csv"},
		{"application/pdf; version=1.7", "application/pdf"},
		{"image/png; foo=bar", "image/png"},

		// Legacy → modern mappings
		{"application/msword", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{"application/vnd.ms-excel", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
		{"application/vnd.ms-powerpoint", "application/vnd.openxmlformats-officedocument.presentationml.presentation"},

		// Legacy with parameters (strip then map)
		{"application/msword; charset=binary", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},

		// Unknown types pass through unchanged
		{"application/zip", "application/zip"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := NormalizeMIME(tt.input); got != tt.want {
			t.Errorf("NormalizeMIME(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestMIMEChecksWithLegacyTypes(t *testing.T) {
	// Verifies that IsConvertibleDocMIME recognizes legacy Office MIME types
	// (application/msword, application/vnd.ms-excel, application/vnd.ms-powerpoint).
	if !IsConvertibleDocMIME("application/msword") {
		t.Error("application/msword should be convertible")
	}
	if !IsConvertibleDocMIME("application/vnd.ms-excel") {
		t.Error("application/vnd.ms-excel should be convertible")
	}
	if !IsConvertibleDocMIME("application/vnd.ms-powerpoint") {
		t.Error("application/vnd.ms-powerpoint should be convertible")
	}
}
