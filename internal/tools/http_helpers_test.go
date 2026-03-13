package tools

import (
	"encoding/base64"
	"testing"
)

func TestIsBinaryContentType(t *testing.T) {
	// Verifies that image, audio, video, and opaque binary MIME types are detected as binary, while text and JSON types are not.
	t.Parallel()
	binary := []string{
		"image/png", "image/jpeg", "audio/mpeg", "video/mp4",
		"application/octet-stream", "application/pdf", "application/zip",
		"image/png; charset=utf-8",
	}
	for _, ct := range binary {
		if !isBinaryContentType(ct) {
			t.Errorf("isBinaryContentType(%q) = false, want true", ct)
		}
	}

	text := []string{
		"text/html", "text/plain", "application/json", "application/xml",
		"application/json; charset=utf-8", "application/ld+json",
		"application/vnd.api+json", "application/atom+xml", "",
	}
	for _, ct := range text {
		if isBinaryContentType(ct) {
			t.Errorf("isBinaryContentType(%q) = true, want false", ct)
		}
	}
}

func TestExtractJSONPath(t *testing.T) {
	// Verifies that dot-notation paths correctly traverse nested objects and arrays, and that missing keys or out-of-range indices return errors.
	t.Parallel()
	data := []byte(`{"data":[{"url":"hello"},{"url":"world"}],"name":"test"}`)

	tests := []struct {
		path string
		want string
	}{
		{"name", "test"},
		{"data.0.url", "hello"},
		{"data.1.url", "world"},
	}
	for _, tt := range tests {
		got, err := extractJSONPath(data, tt.path)
		if err != nil {
			t.Errorf("extractJSONPath(%q): %v", tt.path, err)
			continue
		}
		if got != tt.want {
			t.Errorf("extractJSONPath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}

	// Error cases
	_, err := extractJSONPath(data, "missing")
	if err == nil {
		t.Error("expected error for missing key")
	}
	_, err = extractJSONPath(data, "data.99")
	if err == nil {
		t.Error("expected error for out of range index")
	}
}

func TestDecodeDataURI(t *testing.T) {
	// Verifies that valid base64 data URIs are decoded correctly, and that non-data URIs or malformed URIs return errors.
	t.Parallel()
	raw := []byte{0x89, 0x50, 0x4e, 0x47}
	b64 := base64.StdEncoding.EncodeToString(raw)
	decoded, err := decodeDataURI("data:image/png;base64," + b64)
	if err != nil {
		t.Fatalf("decodeDataURI: %v", err)
	}
	if len(decoded) != len(raw) {
		t.Errorf("decoded %d bytes, want %d", len(decoded), len(raw))
	}

	// Not a data URI
	_, err = decodeDataURI("https://example.com")
	if err == nil {
		t.Error("expected error for non-data URI")
	}

	// Malformed (no comma)
	_, err = decodeDataURI("data:image/png;base64")
	if err == nil {
		t.Error("expected error for malformed data URI")
	}
}
