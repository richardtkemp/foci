package telegram

import (
	"testing"

	"foci/internal/platform"
)

func TestIsImageMIME(t *testing.T) {
	// Verifies that isImageMIME correctly identifies image MIME
	// types.
	tests := []struct {
		mime string
		want bool
	}{
		{"image/jpeg", true},
		{"image/png", true},
		{"image/gif", true},
		{"image/webp", true},
		{"application/pdf", false},
		{"text/plain", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isImageMIME(tt.mime); got != tt.want {
			t.Errorf("isImageMIME(%q) = %v, want %v", tt.mime, got, tt.want)
		}
	}
}

func TestExtForMediaType(t *testing.T) {
	// Verifies that extForMediaType returns correct file
	// extensions for MIME types.
	tests := []struct {
		mt   string
		want string
	}{
		{"image/jpeg", ".jpg"},
		{"image/png", ".png"},
		{"image/gif", ".gif"},
		{"image/webp", ".webp"},
		{"application/pdf", ".pdf"},
		{"image/tiff", ".bin"},
		{"", ".bin"},
	}
	for _, tt := range tests {
		if got := extForMediaType(tt.mt); got != tt.want {
			t.Errorf("extForMediaType(%q) = %q, want %q", tt.mt, got, tt.want)
		}
	}
}

func TestIsConvertibleDocMIME(t *testing.T) {
	// Verifies that platform.IsConvertibleDocMIME correctly
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
		if got := platform.IsConvertibleDocMIME(tt.mime); got != tt.want {
			t.Errorf("IsConvertibleDocMIME(%q) = %v, want %v", tt.mime, got, tt.want)
		}
	}
}

func TestIsPDFMIME(t *testing.T) {
	// Verifies that isPDFMIME correctly identifies PDF MIME types.
	if !isPDFMIME("application/pdf") {
		t.Error("application/pdf should be PDF")
	}
	if isPDFMIME("image/jpeg") {
		t.Error("image/jpeg should not be PDF")
	}
	if isPDFMIME("application/json") {
		t.Error("application/json should not be PDF")
	}
	if isPDFMIME("") {
		t.Error("empty string should not be PDF")
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
		if got := platform.NormalizeMIME(tt.input); got != tt.want {
			t.Errorf("NormalizeMIME(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestMIMEChecksWithParameters(t *testing.T) {
	// Verifies that platform.IsConvertibleDocMIME, isPDFMIME, and isImageMIME all
	// handle MIME types with parameters (e.g. "; charset=utf-8").
	if !platform.IsConvertibleDocMIME("text/html; charset=utf-8") {
		t.Error("text/html with charset should be convertible")
	}
	if !platform.IsConvertibleDocMIME("text/csv; header=present") {
		t.Error("text/csv with params should be convertible")
	}
	if !isPDFMIME("application/pdf; version=1.7") {
		t.Error("application/pdf with params should be PDF")
	}
	if !isImageMIME("image/jpeg; quality=85") {
		t.Error("image/jpeg with params should be image")
	}
}

func TestMIMEChecksWithLegacyTypes(t *testing.T) {
	// Verifies that platform.IsConvertibleDocMIME recognizes legacy Office MIME types
	// (application/msword, application/vnd.ms-excel, application/vnd.ms-powerpoint).
	if !platform.IsConvertibleDocMIME("application/msword") {
		t.Error("application/msword should be convertible")
	}
	if !platform.IsConvertibleDocMIME("application/vnd.ms-excel") {
		t.Error("application/vnd.ms-excel should be convertible")
	}
	if !platform.IsConvertibleDocMIME("application/vnd.ms-powerpoint") {
		t.Error("application/vnd.ms-powerpoint should be convertible")
	}
}
