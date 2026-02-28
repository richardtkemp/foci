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
			name: "heading single level h1 only",
			in:   "# My Title",
			want: "<b>My Title</b>",
		},
		{
			name: "heading single level h3 only",
			in:   "### Section",
			want: "<b>Section</b>",
		},
		{
			name: "heading single level h4 only",
			in:   "#### Deep Section",
			want: "<b>Deep Section</b>",
		},
		{
			name: "heading two levels h1 and h2",
			in:   "# Title\n## Subtitle",
			want: "── Title ──\n<b>Subtitle</b>",
		},
		{
			name: "heading two levels h1 and h3",
			in:   "# Title\n### Section",
			want: "── Title ──\n<b>Section</b>",
		},
		{
			name: "heading three levels h1 h2 h3",
			in:   "# Title\n## Subtitle\n### Section",
			want: "═══ Title ═══\n── Subtitle ──\n<b>Section</b>",
		},
		{
			name: "heading three levels h1 h2 h4",
			in:   "# Title\n## Subtitle\n#### Deep",
			want: "═══ Title ═══\n── Subtitle ──\n<b>Deep</b>",
		},
		{
			name: "heading four levels",
			in:   "# H1\n## H2\n### H3\n#### H4",
			want: "═══ H1 ═══\n── H2 ──\n<b>H3</b>\n<b>H4</b>",
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
			want: "<pre>| Col1 | Col2 |\n| ---- | ---- |\n| a    | b    |</pre>",
		},
		{
			name: "table with HTML chars",
			in:   "| Key | Value |\n|-----|-------|\n| a<b | c&d   |",
			want: "<pre>| Key | Value |\n| --- | ----- |\n| a&lt;b | c&amp;d   |</pre>",
		},
		{
			name: "table surrounded by text",
			in:   "Results:\n| Name | Score |\n|------|-------|\n| Bob  | 42    |\nDone.",
			want: "Results:\n<pre>| Name | Score |\n| ---- | ----- |\n| Bob  | 42    |</pre>\nDone.",
		},
		{
			name: "table uneven columns padded",
			in:   "| A | BB | CCC |\n|---|-------|---|\n| x | y | z |",
			want: "<pre>| A   | BB  | CCC |\n| --- | --- | --- |\n| x   | y   | z   |</pre>",
		},
		{
			name: "table with Chinese characters",
			in:   "| 名称 | 值 |\n|------|------|\n| 测试 | 123 |",
			want: "<pre>| 名称 | 值  |\n| ---- | --- |\n| 测试 | 123 |</pre>",
		},
		{
			name: "table with mixed width characters",
			in:   "| Name | 数量 |\n|------|------|\n| apple | 苹果 |\n| banana | 香蕉 |",
			want: "<pre>| Name   | 数量 |\n| ------ | ---- |\n| apple  | 苹果 |\n| banana | 香蕉 |</pre>",
		},
		{
			name: "table with emoji",
			in:   "| Status | Count |\n|--------|-------|\n| ✅ | 5 |\n| ❌ | 2 |",
			want: "<pre>| Status | Count |\n| ------ | ----- |\n| ✅     | 5     |\n| ❌     | 2     |</pre>",
		},
		// Snake case protection
		{
			name: "snake_case identifier protected",
			in:   "set inject_agent_warnings to true",
			want: "set inject_agent_warnings to true",
		},
		{
			name: "snake_case in sentence",
			in:   "Use memory_search for this",
			want: "Use memory_search for this",
		},
		{
			name: "snake_case already in code not double-wrapped",
			in:   "use `inject_agent_warnings` here",
			want: "use <code>inject_agent_warnings</code> here",
		},
		{
			name: "intentional italic preserved",
			in:   "this is _italic_ text",
			want: "this is <i>italic</i> text",
		},
		{
			name: "single underscore word not protected",
			in:   "some_var is ok",
			want: "some_var is ok",
		},
		{
			name: "multiple snake_case identifiers",
			in:   "config has cache_bust_detect and inject_agent_warnings",
			want: "config has cache_bust_detect and inject_agent_warnings",
		},
		{
			name: "snake_case with numbers",
			in:   "use v2_api_endpoint here",
			want: "use v2_api_endpoint here",
		},
		// HTML escaping in body text
		{
			name: "angle brackets in text escaped",
			in:   "use a <b> tag for bold",
			want: "use a &lt;b> tag for bold",
		},
		{
			name: "ampersand in text escaped",
			in:   "foo & bar",
			want: "foo &amp; bar",
		},
		{
			name: "angle brackets preserved in code block",
			in:   "```\n<div>hello</div>\n```",
			want: "<pre><code>&lt;div&gt;hello&lt;/div&gt;</code></pre>",
		},
		{
			name: "angle brackets preserved in inline code",
			in:   "use `<div>` for this",
			want: "use <code>&lt;div&gt;</code> for this",
		},
		{
			name: "thinking-like tags in text escaped",
			in:   "The model may output <think> tags",
			want: "The model may output &lt;think> tags",
		},
		{
			name: "math inequality in text escaped",
			in:   "when a < b and c > d",
			want: "when a &lt; b and c > d",
		},
		{
			name: "HTML escaping with bold markdown",
			in:   "**bold** and a < b",
			want: "<b>bold</b> and a &lt; b",
		},
		{
			name: "ampersand in link URL preserved",
			in:   "click [here](https://example.com?a=1&b=2)",
			want: `click <a href="https://example.com?a=1&amp;b=2">here</a>`,
		},
		// Edge cases for FindStringSubmatch guards
		{
			name: "code block with unicode box-drawing chars",
			in:   "```\n──────────\n```",
			want: "<pre><code>──────────</code></pre>",
		},
		{
			name: "code block no language no trailing newline content",
			in:   "```\nline1\nline2\n```",
			want: "<pre><code>line1\nline2</code></pre>",
		},
		{
			name: "empty backtick pair not panics",
			in:   "text `` more",
			want: "text `` more",
		},
		{
			name: "many inline codes beyond index 9",
			in:   "`a` `b` `c` `d` `e` `f` `g` `h` `i` `j` `k` `l`",
			want: "<code>a</code> <code>b</code> <code>c</code> <code>d</code> <code>e</code> <code>f</code> <code>g</code> <code>h</code> <code>i</code> <code>j</code> <code>k</code> <code>l</code>",
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
