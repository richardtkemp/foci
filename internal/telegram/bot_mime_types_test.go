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
