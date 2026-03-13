package telegram

import (
	"testing"

	"foci/internal/platform"
)

// TestIsImageMIME verifies that isImageMIME correctly identifies image MIME
// types.
func TestIsImageMIME(t *testing.T) {
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

// TestExtForMediaType verifies that extForMediaType returns correct file
// extensions for MIME types.
func TestExtForMediaType(t *testing.T) {
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

// TestIsConvertibleDocMIME verifies that platform.IsConvertibleDocMIME
// correctly identifies document types that can be converted to text.
func TestIsConvertibleDocMIME(t *testing.T) {
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

// TestIsPDFMIME verifies that isPDFMIME correctly identifies PDF MIME types.
func TestIsPDFMIME(t *testing.T) {
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
