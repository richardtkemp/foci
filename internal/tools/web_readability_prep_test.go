package tools

import (
	"strings"
	"testing"
)

// TestParseArticleKeepsMisnamedHeadingsAndFootnotes covers the #2066 shapes
// beyond the Substack corpus pages: a heading whose slug id hits readability's
// negative-weight list ("media"), and a footnotes block whose class and id both
// say "footnotes" (the id alone would still score it negative).
func TestParseArticleKeepsMisnamedHeadingsAndFootnotes(t *testing.T) {
	para := "<p>" + strings.Repeat("This is a long paragraph of real article prose, with commas, that readability should score highly. ", 6) + "</p>"
	page := `<html><head><title>Post</title></head><body>
<div class="nav"><a href="/">Home</a></div>
<article>` + para + `
<h2 id="media-coverage">Media coverage of the result</h2>` + para + para + `
<div id="footnotes" class="footnotes"><div class="footnote-content">
<p>The footnote body that must survive extraction, with enough words to matter to a reader.</p>
</div></div>
</article></body></html>`

	a, err := ParseArticle(strings.NewReader(page), nil)
	if err != nil {
		t.Fatalf("ParseArticle: %v", err)
	}
	for _, want := range []string{"Media coverage of the result", "The footnote body that must survive"} {
		if !strings.Contains(a.TextContent, want) {
			t.Errorf("extraction missing %q", want)
		}
	}
}
