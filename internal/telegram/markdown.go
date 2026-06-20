package telegram

import (
	"fmt"
	"foci/internal/display"
	"regexp"
	"strings"
)

// Precompiled regexes for markdown → HTML conversion.
// These are called ~4/s during streaming; compiling once avoids repeated work.
var (
	reCodeBlock        = regexp.MustCompile("(?m)^```(?:[a-z]+)?\n([\\s\\S]*?)\n```")
	reInlineCode       = regexp.MustCompile("`([^`]+)`")
	reLink             = regexp.MustCompile(`\[([^\]]+)\]\(([^\)]+)\)`)
	reSpoiler          = regexp.MustCompile(`\|\|([^\|]+)\|\|`)
	reStrikethrough    = regexp.MustCompile(`~~([^~]+)~~`)
	reUnderline        = regexp.MustCompile(`__([^_]+)__`)
	// Boundary class excludes letters (both cases), digits and underscore so
	// snake_case identifiers — including UPPER_SNAKE like MAX_TOKEN_LEN — never
	// open an italic run. A bare _x_ delimited by whitespace/punctuation still
	// italicises because the boundary is the delimiter, not the content. (#709)
	reItalicUnderscore = regexp.MustCompile(`(^|[^a-zA-Z0-9_])_([^_\n]+?)_([^a-zA-Z0-9_]|$)`)
	reHRule            = regexp.MustCompile(`(?m)^[-*_]{3,}\s*$`)
	reBulletList       = regexp.MustCompile(`(?m)^[-*]\s+(.+)$`)
	reOrderedList      = regexp.MustCompile(`(?m)^(\d+)\.\s+(.+)$`)
	reH1               = regexp.MustCompile(`(?m)^#\s+(.+)$`)
	reH2               = regexp.MustCompile(`(?m)^##\s+(.+)$`)
	reH3Plus           = regexp.MustCompile(`(?m)^###+ (.+)$`)
	reBlockquote       = regexp.MustCompile(`^> ?(.*)$`)
)

