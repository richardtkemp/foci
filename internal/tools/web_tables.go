package tools

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// ariaTablesToHTML rewrites tables built from ARIA roles on generic elements
// (<div role="table"> holding role="row" and role="cell") into real <table>
// markup (#3241). Docs platforms such as GitBook render every table this
// way. Readability only recognises <table> as tabular: it scored each cell
// as a short div and removed most rows (Oxylabs' restricted-targets page
// kept 2 of 8, losing "Government websites"). As a real table with <th>
// header cells, readability marks it a data table and keeps it whole, and
// the Markdown converter renders it as a table.
//
// A table is rebuilt only when every piece of its visible text sits inside a
// cell, so nothing outside the rows is lost. Cell contents are moved, not
// copied. Native <table> elements are left as they are.
func ariaTablesToHTML(n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		ariaTablesToHTML(c)
		c = next
	}
	if n.Type == html.ElementNode && n.DataAtom != atom.Table && isAriaTableRole(attrValue(n, "role")) {
		rebuildAriaTable(n)
	}
}

func isAriaTableRole(role string) bool {
	switch role {
	case "table", "grid", "treegrid":
		return true
	}
	return false
}

// ariaCellTag maps an ARIA cell role to the table element that carries it.
func ariaCellTag(role string) (atom.Atom, bool) {
	switch role {
	case "cell", "gridcell":
		return atom.Td, true
	case "columnheader", "rowheader":
		return atom.Th, true
	}
	return 0, false
}

// rebuildAriaTable replaces the ARIA table t with a real <table> whose rows
// are t's role="row" descendants and whose cells are each row's cell-role
// descendants. Inner tables were already rebuilt (ariaTablesToHTML recurses
// first), so the walks stop at any <table>.
func rebuildAriaTable(t *html.Node) {
	var rows []*html.Node
	walkUntilTable(t, func(n *html.Node) bool {
		if attrValue(n, "role") == "row" {
			rows = append(rows, n)
			return false
		}
		return true
	})
	if len(rows) == 0 {
		return
	}
	cellsOf := make([][]*html.Node, len(rows))
	cellText := 0
	for i, r := range rows {
		walkUntilTable(r, func(n *html.Node) bool {
			if _, ok := ariaCellTag(attrValue(n, "role")); ok {
				cellsOf[i] = append(cellsOf[i], n)
				cellText += len(stripSpace(nodeText(n)))
				return false
			}
			return true
		})
	}
	// Text outside the cells (a caption, a stray note) would be lost in the
	// rebuild: leave such a table alone.
	if cellText == 0 || cellText != len(stripSpace(nodeText(t))) {
		return
	}

	table := &html.Node{Type: html.ElementNode, Data: "table", DataAtom: atom.Table}
	body := &html.Node{Type: html.ElementNode, Data: "tbody", DataAtom: atom.Tbody}
	for i, cells := range cellsOf {
		if len(cells) == 0 {
			continue
		}
		tr := &html.Node{Type: html.ElementNode, Data: "tr", DataAtom: atom.Tr}
		for _, c := range cells {
			tag, _ := ariaCellTag(attrValue(c, "role"))
			cell := &html.Node{Type: html.ElementNode, Data: tag.String(), DataAtom: tag}
			for k := c.FirstChild; k != nil; {
				next := k.NextSibling
				c.RemoveChild(k)
				cell.AppendChild(k)
				k = next
			}
			tr.AppendChild(cell)
		}
		// A leading row of header cells becomes the <thead>.
		if i == 0 && allHeaderCells(cells) {
			head := &html.Node{Type: html.ElementNode, Data: "thead", DataAtom: atom.Thead}
			head.AppendChild(tr)
			table.AppendChild(head)
			continue
		}
		body.AppendChild(tr)
	}
	table.AppendChild(body)
	t.Parent.InsertBefore(table, t)
	t.Parent.RemoveChild(t)
}

func allHeaderCells(cells []*html.Node) bool {
	for _, c := range cells {
		if tag, _ := ariaCellTag(attrValue(c, "role")); tag != atom.Th {
			return false
		}
	}
	return true
}

