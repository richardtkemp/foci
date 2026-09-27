package tools

import (
	"bytes"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// Readability's own class/id rules for "unlikely candidates" (go-readability
// internal/re2go/grab-article.re, not importable): a node whose class+id
// matches unlikelyCandidateRx and not maybeCandidateRx is deleted before
// scoring. Kept in step with the pinned go-readability version.
var (
	unlikelyCandidateRx = regexp.MustCompile(`(?i)-ad-|ai2html|banner|breadcrumbs|combx|comment|community|cover-wrap|disqus|extra|footer|gdpr|header|legends|menu|related|remark|replies|rss|shoutbox|sidebar|skyscraper|social|sponsor|supplemental|ad-break|agegate|pagination|pager|popup|yom-remote`)
	maybeCandidateRx    = regexp.MustCompile(`(?i)and|article|body|column|content|main|shadow`)
	displayNoneRx       = regexp.MustCompile(`(?i)display\s*:\s*none`)
	visibilityHiddenRx  = regexp.MustCompile(`(?i)visibility\s*:\s*hidden`)
)

// contentRegions returns the page as the extractor sees it (streamed segments
// resolved, misleading class/ids neutralized, as in ParseArticle) with every
// region it rejected removed: hidden nodes, readability's unlikely candidates
// (comment threads, sidebars, footers, menus) and the page-chrome elements
// isBoilerplateElement names. It is the baseline the thin and list-drop
// warnings compare the extraction against (#2069): measured against the whole
// page, a post whose comment thread was rightly left out read as a thin
// extraction missing hundreds of chars of list text — the comments'.
//
// A region matching those rules that the extraction draws on was evidently
// NOT rejected, and is kept (its own sub-regions are still judged): one that
// is mostly extracted, or one that holds most of the extraction. Readability
// retries without the unlikely-candidate rules when they leave too little, and
// on LessWrong the post sits inside both div.commentOnSelection and, after
// streamed segments are resolved, span.Header-headerHeight — which also holds
// the comment thread. Returns nil if the body doesn't parse.
func contentRegions(body []byte, extracted string) *html.Node {
	doc, err := html.Parse(bytes.NewReader(resolveStreamedSegments(body)))
	if err != nil {
		return nil
	}
	neutralizeMisleadingAttrs(doc)
	pruneRejectedRegions(doc, stripSpace(extracted))
	return doc
}

func pruneRejectedRegions(n *html.Node, have string) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if c.Type == html.ElementNode && isRejectedRegion(c) && !drawnOnByExtraction(c, have) {
			n.RemoveChild(c)
		} else {
			pruneRejectedRegions(c, have)
		}
		c = next
	}
}

// extractedTextMinChars is the shortest text node drawnOnByExtraction weighs:
// shorter ones (labels, dates, vote counts) say nothing about where a region
// ended up.
const extractedTextMinChars = 20

// drawnOnByExtraction reports whether at least half of n's substantive text
// (text nodes of extractedTextMinChars or more, whitespace removed) appears in
// have, the whitespace-stripped extraction, or whether that text makes up at
// least half of have. A comment thread that quotes a line of the post meets
// neither.
func drawnOnByExtraction(n *html.Node, have string) bool {
	var total, found int
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "template":
				return
			}
		}
		if n.Type == html.TextNode {
			if t := stripSpace(n.Data); len(t) >= extractedTextMinChars {
				total += len(t)
				if strings.Contains(have, t) {
					found += len(t)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return found > 0 && (2*found >= total || 2*found >= len(have))
}

// isRejectedRegion mirrors the node-removal rules readability applies before
// scoring (isProbablyVisible, unlikely candidates, unlikely roles), plus the
// page-chrome elements readability cleans out of the article afterwards.
func isRejectedRegion(n *html.Node) bool {
	if isBoilerplateElement(n) || !isProbablyVisible(n) {
		return true
	}
	switch n.Data {
	case "html", "body", "a":
		return false
	}
	switch attrValue(n, "role") {
	case "alert", "alertdialog", "dialog":
		return true
	}
	match := attrValue(n, "class") + " " + attrValue(n, "id")
	return unlikelyCandidateRx.MatchString(match) && !maybeCandidateRx.MatchString(match) &&
		!hasAncestorWithin(n, "table", 3) && !hasAncestorWithin(n, "code", 3)
}

func isProbablyVisible(n *html.Node) bool {
	style := attrValue(n, "style")
	return !displayNoneRx.MatchString(style) && !visibilityHiddenRx.MatchString(style) &&
		!hasAttr(n, "hidden") &&
		(attrValue(n, "aria-hidden") != "true" || strings.Contains(attrValue(n, "class"), "fallback-image"))
}

// hasAncestorWithin reports whether one of n's nearest maxDepth+1 ancestors is
// a tag element, as readability's hasAncestorTag does.
func hasAncestorWithin(n *html.Node, tag string, maxDepth int) bool {
	for depth := 0; n.Parent != nil && depth <= maxDepth; depth++ {
		if n.Parent.Type == html.ElementNode && n.Parent.Data == tag {
			return true
		}
		n = n.Parent
	}
	return false
}
