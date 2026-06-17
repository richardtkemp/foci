package telegram

import (
	"fmt"
	"strings"
	"testing"

	"foci/internal/display"
)

func TestHTMLEscape(t *testing.T) {
	// Verifies that htmlEscape correctly escapes HTML special characters
	// (&, <, >) while leaving safe text and quotes unchanged.
	tests := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{"<b>bold</b>", "&lt;b&gt;bold&lt;/b&gt;"},
		{"a & b", "a &amp; b"},
		{`say "hi"`, `say "hi"`},
		{"<>&\"", "&lt;&gt;&amp;\""},
	}
	for _, tt := range tests {
		got := htmlEscape(tt.in)
		if got != tt.want {
			t.Errorf("htmlEscape(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestConvertToTelegramHTML(t *testing.T) {
	// Verifies that ConvertToTelegramHTML correctly converts Markdown to Telegram
	// HTML across a wide range of features: bold, italic, code blocks, links, tables,
	// headings, lists, blockquotes, horizontal rules, snake_case protection, and HTML
	// escaping. Each subtest targets a specific formatting rule or edge case.
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
			name: "italic nested inside bold",
			in:   "**1. The actual *fix* here.**",
			want: "<b>1. The actual <i>fix</i> here.</b>",
		},
		{
			name: "bold nested inside italic",
			in:   "*a **b** c*",
			want: "<i>a <b>b</b> c</i>",
		},
		{
			name: "two bolds with italic between",
			in:   "**one** *mid* **two**",
			want: "<b>one</b> <i>mid</i> <b>two</b>",
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
			want: "<pre><code>fmt.Println(\"hi\")</code></pre>",
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
			want: "<pre>Col1  Col2\n──────────\na     b   </pre>",
		},
		{
			name: "table with HTML chars",
			in:   "| Key | Value |\n|-----|-------|\n| a<b | c&d   |",
			want: "<pre>Key  Value\n──────────\na&lt;b  c&amp;d  </pre>",
		},
		{
			name: "table surrounded by text",
			in:   "Results:\n| Name | Score |\n|------|-------|\n| Bob  | 42    |\nDone.",
			want: "Results:\n<pre>Name  Score\n───────────\nBob   42   </pre>\nDone.",
		},
		{
			name: "table uneven columns padded",
			in:   "| A | BB | CCC |\n|---|-------|---|\n| x | y | z |",
			want: "<pre>A    BB   CCC\n─────────────\nx    y    z  </pre>",
		},
		{
			name: "table with Chinese characters",
			in:   "| 名称 | 值 |\n|------|------|\n| 测试 | 123 |",
			want: "<pre>名称  值 \n─────────\n测试  123</pre>",
		},
		{
			name: "table with mixed width characters",
			in:   "| Name | 数量 |\n|------|------|\n| apple | 苹果 |\n| banana | 香蕉 |",
			want: "<pre>Name    数量\n────────────\napple   苹果\nbanana  香蕉</pre>",
		},
		{
			name: "table with emoji",
			in:   "| Status | Count |\n|--------|-------|\n| ✅ | 5 |\n| ❌ | 2 |",
			want: "<pre>Status  Count\n─────────────\n✅      5    \n❌      2    </pre>",
		},
		// Tables with markdown in cells — degraded to Unicode styled text
		{
			name: "table with bold cells",
			in:   "| Name | Status |\n|------|--------|\n| **Alpha** | done |",
			want: "<pre>Name   Status\n─────────────\n𝗔𝗹𝗽𝗵𝗮  done  </pre>",
		},
		{
			name: "table with italic cells",
			in:   "| Key | Note |\n|-----|------|\n| foo | *bar* |",
			want: "<pre>Key  Note\n─────────\nfoo  𝘣𝘢𝘳 </pre>",
		},
		{
			name: "table with inline code cells",
			in:   "| Cmd | Desc |\n|-----|------|\n| `ls` | list |",
			want: "<pre>Cmd  Desc\n─────────\nls   list</pre>",
		},
		{
			name: "table bold header and plain data",
			in:   "| **Tool** | **Count** |\n|----------|----------|\n| exec | 5 |",
			want: "<pre>𝗧𝗼𝗼𝗹  𝗖𝗼𝘂𝗻𝘁\n───────────\nexec  5    </pre>",
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
		// Inline code adjacent to parens must NOT be consumed by the link
		// regex. Regression for a bug where the [INLINECODE%d] placeholder
		// collided with the `[text](url)` pattern, silently dropping the
		// inline code content and leaking the placeholder as link anchor text.
		{
			name: "inline code followed by parens not treated as link",
			in:   "`code`(url)",
			want: "<code>code</code>(url)",
		},
		{
			name: "inline code function signature",
			in:   "use `Printf`(format, args) here",
			want: "use <code>Printf</code>(format, args) here",
		},
		{
			name: "two adjacent inline codes then parens",
			in:   "`first` `second`(c)",
			want: "<code>first</code> <code>second</code>(c)",
		},
		{
			name: "code block followed by parens",
			in:   "```\nbody\n```\n(not a link)",
			want: "<pre><code>body</code></pre>\n(not a link)",
		},
		// A fenced code block containing single backticks (e.g. a shell
		// command with `$(...)` or grep `\`pattern\``) must render with the
		// full content intact. This is the formatToolInput → markdown
		// pipeline for permission prompt commands.
		{
			name: "code block with internal single backticks",
			in:   "```\ngrep -oP '`(high|med|low)`' file\n```",
			want: "<pre><code>grep -oP '`(high|med|low)`' file</code></pre>",
		},
		// Stray NUL bytes in input must be stripped so they can't corrupt
		// placeholder boundaries (we use NUL as the placeholder delimiter).
		{
			name: "nul bytes stripped from input",
			in:   "before\x00after",
			want: "beforeafter",
		},
		// Literal "[INLINECODE0]" in input should render as-is — placeholders
		// use NUL delimiters now, so this form is just text.
		{
			name: "literal bracketed placeholder form passes through",
			in:   "see [INLINECODE0] token",
			want: "see [INLINECODE0] token",
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

// TestConvertToTelegramHTMLNestedEmphasis pins the triple-marker / overlapping
// emphasis family. These are the cases the sequential bold-then-italic regex
// passes get wrong: a leading "***" or a "***" closing across an open italic
// produces crossed tags like "<b><i>x</b> y</i>", which Telegram's parser
// rejects with "Unmatched end tag ... expected </i>, found </b>".
//
// Real-world repro (helen, 2026-06-17): the markdown "***Όποιος** θέλει*"
// was emitted as "<b><i>Όποιος</b> θέλει</i>" → Telegram 400 → plain-text
// fallback leaked raw tags to the user. Correct output keeps italic as the
// outer span: "<i><b>Όποιος</b> θέλει</i>".
//
// Expected outputs follow CommonMark emphasis semantics (the inner-most marker
// binds tightest; when "***" wraps a whole span the emphasis is outer and the
// strong is inner).
func TestConvertToTelegramHTMLNestedEmphasis(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "triple marker bold span then italic tail (helen repro)",
			in:   "***Όποιος** θέλει*",
			want: "<i><b>Όποιος</b> θέλει</i>",
		},
		{
			name: "triple marker bold span then italic tail ascii",
			in:   "***bold** rest*",
			want: "<i><b>bold</b> rest</i>",
		},
		{
			name: "triple marker whole span ascii",
			in:   "***word***",
			want: "<i><b>word</b></i>",
		},
		{
			name: "triple marker whole span unicode",
			in:   "***Όποιος***",
			want: "<i><b>Όποιος</b></i>",
		},
		{
			name: "italic open then bold tail closing with triple",
			in:   "*lead **bold***",
			want: "<i>lead <b>bold</b></i>",
		},
		{
			name: "bold open then italic tail closing with triple",
			in:   "**lead *italic***",
			want: "<b>lead <i>italic</i></b>",
		},
		{
			name: "triple marker italic span then bold tail",
			in:   "*italic* then **bold**",
			want: "<i>italic</i> then <b>bold</b>",
		},
		{
			name: "bold with underscore italic inside",
			in:   "**_word_**",
			want: "<b><i>word</i></b>",
		},
		{
			name: "control: italic wrapping bold (already valid)",
			in:   "*a **b** c*",
			want: "<i>a <b>b</b> c</i>",
		},
		{
			name: "control: bold wrapping italic (already valid)",
			in:   "**a *b* c**",
			want: "<b>a <i>b</i> c</b>",
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

// TestConvertToTelegramHTMLEmphasisWellFormed is the structural backstop: for a
// corpus of emphasis-heavy inputs, the converter's output must contain only
// properly-nested formatting tags (no crossed or unclosed tags). This catches
// the crossed-nesting failure mode generally — Telegram rejects ANY mis-nested
// tag, not just the specific strings enumerated above — so it guards against
// regressions in inputs we didn't think to spell out.
func TestConvertToTelegramHTMLEmphasisWellFormed(t *testing.T) {
	corpus := []string{
		"***Όποιος** θέλει*",
		"***word***",
		"*lead **bold***",
		"**lead *italic***",
		"***a** b **c***",
		"*a **b** c*",
		"**a *b* c**",
		"~~strike *italic* end~~",
		"__under **bold** line__",
		"a ***b*** c ***d** e*",
		"**bold** and *italic* and ~~strike~~",
		"***",      // bare triple — must not emit unbalanced tags
		"****word**", // stray marker — must still be well-formed
	}

	for _, in := range corpus {
		t.Run(in, func(t *testing.T) {
			got := ConvertToTelegramHTML(in)
			if err := checkTagNesting(got); err != nil {
				t.Errorf("ConvertToTelegramHTML(%q) = %q: %v", in, got, err)
			}
		})
	}
}

// checkTagNesting verifies that the Telegram formatting tags in s are properly
// nested using a stack: every closing tag must match the most-recently-opened
// tag, and all tags must be closed. Only the inline formatting tags Telegram
// supports are tracked; other "<...>" are ignored (body text is HTML-escaped
// upstream, so real tag-like text won't appear).
func checkTagNesting(s string) error {
	tracked := map[string]bool{"b": true, "i": true, "u": true, "s": true, "tg-spoiler": true, "code": true, "pre": true}
	var stack []string
	for i := 0; i < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		end := strings.IndexByte(s[i:], '>')
		if end < 0 {
			break
		}
		inner := s[i+1 : i+end]
		i += end
		closing := false
		if strings.HasPrefix(inner, "/") {
			closing = true
			inner = inner[1:]
		}
		// Strip attributes (e.g. `a href="..."`); we only track bare formatting tags.
		name := inner
		if sp := strings.IndexByte(name, ' '); sp >= 0 {
			name = name[:sp]
		}
		if !tracked[name] {
			continue
		}
		if closing {
			if len(stack) == 0 {
				return fmt.Errorf("closing </%s> with no open tag", name)
			}
			top := stack[len(stack)-1]
			if top != name {
				return fmt.Errorf("crossed tags: expected </%s>, found </%s>", top, name)
			}
			stack = stack[:len(stack)-1]
		} else {
			stack = append(stack, name)
		}
	}
	if len(stack) > 0 {
		return fmt.Errorf("unclosed tags: %v", stack)
	}
	return nil
}

func TestConvertToTelegramHTMLTableWrapping(t *testing.T) {
	// Verifies that ConvertToTelegramHTML respects RenderOpts for table rendering:
	// constrained width, line wrapping, truncation, and different table styles
	// (pretty vs. markdown). Each subtest exercises a distinct combination of options.
	tests := []struct {
		name string
		in   string
		opts display.RenderOpts
		want string
	}{
		{
			name: "table constrained and wrapped (markdown)",
			in:   "| Tool | Description |\n|------|-------------|\n| exec | Execute shell commands in a sandbox |",
			opts: display.RenderOpts{MaxWidth: 30, WrapLines: 5, Style: "markdown"},
			want: "<pre>| Tool | Description         |\n| ---- | ------------------- |\n| exec | Execute shell       |\n|      | commands in a       |\n|      | sandbox             |</pre>",
		},
		{
			name: "wrap lines cap with truncation (markdown)",
			in:   "| Col |\n|-----|\n| one two three four five six seven eight |",
			opts: display.RenderOpts{MaxWidth: 15, WrapLines: 2, Style: "markdown"},
			want: "<pre>| Col         |\n| ----------- |\n| one two     |\n| three four… |</pre>",
		},
		{
			name: "separator row stays single line (markdown)",
			in:   "| A | B |\n|---|---|\n| x | y |",
			opts: display.RenderOpts{MaxWidth: 40, WrapLines: 5, Style: "markdown"},
			want: "<pre>| A   | B   |\n| --- | --- |\n| x   | y   |</pre>",
		},
		{
			name: "no opts pretty default",
			in:   "| Col1 | Col2 |\n|------|------|\n| a    | b    |",
			opts: display.RenderOpts{},
			want: "<pre>Col1  Col2\n──────────\na     b   </pre>",
		},
		{
			name: "wrap disabled truncates (markdown)",
			in:   "| Name |\n|------|\n| a very long name here |",
			opts: display.RenderOpts{MaxWidth: 15, WrapLines: 0, Style: "markdown"},
			want: "<pre>| Name        |\n| ----------- |\n| a very lon… |</pre>",
		},
		{
			name: "pretty style with wrapping",
			in:   "| Tool | Description |\n|------|-------------|\n| exec | Execute shell commands in a sandbox |",
			opts: display.RenderOpts{MaxWidth: 28, WrapLines: 5},
			want: "<pre>Tool  Description           \n────────────────────────────\nexec  Execute shell commands\n      in a sandbox          </pre>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConvertToTelegramHTML(tt.in, tt.opts)
			if got != tt.want {
				t.Errorf("ConvertToTelegramHTML with opts\n  got  = %q\n  want = %q", got, tt.want)
			}
		})
	}
}

func TestClosePartialMarkdown(t *testing.T) {
	// Verifies that closePartialMarkdown strips or closes unmatched markdown
	// delimiters so that partial streaming text can be safely converted to HTML.
	// Each case simulates a mid-stream buffer state with incomplete syntax.
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no markdown passthrough",
			in:   "plain text",
			want: "plain text",
		},
		{
			name: "complete bold unchanged",
			in:   "**bold**",
			want: "**bold**",
		},
		{
			name: "unmatched bold closed",
			in:   "**Bold tex",
			want: "**Bold tex**",
		},
		{
			name: "unmatched bold at start with text after",
			in:   "hello **world",
			want: "hello **world**",
		},
		{
			name: "complete italic unchanged",
			in:   "*italic*",
			want: "*italic*",
		},
		{
			name: "unmatched italic star closed",
			in:   "hello *world",
			want: "hello *world*",
		},
		{
			name: "unmatched italic underscore closed",
			in:   "hello _world",
			want: "hello _world_",
		},
		{
			name: "complete strikethrough unchanged",
			in:   "~~deleted~~",
			want: "~~deleted~~",
		},
		{
			name: "unmatched strikethrough closed",
			in:   "hello ~~deleted",
			want: "hello ~~deleted~~",
		},
		{
			name: "complete underline unchanged",
			in:   "__underline__",
			want: "__underline__",
		},
		{
			name: "unmatched underline closed",
			in:   "hello __under",
			want: "hello __under__",
		},
		{
			name: "complete code fence unchanged",
			in:   "```\ncode\n```",
			want: "```\ncode\n```",
		},
		{
			name: "unclosed code fence closed",
			in:   "before\n```\nsome code",
			want: "before\n```\nsome code\n```",
		},
		{
			name: "unclosed code fence at start",
			in:   "```\ncode here",
			want: "```\ncode here\n```",
		},
		{
			name: "complete inline code unchanged",
			in:   "use `code` here",
			want: "use `code` here",
		},
		{
			name: "unmatched backtick closed",
			in:   "use `code",
			want: "use `code`",
		},
		{
			name: "mixed complete and partial",
			in:   "**bold** and *ital",
			want: "**bold** and *ital*",
		},
		{
			name: "bold with italic inside complete",
			in:   "**bold *italic***",
			// The lone * inside is closed by appending — already valid markdown
			want: "**bold *italic****",
		},
		{
			name: "empty string",
			in:   "",
			want: "",
		},
		{
			name: "multiple complete delimiters",
			in:   "**a** and ~~b~~ and `c`",
			want: "**a** and ~~b~~ and `c`",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := closePartialMarkdown(tt.in)
			if got != tt.want {
				t.Errorf("closePartialMarkdown(%q)\n  got  = %q\n  want = %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestClosePartialMarkdown_ThenConvert(t *testing.T) {
	// Verifies the full pipeline: closePartialMarkdown followed by
	// ConvertToTelegramHTML produces valid output for streaming scenarios.
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "partial bold becomes bold HTML",
			in:   "**Bold tex",
			want: "<b>Bold tex</b>",
		},
		{
			name: "complete bold becomes HTML",
			in:   "**Bold text**",
			want: "<b>Bold text</b>",
		},
		{
			name: "partial code fence closed",
			in:   "before\n```\ncode",
			want: "before\n<pre><code>code</code></pre>",
		},
		{
			name: "complete code fence becomes pre",
			in:   "before\n```\ncode\n```",
			want: "before\n<pre><code>code</code></pre>",
		},
		{
			name: "partial inline code closed",
			in:   "use `func",
			want: "use <code>func</code>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			closed := closePartialMarkdown(tt.in)
			got := ConvertToTelegramHTML(closed)
			if got != tt.want {
				t.Errorf("pipeline(%q)\n  closed = %q\n  got    = %q\n  want   = %q", tt.in, closed, got, tt.want)
			}
		})
	}
}
