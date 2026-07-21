package voice

import "testing"

// #1444: NormalizeForSpeech must flatten the classes of markdown/symbol that
// garbled Orpheus's synthesis tail — a ticket reference ("#1443"), markdown
// emphasis/heading/code markers, an em-dash, and a path-style slash — into
// plain speakable text, without mangling ordinary words or hyphens.
func TestNormalizeForSpeech(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "ticket reference hash",
			input: "Filed as #1443.",
			want:  "Filed as 1443.",
		},
		{
			name:  "em dash becomes a spoken pause",
			input: "That is settled—let me capture it.",
			want:  "That is settled, let me capture it.",
		},
		{
			name:  "en dash with surrounding spaces",
			input: "9am – 5pm",
			want:  "9am, 5pm",
		},
		{
			name:  "markdown emphasis and code markers stripped",
			input: "Use **bold**, _italic_, and `code`.",
			want:  "Use bold, italic, and code.",
		},
		{
			name:  "markdown heading marker stripped",
			input: "# Heading text",
			want:  "Heading text",
		},
		{
			name:  "markdown link keeps only the label",
			input: "See [the doc](https://example.com/path) for details.",
			want:  "See the doc for details.",
		},
		{
			name:  "path-style slash becomes a word break",
			input: "filed under foci-client/voice-mode",
			want:  "filed under foci-client voice-mode",
		},
		{
			name:  "representative symbol-heavy string (repro of #1444's garbled tail)",
			input: "Filed as #1443 — see foci-client/voice-mode, **done**.",
			want:  "Filed as 1443, see foci-client voice-mode, done.",
		},
		{
			name:  "plain text unchanged",
			input: "Hello there, this is fine.",
			want:  "Hello there, this is fine.",
		},
		{
			name:  "plain hyphen preserved (not a dash or slash)",
			input: "foci-client is the repo name.",
			want:  "foci-client is the repo name.",
		},
		{
			name:  "empty input",
			input: "",
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeForSpeech(tt.input); got != tt.want {
				t.Errorf("NormalizeForSpeech(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