// walkUntilTable calls visit on each element below n in document order,
// descending into an element only when visit returns true. It never enters a
// <table>: that table's rows and cells belong to it.
func walkUntilTable(n *html.Node, visit func(*html.Node) bool) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || c.DataAtom == atom.Table {
			continue
		}
		if visit(c) {
			walkUntilTable(c, visit)
		}
	}
}

// nodeText returns the text below n, skipping script, style and template
// content.
func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			return
		}
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.Script, atom.Style, atom.Template:
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// tableDropNote is appended when table rows on the page are absent from the
// extraction (#3241). A dropped row costs a few dozen characters, far below
// what the thin check can see, and a table row is not a list item, so the
// list-drop check is blind to it too. One lost row can be the answer
// ("Government websites" on a proxy provider's restricted-targets table).
const tableDropNote = "\n\n---\n_[foci: %d table rows on the page are missing from this extraction (e.g. %q) — part of a table was probably skipped by the extractor. If this looks incomplete, retry with `raw=true`.]_"

const (
	// tableDropMinRows is the number of missing rows that triggers
	// tableDropNote.
	tableDropMinRows = 2
	// tableRowMinChars skips rows too short to judge.
	tableRowMinChars = 8
	// tableRowMaxChars skips rows that are page layout (a sidebar cell next
	// to the article), not data.
	tableRowMaxChars = 1000
)

// missingTableRows counts the table rows in doc (the contentRegions
// baseline) whose cells are mostly absent from the extracted text, and
// returns the first such row as an example, its cells joined with " | ".
// A row is a <tr> or an element with role="row"; its cells are td/th or the
// ARIA cell roles. Rows that hold a nested table (layout), rows with fewer
// than two non-empty cells, rows shorter than tableRowMinChars or longer
// than tableRowMaxChars, and rows that are mostly link text (menus) are not
// judged. A row is missing when at least half of its non-empty cells are
// absent, presence checked with all whitespace removed from both sides.
func missingTableRows(doc *html.Node, extracted string) (missing int, example string) {
	if doc == nil {
		return 0, ""
	}
	have := stripSpace(extracted)
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.DataAtom == atom.Tr || attrValue(n, "role") == "row") {
			if row, ok := judgeTableRow(n, have); ok {
				missing++
				if example == "" {
					example = row
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return missing, example
}

// judgeTableRow reports whether row is a judgeable data row that is missing
// from have, and returns its cells' text joined with " | ".
func judgeTableRow(row *html.Node, have string) (string, bool) {
	if hasDescendant(row, isTableElement) {
		return "", false // a table inside a row is page layout
	}
	var cells []string
	textLen, linkLen := 0, 0
	walkUntilTable(row, func(n *html.Node) bool {
		if !isTableCell(n) {
			return true
		}
		if text, linkText := textAndLinks(n, false); text != "" {
			cells = append(cells, text)
			textLen += len(text)
			linkLen += len(linkText)
		}
		return false
	})
	text := strings.Join(cells, " | ")
	if len(cells) < 2 || len(text) < tableRowMinChars || len(text) > tableRowMaxChars || 2*linkLen >= textLen {
		return "", false
	}
	absent := 0
	for _, c := range cells {
		if !strings.Contains(have, stripSpace(c)) {
			absent++
		}
	}
	return text, 2*absent >= len(cells)
}

func isTableElement(n *html.Node) bool {
	return n.DataAtom == atom.Table || isAriaTableRole(attrValue(n, "role"))
}

func isTableCell(n *html.Node) bool {
	if n.DataAtom == atom.Td || n.DataAtom == atom.Th {
		return true
	}
	_, ok := ariaCellTag(attrValue(n, "role"))
	return ok
}

// hasDescendant reports whether any element below n satisfies match.
func hasDescendant(n *html.Node, match func(*html.Node) bool) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && (match(c) || hasDescendant(c, match)) {
			return true
		}
	}
	return false
}
