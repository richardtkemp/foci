package telegram

import (
	"fmt"
	"strings"
	"testing"
)

func TestHtmlTagName(t *testing.T) {
	// Verifies that htmlTagName correctly extracts tag names
	// from HTML tag strings.
	tests := []struct {
		in   string
		want string
	}{
		{"pre>", "pre"},
		{"a href=\"url\">", "a"},
		{"div/", "div"},
		{"b", "b"},
		{"code>text", "code"},
	}
	for _, tt := range tests {
		got := htmlTagName(tt.in)
		if got != tt.want {
			t.Errorf("htmlTagName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestUnescapeJSONStringLiterals(t *testing.T) {
	// Verifies that JSON escape sequences are
	// properly converted to actual characters.
	tests := []struct {
		in   string
		want string
	}{
		{`hello\nworld`, "hello\nworld"},
		{`col1\tcol2`, "col1\tcol2"},
		{`a\nb\tc`, "a\nb\tc"},
		{"no escapes", "no escapes"},
		{"", ""},
	}
	for _, tt := range tests {
		got := unescapeJSONStringLiterals(tt.in)
		if got != tt.want {
			t.Errorf("unescapeJSONStringLiterals(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestHtmlEscapeBot(t *testing.T) {
	// Verifies that HTML special characters are properly
	// escaped.
	tests := []struct {
		in   string
		want string
	}{
		{"a & b", "a &amp; b"},
		{"<tag>", "&lt;tag&gt;"},
		{"safe text", "safe text"},
		{"a & <b> end", "a &amp; &lt;b&gt; end"},
		{"", ""},
	}
	for _, tt := range tests {
		got := htmlEscapeBot(tt.in)
		if got != tt.want {
			t.Errorf("htmlEscapeBot(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	// Verifies that truncate properly shortens strings and adds
	// ellipsis when needed.
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 5, "hello..."},
		{"", 5, ""},
	}
	for _, tt := range tests {
		got := truncate(tt.in, tt.max)
		if got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
		}
	}
}

func TestSanitizeError(t *testing.T) {
	// Verifies that sanitizeError removes sensitive tokens
	// from error messages.
	b := &Bot{botToken: "secret123"}

	// nil error
	if got := b.sanitizeError(nil); got != "" {
		t.Errorf("sanitizeError(nil) = %q, want empty", got)
	}

	// error without token
	if got := b.sanitizeError(fmt.Errorf("timeout")); got != "timeout" {
		t.Errorf("sanitizeError = %q, want 'timeout'", got)
	}

	// error with token
	if got := b.sanitizeError(fmt.Errorf("request to secret123/method failed")); !strings.Contains(got, "[REDACTED]") {
		t.Errorf("sanitizeError should redact token, got %q", got)
	}
	if strings.Contains(b.sanitizeError(fmt.Errorf("request to secret123/method failed")), "secret123") {
		t.Error("token should be redacted")
	}

	// empty token
	b2 := &Bot{botToken: ""}
	if got := b2.sanitizeError(fmt.Errorf("some error")); got != "some error" {
		t.Errorf("sanitizeError with empty token = %q", got)
	}
}

func TestFindSplitPoint(t *testing.T) {
	// Verifies that findSplitPoint correctly identifies where
	// to split text.
	tests := []struct {
		name   string
		text   string
		maxLen int
		want   int
	}{
		{"shorter than max", "hello", 100, 5},
		{"exact length", "hello", 5, 5},
		{"newline boundary", "hello\nworld\nfoo", 12, 12}, // split at second \n + 1
		{"no newline", "abcdefghij", 5, 5},
		{"inside HTML tag", "abc<b>def", 5, 3}, // split before '<'
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findSplitPoint(tt.text, tt.maxLen)
			if got != tt.want {
				t.Errorf("findSplitPoint(%q, %d) = %d, want %d", tt.text, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestSplitChunk(t *testing.T) {
	// Simple split — no open tags
	chunk, rest := splitChunk("hello world this is long", 11)
	if chunk != "hello world" || rest != " this is long" {
		t.Errorf("simple split: chunk=%q, rest=%q", chunk, rest)
	}

	// Split with open HTML tag — should close and reopen
	chunk, rest = splitChunk("<b>hello world</b>", 10)
	if !strings.HasSuffix(chunk, "</b>") {
		t.Errorf("chunk should end with </b>, got %q", chunk)
	}
	if !strings.HasPrefix(rest, "<b>") {
		t.Errorf("rest should start with <b>, got %q", rest)
	}
}
