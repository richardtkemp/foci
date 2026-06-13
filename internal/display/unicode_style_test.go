package display

import (
	"testing"
)

func TestApplyBold(t *testing.T) {
	// Verifies that ApplyBold maps A-Z, a-z, 0-9 to Mathematical Sans-Serif
	// Bold codepoints, leaving punctuation and spaces unchanged.
	tests := []struct {
		in, want string
	}{
		{"Hello", "𝗛𝗲𝗹𝗹𝗼"},
		{"ABC", "𝗔𝗕𝗖"},
		{"xyz", "𝘅𝘆𝘇"},
		{"Test 123!", "𝗧𝗲𝘀𝘁 𝟭𝟮𝟯!"},
		{"", ""},
		{"...", "..."},
	}
	for _, tt := range tests {
		got := ApplyBold(tt.in)
		if got != tt.want {
			t.Errorf("ApplyBold(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestApplyItalic(t *testing.T) {
	// Verifies that ApplyItalic maps A-Z, a-z to Mathematical Sans-Serif
	// Italic codepoints, leaving digits and punctuation unchanged.
	tests := []struct {
		in, want string
	}{
		{"Hello", "𝘏𝘦𝘭𝘭𝘰"},
		{"Test 123", "𝘛𝘦𝘴𝘵 123"}, // digits unchanged
		{"", ""},
	}
	for _, tt := range tests {
		got := ApplyItalic(tt.in)
		if got != tt.want {
			t.Errorf("ApplyItalic(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestApplyBoldItalic(t *testing.T) {
	// Verifies bold-italic mapping. Digits fall back to bold.
	tests := []struct {
		in, want string
	}{
		{"Hello", "𝙃𝙚𝙡𝙡𝙤"},
		{"A1", "𝘼𝟭"}, // letter bold-italic, digit bold
		{"", ""},
	}
	for _, tt := range tests {
		got := ApplyBoldItalic(tt.in)
		if got != tt.want {
			t.Errorf("ApplyBoldItalic(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDegradeMarkdown(t *testing.T) {
	// Verifies that DegradeMarkdown converts markdown formatting to Unicode
	// equivalents suitable for pre-formatted blocks.
	tests := []struct {
		name     string
		in, want string
	}{
		{
			name: "bold",
			in:   "**Hello**",
			want: "𝗛𝗲𝗹𝗹𝗼",
		},
		{
			name: "italic star",
			in:   "*Hello*",
			want: "𝘏𝘦𝘭𝘭𝘰",
		},
		{
			name: "bold italic",
			in:   "***Hello***",
			want: "𝙃𝙚𝙡𝙡𝙤",
		},
		{
			name: "underline to bold",
			in:   "__Hello__",
			want: "𝗛𝗲𝗹𝗹𝗼",
		},
		{
			name: "strikethrough collapses",
			in:   "~~deleted~~",
			want: "~deleted~",
		},
		{
			name: "inline code stripped",
			in:   "`code`",
			want: "code",
		},
		{
			name: "link text only",
			in:   "[click here](https://example.com)",
			want: "click here",
		},
		{
			name: "mixed in sentence",
			in:   "the **count** is *high*",
			want: "the 𝗰𝗼𝘂𝗻𝘁 is 𝘩𝘪𝘨𝘩",
		},
		{
			name: "bold with punctuation",
			in:   "**hello, world!**",
			want: "𝗵𝗲𝗹𝗹𝗼, 𝘄𝗼𝗿𝗹𝗱!",
		},
		{
			name: "no markdown unchanged",
			in:   "plain text",
			want: "plain text",
		},
		{
			name: "empty string",
			in:   "",
			want: "",
		},
		{
			name: "bold with digits",
			in:   "**v2**",
			want: "𝘃𝟮",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DegradeMarkdown(tt.in)
			if got != tt.want {
				t.Errorf("DegradeMarkdown(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestStyledRuneDisplayWidth(t *testing.T) {
	// Verifies that Mathematical Sans-Serif characters have display width 1,
	// matching their visual appearance in monospace fonts.
	bold := ApplyBold("Hello")
	if w := DisplayWidth(bold); w != 5 {
		t.Errorf("DisplayWidth(bold %q) = %d, want 5", bold, w)
	}

	italic := ApplyItalic("World")
	if w := DisplayWidth(italic); w != 5 {
		t.Errorf("DisplayWidth(italic %q) = %d, want 5", italic, w)
	}

	// Mixed: bold with punctuation — same width as original
	mixed := ApplyBold("hi, 42!")
	if w := DisplayWidth(mixed); w != 7 {
		t.Errorf("DisplayWidth(bold mixed %q) = %d, want 7", mixed, w)
	}
}
