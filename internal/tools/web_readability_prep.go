package tools

import (
	"strings"

	"golang.org/x/net/html"
)

// neutralizeMisleadingAttrs rewrites class/id attributes that trip
// readability's name-based heuristics on content it should keep (#2066).
// Readability judges a node partly by its class and id, and two common
// patterns misfire:
//
//   - Headings. Any class/id containing "header" makes a node an "unlikely
//     candidate", deleted before scoring (Substack's in-body headings are
//     h2.header-anchor-post, so 14 of 15 section headings vanished, text and
//     all). A slug id like "media-coverage" or "related-work" hits the
//     negative-weight list, and cleanHeaders then drops the h1/h2. A heading's
//     own class/id never says anything useful about whether it is content, so
//     both are removed from every h1-h6.
//   - Footnotes. "footnote" is on the negative-weight list, so a footnote
//     container scores below zero and cleanConditionally deletes it: the
//     in-text anchors survive while the footnote bodies (sometimes the most
//     substantive text on the page) are dropped. Class tokens containing
//     "footnote" are removed, as is an id containing it.
//
// Both consumers (web_fetch, HTML attachments) render the article to Markdown,
// which carries neither classes nor ids, so dropping them loses nothing.
func neutralizeMisleadingAttrs(n *html.Node) {
	if n.Type == html.ElementNode {
		switch n.Data {
		case "h1", "h2", "h3", "h4", "h5", "h6":
			removeAttrs(n, func(a html.Attribute) bool { return a.Key == "class" || a.Key == "id" })
		default:
			neutralizeFootnoteAttrs(n)
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		neutralizeMisleadingAttrs(c)
	}
}

func neutralizeFootnoteAttrs(n *html.Node) {
	for i := 0; i < len(n.Attr); i++ {
		a := &n.Attr[i]
		switch a.Key {
		case "class":
			var kept []string
			for _, tok := range strings.Fields(a.Val) {
				if !strings.Contains(strings.ToLower(tok), "footnote") {
					kept = append(kept, tok)
				}
			}
			a.Val = strings.Join(kept, " ")
		case "id":
			if strings.Contains(strings.ToLower(a.Val), "footnote") {
				a.Val = ""
			}
		}
	}
	removeAttrs(n, func(a html.Attribute) bool { return (a.Key == "class" || a.Key == "id") && a.Val == "" })
}

func removeAttrs(n *html.Node, drop func(html.Attribute) bool) {
	kept := n.Attr[:0]
	for _, a := range n.Attr {
		if !drop(a) {
			kept = append(kept, a)
		}
	}
	n.Attr = kept
}
