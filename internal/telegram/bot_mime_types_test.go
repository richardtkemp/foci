package telegram

import (
	"testing"
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
	// Verifies that isConvertibleDocMIME correctly
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
		if got := isConvertibleDocMIME(tt.mime); got != tt.want {
			t.Errorf("isConvertibleDocMIME(%q) = %v, want %v", tt.mime, got, tt.want)
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