// ConvertToTelegramHTML converts standard markdown to Telegram's HTML format.
// HTML is simpler and safer than MarkdownV2 (no escaping hell).
//
// Conversions:
// - **text** -> <b>text</b> (bold)
// - *text* or _text_ -> <i>text</i> (italic)
// - __text__ -> <u>text</u> (underline)
// - ~~text~~ -> <s>text</s> (strikethrough)
// - `code` -> <code>code</code> (inline code)
// - ```lang\ncode\n``` -> <pre><code>code</code></pre> (code block)
// - [text](url) -> <a href="url">text</a> (links)
// - > blockquote -> <blockquote>blockquote</blockquote> (multiline)
// - # Heading -> <b>Heading</b> (bold, Telegram has no headings)
// - ||spoiler|| -> <tg-spoiler>spoiler</tg-spoiler> (Telegram spoiler)
// - | tables | -> <pre> block
// - --- / *** / ___ -> em-dash horizontal rule
// - - item / * item -> bullet lists
// - 1. item -> ordered lists
func ConvertToTelegramHTML(text string, opts ...display.RenderOpts) string {
	// Strip any stray NUL bytes from input. We use \x00 as the delimiter for
	// code-extraction placeholders (see below); a pre-existing NUL in the
	// input could corrupt placeholder boundaries. Valid UTF-8 text never
	// contains NUL, so this is a defensive no-op in normal use.
	text = strings.ReplaceAll(text, "\x00", "")

	// Convert markdown formatting in order of precedence
	// Code blocks first (preserve everything inside)
	var codeBlocks []string
	text = reCodeBlock.ReplaceAllStringFunc(text, func(match string) string {
		idx := len(codeBlocks)
		// Extract code (everything between ``` markers)
		parts := reCodeBlock.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		inner := htmlEscape(parts[1])
		codeBlocks = append(codeBlocks, "<pre><code>"+inner+"</code></pre>")
		return codeBlockPlaceholder(idx)
	})

	// Tables: detect blocks of lines containing | with a separator row (---).
	// Extract early to protect | chars from other conversions.
	var tOpts display.RenderOpts
	if len(opts) > 0 {
		tOpts = opts[0]
	}
	text = convertTables(text, &codeBlocks, tOpts)

	// Inline code (extract early to protect content)
	var inlineCodes []string
	text = reInlineCode.ReplaceAllStringFunc(text, func(match string) string {
		idx := len(inlineCodes)
		parts := reInlineCode.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		code := htmlEscape(parts[1])
		inlineCodes = append(inlineCodes, "<code>"+code+"</code>")
		return inlineCodePlaceholder(idx)
	})

	// Escape & and < in the body text after extracting code blocks and inline
	// code (which have their own escaping) but before markdown → HTML conversion.
	// Without this, stray < and & in model output break Telegram's HTML parser,
	// causing fallback to unformatted plain text with raw HTML tags visible.
	// We don't escape > because it's needed for blockquote syntax ("> text")
	// and is harmless in HTML outside of tags.
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")

	// Links: [text](url)
	text = reLink.ReplaceAllString(text, "<a href=\"$2\">$1</a>")

	// Spoilers: ||text||
	text = reSpoiler.ReplaceAllString(text, "<tg-spoiler>$1</tg-spoiler>")

	// Bold (**), italic (*) and overlapping/triple runs (***): handled together
	// by a delimiter-stack parser so closing tags always match the
	// most-recently-opened tag. Independent regex passes cannot do this — they
	// produced crossed tags like "<b><i>x</b> y</i>" on input "***x** y*",
	// which Telegram's HTML parser rejects.
	text = convertStarEmphasis(text)

	// Strikethrough: ~~text~~
	text = reStrikethrough.ReplaceAllString(text, "<s>$1</s>")

	// Underline: __text__
	text = reUnderline.ReplaceAllString(text, "<u>$1</u>")

	// Italic: _text_ (but not when part of snake_case identifiers like word_word_word)
	// Only matches single-word content between underscores (no additional underscores inside)
	// Uses capture groups to preserve surrounding characters
	text = reItalicUnderscore.ReplaceAllString(text, "$1<i>$2</i>$3")

	// Headings: relative hierarchy rendering based on levels actually used
	text = convertHeadings(text)

	// Horizontal rules: ---, ***, ___ (3+ chars on a line by themselves)
	text = reHRule.ReplaceAllString(text, "————————————————")

	// Bullet lists: - item or * item at start of line
	text = reBulletList.ReplaceAllString(text, "  • $1")

	// Ordered lists: 1. item — indent to align with bullets
	text = reOrderedList.ReplaceAllString(text, "  $1. $2")

	// Multiline blockquotes: consecutive > lines merged into single <blockquote>
	text = convertBlockquotes(text)

	// Restore code blocks and inline codes
	for i, code := range codeBlocks {
		text = strings.ReplaceAll(text, codeBlockPlaceholder(i), code)
	}
	for i, code := range inlineCodes {
		text = strings.ReplaceAll(text, inlineCodePlaceholder(i), code)
	}

	return text
}

