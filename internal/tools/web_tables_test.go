package tools

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	readability "github.com/go-shiori/go-readability"
	"golang.org/x/net/html"
)

const oxylabsFixture = "webfetch_corpus/oxylabs_restricted_targets.html"

func readFixture(t *testing.T, file string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func renderNode(t *testing.T, n *html.Node) string {
	t.Helper()
	var b bytes.Buffer
	if err := html.Render(&b, n); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestAriaTablesToHTML(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{
			name: "header row becomes thead, cells keep their children, wrappers go",
			in: `<div role="table" class="x"><div role="rowgroup"><div role="row"><div role="columnheader">Category</div><div role="columnheader">Example</div></div></div>` +
				`<div class="wrap"><div role="rowgroup"><div role="row"><div role="cell"><p>Government websites</p></div><div role="cell"><p>usa.gov</p></div></div></div></div></div>`,
			want: `<table><thead><tr><th>Category</th><th>Example</th></tr></thead><tbody><tr><td><p>Government websites</p></td><td><p>usa.gov</p></td></tr></tbody></table>`,
		},
		{
			name: "grid with gridcell and rowheader, no header row",
			in:   `<div role="grid"><div role="row"><span role="rowheader">Mon</span><span role="gridcell">9-5</span></div></div>`,
			want: `<table><tbody><tr><th>Mon</th><td>9-5</td></tr></tbody></table>`,
		},
		{
			name: "text outside the cells: left alone",
			in:   `<div role="table"><div>Caption that is not in a cell</div><div role="row"><div role="cell">a</div><div role="cell">b</div></div></div>`,
			want: `<div role="table"><div>Caption that is not in a cell</div><div role="row"><div role="cell">a</div><div role="cell">b</div></div></div>`,
		},
		{
			name: "no rows: left alone",
			in:   `<div role="grid"><div>photo one</div><div>photo two</div></div>`,
			want: `<div role="grid"><div>photo one</div><div>photo two</div></div>`,
		},
		{
			name: "native table with a role: left alone",
			in:   `<table role="table"><tbody><tr role="row"><td role="cell">a</td></tr></tbody></table>`,
			want: `<table role="table"><tbody><tr role="row"><td role="cell">a</td></tr></tbody></table>`,
		},
		{
			name: "nested ARIA table inside a cell is rebuilt too",
			in:   `<div role="table"><div role="row"><div role="cell">outer</div><div role="cell"><div role="table"><div role="row"><div role="cell">inner</div></div></div></div></div></div>`,
			want: `<table><tbody><tr><td>outer</td><td><table><tbody><tr><td>inner</td></tr></tbody></table></td></tr></tbody></table>`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			doc, err := html.Parse(strings.NewReader("<html><body>" + c.in + "</body></html>"))
			if err != nil {
				t.Fatal(err)
			}
			ariaTablesToHTML(doc)
			got := renderNode(t, doc)
			want := "<html><head></head><body>" + c.want + "</body></html>"
			if got != want {
				t.Errorf("got\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// TestMissingTableRows: with readability's own handling (no ARIA rewrite) the
// Oxylabs table loses 6 of its 9 rows, header included (#3241). The
// detector must see that loss; after ParseArticle it must see none.
func TestMissingTableRows(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("https://developers.oxylabs.io/proxies/residential-proxies/restricted-targets")
	oxy := readFixture(t, oxylabsFixture)

	def, err := readability.FromReader(bytes.NewReader(oxy), u)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(def.TextContent, "Government websites") {
		t.Fatal("precondition: default readability now keeps the Government row; this test no longer covers a drop")
	}
	missing, example := missingTableRows(contentRegions(oxy, def.TextContent), def.TextContent)
	if missing < tableDropMinRows || !strings.Contains(example, " | ") {
		t.Errorf("default readability on Oxylabs: missing=%d example=%q, want >= %d rows", missing, example, tableDropMinRows)
	}

	fixed, err := ParseArticle(bytes.NewReader(oxy), u)
	if err != nil {
		t.Fatal(err)
	}
	if missing, example := missingTableRows(contentRegions(oxy, fixed.TextContent), fixed.TextContent); missing != 0 {
		t.Errorf("ParseArticle on Oxylabs: missing=%d (%q), want 0", missing, example)
	}
}

func TestJudgeTableRow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, row, have string
		want            bool
	}{
		{"both cells absent", `<tr><td>Government websites</td><td>usa.gov, canada.ca</td></tr>`, "other text", true},
		{"half absent counts", `<tr><td>Ads</td><td>Google Tag Manager</td></tr>`, "Google Tag Manager", true},
		{"present", `<tr><td>Gaming</td><td>Playstation, Steam</td></tr>`, "Gaming Playstation, Steam", false},
		{"ARIA row absent", `<div role="row"><div role="cell">Mailing</div><div role="cell">Outlook</div></div>`, "", true},
		{"one cell is not a data row", `<tr><td>Only one cell with a lot of text</td></tr>`, "", false},
		{"too short", `<tr><td>a</td><td>b</td></tr>`, "", false},
		{"mostly links (a menu)", `<tr><td><a href="/a">Home page</a></td><td><a href="/b">About us</a></td></tr>`, "", false},
		{"nested table is layout", `<tr><td>Sidebar text</td><td><table><tr><td>x</td><td>y</td></tr></table></td></tr>`, "", false},
		{"too long is layout", `<tr><td>Nav</td><td>` + strings.Repeat("article prose ", 100) + `</td></tr>`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			markup := c.row
			if strings.HasPrefix(markup, "<tr") {
				markup = "<table>" + markup + "</table>"
			}
			doc, err := html.Parse(strings.NewReader("<html><body>" + markup + "</body></html>"))
			if err != nil {
				t.Fatal(err)
			}
			var row *html.Node
			var find func(*html.Node)
			find = func(n *html.Node) {
				if row == nil && n.Type == html.ElementNode && (n.Data == "tr" || attrValue(n, "role") == "row") {
					row = n
				}
				for k := n.FirstChild; k != nil; k = k.NextSibling {
					find(k)
				}
			}
			find(doc)
			if row == nil {
				t.Fatal("no row parsed")
			}
			if text, got := judgeTableRow(row, stripSpace(c.have)); got != c.want {
				t.Errorf("judgeTableRow = %v (%q), want %v", got, text, c.want)
			}
		})
	}
}

// TestWebFetchTableDropNote proves the note is wired into web_fetch: a stray
// text node inside the Oxylabs table makes ariaTablesToHTML leave it alone
// (text outside the cells), so readability drops the rows as it did before
// #3241, and the note must say so.
func TestWebFetchTableDropNote(t *testing.T) {
	t.Parallel()
	oxy := string(readFixture(t, oxylabsFixture))
	const marker = `role="table">`
	if strings.Count(oxy, marker) != 1 {
		t.Fatalf("fixture has %d %q, want 1", strings.Count(oxy, marker), marker)
	}
	broken := strings.Replace(oxy, marker, marker+"<span>Note outside the rows</span>", 1)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "oxy.html"), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	got := fetchFixtureAt(t, filepath.Join(dir, "oxy.html"))
	if strings.Contains(got, "Government websites") {
		t.Fatal("precondition: rows survived without the rewrite; this test no longer covers a drop")
	}
	if !strings.Contains(got, "table rows on the page are missing") {
		t.Errorf("table-drop note missing from:\n%s", got)
	}
}

// TestWebFetchRendersTables: a kept table is a Markdown table, one row per
// line, not its cells run together (#3241).
func TestWebFetchRendersTables(t *testing.T) {
	t.Parallel()
	got := fetchFixture(t, oxylabsFixture)
	for _, want := range []string{
		"| Target category ",
		"| Government websites                      | usa.gov, canada.ca               |",
		"| IP checking websites*                    | ipinfo.info, 2ip.io              |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing table line %q in:\n%s", want, got)
		}
	}
}
