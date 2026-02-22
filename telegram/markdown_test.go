package telegram

import "testing"

func TestHTMLEscape(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{"<b>bold</b>", "&lt;b&gt;bold&lt;/b&gt;"},
		{"a & b", "a &amp; b"},
		{`say "hi"`, "say &quot;hi&quot;"},
		{"<>&\"", "&lt;&gt;&amp;&quot;"},
	}
	for _, tt := range tests {
		got := htmlEscape(tt.in)
		if got != tt.want {
			t.Errorf("htmlEscape(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestConvertToTelegramHTML(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bold",
			in:   "this is **bold** text",
			want: "this is <b>bold</b> text",
		},
		{
			name: "italic star",
			in:   "this is *italic* text",
			want: "this is <i>italic</i> text",
		},
		{
			name: "italic underscore",
			in:   "this is _italic_ text",
			want: "this is <i>italic</i> text",
		},
		{
			name: "underline",
			in:   "this is __underline__ text",
			want: "this is <u>underline</u> text",
		},
		{
			name: "strikethrough",
			in:   "this is ~~deleted~~ text",
			want: "this is <s>deleted</s> text",
		},
		{
			name: "spoiler",
			in:   "this is ||spoiler|| text",
			want: "this is <tg-spoiler>spoiler</tg-spoiler> text",
		},
		{
			name: "inline code",
			in:   "use `fmt.Println` here",
			want: "use <code>fmt.Println</code> here",
		},
		{
			name: "code block",
			in:   "```go\nfmt.Println(\"hi\")\n```",
			want: "<pre><code>fmt.Println(&quot;hi&quot;)</code></pre>",
		},
		{
			name: "code block HTML escaping",
			in:   "```\n<script>alert(1)</script>\n```",
			want: "<pre><code>&lt;script&gt;alert(1)&lt;/script&gt;</code></pre>",
		},
		{
			name: "link",
			in:   "click [here](https://example.com)",
			want: `click <a href="https://example.com">here</a>`,
		},
		{
			name: "heading",
			in:   "# My Title",
			want: "<b>My Title</b>",
		},
		{
			name: "heading h2",
			in:   "## Subtitle",
			want: "<b>Subtitle</b>",
		},
		{
			name: "blockquote single line",
			in:   "> quoted text",
			want: "<blockquote>quoted text</blockquote>",
		},
		{
			name: "blockquote multiline",
			in:   "> line 1\n> line 2\n> line 3",
			want: "<blockquote>line 1\nline 2\nline 3</blockquote>",
		},
		{
			name: "blockquote separated by text",
			in:   "> first\nsome text\n> second",
			want: "<blockquote>first</blockquote>\nsome text\n<blockquote>second</blockquote>",
		},
		{
			name: "mixed formatting",
			in:   "**bold** and *italic* and `code`",
			want: "<b>bold</b> and <i>italic</i> and <code>code</code>",
		},
		{
			name: "code block protects inner content",
			in:   "```\n**not bold** and *not italic*\n```",
			want: "<pre><code>**not bold** and *not italic*</code></pre>",
		},
		{
			name: "inline code protects inner content",
			in:   "use `**not bold**` here",
			want: "use <code>**not bold**</code> here",
		},
		{
			name: "plain text unchanged",
			in:   "hello world",
			want: "hello world",
		},
		{
			name: "empty string",
			in:   "",
			want: "",
		},
		// --- New features ---
		{
			name: "horizontal rule dashes",
			in:   "above\n---\nbelow",
			want: "above\n————————————————\nbelow",
		},
		{
			name: "horizontal rule stars",
			in:   "above\n***\nbelow",
			want: "above\n————————————————\nbelow",
		},
		{
			name: "horizontal rule underscores",
			in:   "above\n___\nbelow",
			want: "above\n————————————————\nbelow",
		},
		{
			name: "bullet list dash",
			in:   "- first item\n- second item",
			want: "  • first item\n  • second item",
		},
		{
			name: "bullet list star",
			in:   "* first item\n* second item",
			want: "  • first item\n  • second item",
		},
		{
			name: "ordered list",
			in:   "1. first\n2. second\n3. third",
			want: "  1. first\n  2. second\n  3. third",
		},
		{
			name: "table simple",
			in:   "| Col1 | Col2 |\n|------|------|\n| a    | b    |",
			want: "<pre>| Col1 | Col2 |\n|------|------|\n| a    | b    |</pre>",
		},
		{
			name: "table with HTML chars",
			in:   "| Key | Value |\n|-----|-------|\n| a<b | c&d   |",
			want: "<pre>| Key | Value |\n|-----|-------|\n| a&lt;b | c&amp;d   |</pre>",
		},
		{
			name: "table surrounded by text",
			in:   "Results:\n| Name | Score |\n|------|-------|\n| Bob  | 42    |\nDone.",
			want: "Results:\n<pre>| Name | Score |\n|------|-------|\n| Bob  | 42    |</pre>\nDone.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConvertToTelegramHTML(tt.in)
			if got != tt.want {
				t.Errorf("ConvertToTelegramHTML(%q)\n  got  = %q\n  want = %q", tt.in, got, tt.want)
			}
		})
	}
}