// convertStarEmphasis converts asterisk emphasis — *italic*, **bold**, and
// overlapping/triple runs like ***x***, ***x** y*, **x *y*** — into properly
// nested <b>/<i> tags. It uses a delimiter stack so a closing run always
// matches the most-recently-opened tag, guaranteeing well-formed output. The
// previous independent bold-then-italic regex passes could not do this: on
// "***x** y*" they emitted the crossed "<b><i>x</b> y</i>", which Telegram's
// HTML parser rejects ("Unmatched end tag ... expected </i>, found </b>"),
// forcing a plain-text fallback that leaked raw tags to the user.
//
// Only '*' runs are handled here; '_' italic and '__' underline keep their own
// passes. Unmatched markers stay literal (preserving "2 * 3", a dangling
// "**bold", a "* item" bullet, and a standalone "***" rule for later passes).
//
// Emphasis pairing follows CommonMark's flanking rules in simplified form: a
// run can open only if the char after it is non-space, and can close only if
// the char before it is non-space. When a run wraps a whole span (odd length),
// italic is the outer tag and bold the inner (matching "<em><strong>").
func convertStarEmphasis(s string) string {
	type openTag struct {
		typ  byte // 'b' or 'i'
		node int  // index into nodes of the opening run
		seq  int  // index into that node's opens slice
	}
	type emit struct {
		closes []byte // tag types closed here, in order (inner→outer)
		litPre int    // literal stars emitted before opens (unmatched leftovers)
		opens  []byte // tag types opened here, in order (outer→inner)
		commit []bool // parallel to opens: whether each open was matched
		text   string // literal text (for non-star nodes)
		star   bool
	}

	// NUL is deliberately NOT whitespace here: code spans are extracted to
	// NUL-delimited placeholders before this pass, so a '*' run adjacent to a
	// placeholder sits next to a NUL. Emphasis must bind to that content
	// (e.g. **`code`** → <b><code>code</code></b>), so the NUL must read as a
	// non-space character. String-boundary handling (no char before/after) is
	// done separately via hasPrev/hasNext, not by a NUL sentinel.
	isSpace := func(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

	// Tokenize into star-run nodes and literal-text nodes.
	var nodes []emit
	for i := 0; i < len(s); {
		if s[i] == '*' {
			j := i
			for j < len(s) && s[j] == '*' {
				j++
			}
			nodes = append(nodes, emit{star: true})
			// store run length transiently in litPre; resolved during matching
			nodes[len(nodes)-1].litPre = j - i
			// flanking flags packed via text field is ugly; compute inline below
			i = j
		} else {
			j := i
			for j < len(s) && s[j] != '*' {
				j++
			}
			nodes = append(nodes, emit{text: s[i:j]})
			i = j
		}
	}

	// Second walk: matching. We need flanking context, so recompute run bounds.
	var stack []openTag
	// Recover per-node remaining-star counts (stored in litPre at tokenize time).
	rem := make([]int, len(nodes))
	canOpen := make([]bool, len(nodes))
	canClose := make([]bool, len(nodes))
	{
		pos := 0
		for idx, n := range nodes {
			if n.star {
				runLen := n.litPre
				var prev, next byte
				hasPrev := pos > 0
				if hasPrev {
					prev = s[pos-1]
				}
				hasNext := pos+runLen < len(s)
				if hasNext {
					next = s[pos+runLen]
				}
				rem[idx] = runLen
				// A run can open only with a non-space char after it, and close
				// only with a non-space char before it. At a string boundary
				// there is no such char, so the direction is disallowed.
				canOpen[idx] = hasNext && !isSpace(next)
				canClose[idx] = hasPrev && !isSpace(prev)
				nodes[idx].litPre = 0 // reset; becomes true leftover count below
				pos += runLen
			} else {
				pos += len(n.text)
			}
		}
	}

	for idx := range nodes {
		if !nodes[idx].star {
			continue
		}
		r := rem[idx]
		// Closing phase: consume open tags from the top of the stack.
		if canClose[idx] {
			for r > 0 && len(stack) > 0 {
				top := stack[len(stack)-1]
				need := 1
				if top.typ == 'b' {
					need = 2
				}
				if r < need {
					break
				}
				r -= need
				stack = stack[:len(stack)-1]
				nodes[top.node].commit[top.seq] = true
				nodes[idx].closes = append(nodes[idx].closes, top.typ)
			}
		}
		// Opening phase: italic outer (if odd), then bold pairs.
		if canOpen[idx] {
			if r%2 == 1 {
				nodes[idx].opens = append(nodes[idx].opens, 'i')
				nodes[idx].commit = append(nodes[idx].commit, false)
				stack = append(stack, openTag{typ: 'i', node: idx, seq: len(nodes[idx].opens) - 1})
				r--
			}
			for r >= 2 {
				nodes[idx].opens = append(nodes[idx].opens, 'b')
				nodes[idx].commit = append(nodes[idx].commit, false)
				stack = append(stack, openTag{typ: 'b', node: idx, seq: len(nodes[idx].opens) - 1})
				r -= 2
			}
		}
		nodes[idx].litPre = r // whatever stars neither closed nor opened
	}

	// Emit. Committed opens/closes become tags; uncommitted opens revert to
	// literal stars (i=1 star, b=2 stars). Order within a run: closes, then any
	// pure-leftover stars, then opens.
	tagOpen := map[byte]string{'b': "<b>", 'i': "<i>"}
	tagClose := map[byte]string{'b': "</b>", 'i': "</i>"}
	var b strings.Builder
	for _, n := range nodes {
		if !n.star {
			b.WriteString(n.text)
			continue
		}
		for _, t := range n.closes {
			b.WriteString(tagClose[t])
		}
		if n.litPre > 0 {
			b.WriteString(strings.Repeat("*", n.litPre))
		}
		for k, t := range n.opens {
			if n.commit[k] {
				b.WriteString(tagOpen[t])
			} else if t == 'b' {
				b.WriteString("**")
			} else {
				b.WriteString("*")
			}
		}
	}
	return b.String()
}

// Placeholder format for code extraction. We use NUL-byte delimiters rather
// than bracketed tokens because bracketed tokens can be mutated by later
// regex passes — notably reLink matching `[INLINECODE0](x)` as a link when
// inline code happens to sit adjacent to parens. NUL is unreachable via
// valid UTF-8 input (stripped at the top of ConvertToTelegramHTML) and does
// not appear in any markdown regex, so placeholders survive untouched.
func codeBlockPlaceholder(idx int) string  { return fmt.Sprintf("\x00CODEBLOCK%d\x00", idx) }
func inlineCodePlaceholder(idx int) string { return fmt.Sprintf("\x00INLINECODE%d\x00", idx) }

// convertTables finds markdown table blocks and converts them to <pre> blocks
// using display.DetectTables and display.RenderTable.
func convertTables(text string, codeBlocks *[]string, opts display.RenderOpts) string {
	lines := strings.Split(text, "\n")
	blocks := display.DetectTables(text)
	// Degrade markdown in table cells (bold → Unicode bold, etc.)
	// since HTML tags don't render inside <pre> blocks.
	opts.CellTransform = display.DegradeMarkdown
	// Process blocks in reverse order to preserve line indices
	for i := len(blocks) - 1; i >= 0; i-- {
		rendered := display.RenderTable(blocks[i].Lines, opts)
		idx := len(*codeBlocks)
		*codeBlocks = append(*codeBlocks, "<pre>"+htmlEscape(rendered)+"</pre>")
		replacement := []string{codeBlockPlaceholder(idx)}
		lines = append(lines[:blocks[i].StartLine], append(replacement, lines[blocks[i].EndLine:]...)...)
	}
	return strings.Join(lines, "\n")
}

// headingStyle represents the visual style for a heading level
type headingStyle int

const (
	headingBold headingStyle = iota
	headingDoubleLine
	headingTripleLine
)

// convertHeadings detects heading levels used and maps them to styles.
// Mapping is relative based on distinct levels:
// - 1 level: bold
// - 2 levels: double-line + bold
// - 3 levels: triple-line + double-line + bold
// - 4+ levels: triple-line + double-line + bold, extras all bold
func convertHeadings(text string) string {
	hasH1 := reH1.MatchString(text)
	hasH2 := reH2.MatchString(text)
	hasH3Plus := reH3Plus.MatchString(text)

	levels := 0
	if hasH1 {
		levels++
	}
	if hasH2 {
		levels++
	}
	if hasH3Plus {
		levels++
	}

	var h1Style, h2Style, h3Style headingStyle
	switch levels {
	case 1:
		h1Style = headingBold
		h2Style = headingBold
		h3Style = headingBold
	case 2:
		h1Style = headingDoubleLine
		h2Style = headingBold
		h3Style = headingBold
	default:
		h1Style = headingTripleLine
		h2Style = headingDoubleLine
		h3Style = headingBold
	}

	text = reH1.ReplaceAllStringFunc(text, func(m string) string {
		parts := reH1.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		return formatHeading(parts[1], h1Style)
	})
	text = reH2.ReplaceAllStringFunc(text, func(m string) string {
		parts := reH2.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		return formatHeading(parts[1], h2Style)
	})
	text = reH3Plus.ReplaceAllStringFunc(text, func(m string) string {
		parts := reH3Plus.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		return formatHeading(parts[1], h3Style)
	})

	return text
}

func formatHeading(title string, style headingStyle) string {
	switch style {
	case headingTripleLine:
		return "═══ " + title + " ═══"
	case headingDoubleLine:
		return "── " + title + " ──"
	default:
		return "<b>" + title + "</b>"
	}
}

// convertBlockquotes merges consecutive > lines into single <blockquote> tags.
func convertBlockquotes(text string) string {
	lines := strings.Split(text, "\n")

	var result []string
	i := 0
	for i < len(lines) {
		if m := reBlockquote.FindStringSubmatch(lines[i]); m != nil {
			// Start of a blockquote — collect consecutive > lines
			var bqLines []string
			for i < len(lines) {
				if m := reBlockquote.FindStringSubmatch(lines[i]); m != nil {
					bqLines = append(bqLines, m[1])
					i++
				} else {
					break
				}
			}
			result = append(result, "<blockquote>"+strings.Join(bqLines, "\n")+"</blockquote>")
		} else {
			result = append(result, lines[i])
			i++
		}
	}
	return strings.Join(result, "\n")
}

// closePartialMarkdown closes unmatched markdown delimiters so that
// ConvertToTelegramHTML produces valid HTML from incomplete streaming text.
// Unmatched markers are closed by appending their counterpart, so partial
// formatting renders correctly during streaming (e.g. "**Bold tex" becomes
// "**Bold tex**" → renders as bold). Handles: code fences (```), inline code
// (`), bold (**), strikethrough (~~), underline (__), and italic (* and _).
// Designed to be lightweight — called on every stream tick (~4/s).
func closePartialMarkdown(text string) string {
	// Handle unclosed code fences: if odd number of ``` occurrences,
	// close by appending a newline + fence so partial code renders as a block.
	fenceCount := strings.Count(text, "```")
	if fenceCount%2 != 0 {
		text += "\n```"
	}

	// Handle unclosed inline code: count unescaped backticks outside code fences.
	// After code fences are balanced above, remaining solo backticks are inline code.
	// Close by appending so partial inline code renders formatted.
	backtickCount := strings.Count(text, "`")
	if backtickCount%2 != 0 {
		text += "`"
	}

	// Paired delimiters: close trailing unmatched markers by appending.
	// This makes partial formatting render correctly during streaming
	// (e.g. "**Bold tex" → "**Bold tex**" → renders as bold).
	// Order matters — check multi-char delimiters before single-char.
	for _, delim := range []string{"**", "~~", "__"} {
		if strings.Count(text, delim)%2 != 0 {
			text += delim
		}
	}

	// Single-char italic markers: * and _ (but not ** or __ which are handled above).
	// Count standalone * (not part of **) and standalone _ (not part of __).
	// Close by appending rather than stripping so partial italic renders correctly.
	text = closeUnmatchedSingle(text, '*')
	text = closeUnmatchedSingle(text, '_')

	return text
}

// closeUnmatchedSingle closes a trailing unmatched single-char delimiter by
// appending it, ignoring occurrences that are part of a double-char delimiter
// (e.g. ** or __).
func closeUnmatchedSingle(text string, ch byte) string {
	// Count standalone occurrences of ch (not part of doubleCh).
	count := 0
	for i := 0; i < len(text); i++ {
		if text[i] != ch {
			continue
		}
		// Check if this is part of a double delimiter
		if i+1 < len(text) && text[i+1] == ch {
			i++ // skip the pair
			continue
		}
		if i > 0 && text[i-1] == ch {
			continue // second char of a pair already skipped
		}
		count++
	}
	if count%2 != 0 {
		text += string(ch)
	}
	return text
}
