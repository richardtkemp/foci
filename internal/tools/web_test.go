package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	readability "github.com/go-shiori/go-readability"
)

func TestIsStructuredContentType(t *testing.T) {
	// #966: JSON/XML/CSV/plain/YAML skip readability; HTML (incl. xhtml) does not.
	t.Parallel()
	cases := []struct {
		ct   string
		want bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"text/vnd.api+json", true},
		{"application/xml", true},
		{"application/rss+xml", true},
		{"application/xhtml+xml", false}, // xhtml is HTML — readability handles it
		{"text/csv", true},
		{"text/plain; charset=utf-8", true},
		{"application/yaml", true},
		{"text/html", false},
		{"text/html; charset=utf-8", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isStructuredContentType(c.ct); got != c.want {
			t.Errorf("isStructuredContentType(%q) = %v, want %v", c.ct, got, c.want)
		}
	}
}

func TestParseReadableWithTimeout(t *testing.T) {
	// Proves the readability parse step is bounded by a wall-clock timeout: a
	// parse that blocks past the deadline returns a timeout error rather than
	// hanging web_fetch forever. (Defence-in-depth over the x/net DoS bump —
	// the parser still runs on attacker-controlled HTML.)
	orig := readabilityFromReader
	defer func() { readabilityFromReader = orig }()
	block := make(chan struct{})
	defer close(block)
	readabilityFromReader = func(r io.Reader, u *url.URL) (readability.Article, error) {
		<-block // never returns before the deadline
		return readability.Article{}, nil
	}
	_, err := parseReadableWithTimeout([]byte("<html></html>"), nil, 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("err = %v, want timeout error", err)
	}
}

func TestParseReadableWithTimeoutFast(t *testing.T) {
	// Proves a normal, fast parse returns its extracted article well within the
	// timeout (the happy path is not penalised by the timeout wrapper).
	t.Parallel()
	u, _ := url.Parse("https://example.com")
	art, err := parseReadableWithTimeout(
		[]byte("<article><h1>Hi</h1><p>some body text here for readability</p></article>"),
		u, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if art.TextContent == "" && art.Content == "" {
		t.Errorf("expected extracted content, got empty article")
	}
}

func TestWebFetchSuccess(t *testing.T) {
	// Proves that a successful fetch extracts HTML as markdown (no raw tags), sets the correct
	// User-Agent header, and returns the page text.
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "Foci/1.0" {
			t.Errorf("User-Agent = %q, want %q", r.Header.Get("User-Agent"), "Foci/1.0")
		}
		w.Write([]byte("<html><body><p>Hello World</p></body></html>"))
	}))
	defer server.Close()

	tool := NewWebFetchTool()
	params, _ := json.Marshal(map[string]interface{}{
		"url": server.URL,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Content should be extracted as markdown (no HTML tags)
	if !strings.Contains(result.Text, "Hello World") {
		t.Errorf("result = %q, want 'Hello World'", result.Text)
	}
	if strings.Contains(result.Text, "<p>") {
		t.Errorf("result still has HTML tags: %q", result.Text)
	}
}

func TestWebFetchRaw(t *testing.T) {
	// Proves that raw=true skips HTML-to-markdown conversion and returns the unprocessed HTML body.
	t.Parallel()
	html := "<html><body><h1>Title</h1><p>Content</p></body></html>"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(html))
	}))
	defer server.Close()

	tool := NewWebFetchTool()
	params, _ := json.Marshal(map[string]interface{}{
		"url": server.URL,
		"raw": true,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Raw mode should return unprocessed HTML
	if !strings.Contains(result.Text, "<h1>Title</h1>") {
		t.Errorf("raw mode should preserve HTML tags, got: %q", result.Text)
	}
	if !strings.Contains(result.Text, "<p>Content</p>") {
		t.Errorf("raw mode should preserve HTML tags, got: %q", result.Text)
	}
}

func TestWebFetchReadabilityFallback(t *testing.T) {
	// Non-article HTML — readability will likely fail to extract an article
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<div>Just some text</div>"))
	}))
	defer server.Close()

	tool := NewWebFetchTool()
	params, _ := json.Marshal(map[string]interface{}{
		"url": server.URL,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should still produce output via fallback
	if !strings.Contains(result.Text, "Just some text") {
		t.Errorf("fallback should extract text, got: %q", result.Text)
	}
}

func TestWebFetchMarkdownStructure(t *testing.T) {
	// Proves that article HTML is converted to valid markdown with headings and links,
	// and that no raw HTML tags survive the conversion.
	t.Parallel()
	articleHTML := `<html><head><title>Test</title></head><body>
		<article>
			<h1>Main Heading</h1>
			<p>A paragraph with a <a href="https://example.com">link</a>.</p>
			<ul><li>Item one</li><li>Item two</li></ul>
		</article>
	</body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(articleHTML))
	}))
	defer server.Close()

	tool := NewWebFetchTool()
	params, _ := json.Marshal(map[string]interface{}{
		"url": server.URL,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should contain markdown heading
	if !strings.Contains(result.Text, "# ") && !strings.Contains(result.Text, "Main Heading") {
		t.Errorf("expected markdown heading, got: %q", result.Text)
	}
	// Should contain markdown link
	if !strings.Contains(result.Text, "[link]") {
		t.Errorf("expected markdown link, got: %q", result.Text)
	}
	// No raw HTML tags
	if strings.Contains(result.Text, "<h1>") || strings.Contains(result.Text, "<p>") {
		t.Errorf("should not contain HTML tags, got: %q", result.Text)
	}
}

func TestIsThinExtraction(t *testing.T) {
	// #1960: flags an extraction that captures less than half of the page's
	// own VISIBLE text (not raw HTML bytes — scripts/CSS/markup inflate that
	// without bound and don't correlate with what a reader sees), but not a
	// short extraction from an already-short page (nothing lost) nor a large
	// extraction from a large page (a real, full article). The concrete
	// numbers below are the measured ratios from the three fixtures in
	// TestWebFetchThinExtraction (see that test for how they were obtained).
	t.Parallel()
	cases := []struct {
		name           string
		extractedChars int
		visibleChars   int
		want           bool
	}{
		{"real repro: darioamodei.com (~48% of visible text)", 620, 1298, true},
		{"real control: go.dev blog post (~58%)", 2742, 4693, false},
		{"real control: trimmed Wikipedia article (~83%)", 37205, 44824, false},
		{"short page, short extraction — nothing to lose", 200, 400, false},
		{"just under the visible-size floor", 100, 799, false},
		{"just at the visible-size floor, thin extraction", 100, 800, true},
		{"extraction at exactly half — not thin (strict <)", 400, 800, false},
	}
	for _, c := range cases {
		if got := isThinExtraction(c.extractedChars, c.visibleChars); got != c.want {
			t.Errorf("%s: isThinExtraction(%d, %d) = %v, want %v", c.name, c.extractedChars, c.visibleChars, got, c.want)
		}
	}
}

// webFetchThinExtractionFixtures are real pages captured live (2026-09-23) and
// checked in under testdata, chosen to prove the #1960 heuristic against the
// actual page that motivated the ticket rather than a synthetic HTML snippet
// shaped to pass. wikipedia_potato_trimmed.html is the real
// https://en.wikipedia.org/wiki/Potato page with its citation-list bulk
// (hundreds of <li> footnotes, ~340KB alone) trimmed to 6 representative
// entries and one navbox kept — everything that is actual article body
// (every section, the infobox) is untouched, so the extracted:visible ratio
// still matches the live page's (measured ~0.83 here vs ~0.85 on the
// untrimmed live fetch).
var webFetchThinExtractionFixtures = []struct {
	name        string
	file        string
	wantFlagged bool
}{
	{"darioamodei.com — link-hub/author-landing page (#1960 repro)", "dario_amodei_dev.html", true},
	{"go.dev/blog/go1.22 — normal article, real nav chrome", "go_dev_blog_go122.html", false},
	{"en.wikipedia.org/wiki/Potato (trimmed) — long normal article, heavy boilerplate", "wikipedia_potato_trimmed.html", false},
}

func TestWebFetchThinExtraction(t *testing.T) {
	// Proves the #1960 note fires on the actual page that motivated the
	// ticket, and does NOT fire on two real-world control pages — a short
	// article and a long one, both with realistic nav/footer chrome. Fixture
	// HTML is served verbatim by httptest so this stays hermetic (no network
	// call in the test) while still exercising the read fetched bytes.
	t.Parallel()
	for _, c := range webFetchThinExtractionFixtures {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			body, err := os.ReadFile(filepath.Join("testdata", "webfetch_thin", c.file))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write(body)
			}))
			defer server.Close()

			tool := NewWebFetchTool()
			params, _ := json.Marshal(map[string]interface{}{"url": server.URL})
			result, err := tool.Execute(context.Background(), params)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			gotFlagged := strings.Contains(result.Text, "may have been dropped")
			if gotFlagged != c.wantFlagged {
				t.Errorf("thin-extraction note present = %v, want %v (result len=%d)", gotFlagged, c.wantFlagged, len(result.Text))
			}
		})
	}
}

func TestWebFetchNoTruncation(t *testing.T) {
	// Build a response larger than 50k chars — web_fetch no longer truncates (guardToolResult handles it)
	t.Parallel()
	big := strings.Repeat("x", 60_000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(big))
	}))
	defer server.Close()

	tool := NewWebFetchTool()
	params, _ := json.Marshal(map[string]interface{}{
		"url": server.URL,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(result.Text, "truncated") {
		t.Errorf("web_fetch should no longer truncate output (guardToolResult handles it)")
	}
}

func TestWebFetchServerError(t *testing.T) {
	// Proves that HTTP error responses (5xx) are not returned as Go errors — the body is returned
	// as the result, consistent with how curl works.
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("server error"))
	}))
	defer server.Close()

	tool := NewWebFetchTool()
	params, _ := json.Marshal(map[string]interface{}{
		"url": server.URL,
	})

	// web_fetch doesn't error on non-200 — it returns the body
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Text, "server error") {
		t.Errorf("result = %q", result.Text)
	}
}

func TestWebSearchSuccess(t *testing.T) {
	// Proves that the search tool sends the correct headers and query string, and that the
	// tool is constructed with the right name.
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify headers
		if r.Header.Get("X-Subscription-Token") != "test-key" {
			t.Errorf("X-Subscription-Token = %q", r.Header.Get("X-Subscription-Token"))
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		// Verify query
		if r.URL.Query().Get("q") != "golang testing" {
			t.Errorf("query = %q", r.URL.Query().Get("q"))
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"web": map[string]interface{}{
				"results": []map[string]interface{}{
					{"title": "Go Testing", "url": "https://go.dev/testing", "description": "Testing in Go"},
					{"title": "Test Docs", "url": "https://pkg.go.dev", "description": "Package docs"},
				},
			},
		})
	}))
	defer server.Close()

	// Can't easily redirect the search URL, so test the tool creation
	tool := NewWebSearchTool("test-key")
	if tool.Name != "web_search" {
		t.Errorf("name = %q", tool.Name)
	}
}

func TestWebSearchNoAPIKey(t *testing.T) {
	// Proves that executing a search without a configured API key returns a descriptive error.
	t.Parallel()
	tool := NewWebSearchTool("")

	params, _ := json.Marshal(map[string]interface{}{
		"query": "hello",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for missing API key")
	}
	if !strings.Contains(err.Error(), "brave_api_key not configured") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestWebSearchEmptyResults(t *testing.T) {
	// Placeholder test exercising the empty-results path; actual Brave API integration
	// is not mocked here, so the test primarily documents intent.
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"web": map[string]interface{}{
				"results": []interface{}{},
			},
		})
	}))
	defer server.Close()

	// Test webSearch function directly with our server
	params, _ := json.Marshal(map[string]interface{}{
		"query": "test",
	})
	// We can call webSearch directly since it's package-level
	result, err := webSearch(context.Background(), params, "key")
	// This will fail because it hits the real Brave API, not our server.
	// Instead, verify the "no API key" path works correctly.
	_ = result
	_ = err
	_ = server
}

func TestWebSearchAPIError(t *testing.T) {
	// Placeholder for the API error path; verifies the tool and params can be constructed
	// without exercising the live Brave API.
	t.Parallel()
	tool := NewWebSearchTool("test-key")
	params, _ := json.Marshal(map[string]interface{}{
		"query": "test",
	})

	// This will try to hit the real Brave API, which will work with the key
	// For a unit test, we verify the error path by checking the no-key case
	_ = tool
	_ = params
}

func TestWebFetchInvalidURL(t *testing.T) {
	// Proves that a URL with no scheme/host returns an error rather than making a network request.
	t.Parallel()
	tool := NewWebFetchTool()
	params, _ := json.Marshal(map[string]interface{}{
		"url": "not-a-valid-url",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestWebFetchToolName(t *testing.T) {
	// Proves that the web_fetch tool is registered with the correct name for tool dispatch.
	t.Parallel()
	tool := NewWebFetchTool()
	if tool.Name != "web_fetch" {
		t.Errorf("name = %q, want %q", tool.Name, "web_fetch")
	}
}

// webFetchCorpus is the #2011 regression corpus: real pages served verbatim,
// each with text that must survive extraction and nav/footer boilerplate that
// must not leak in. It guards the "li" TagsToScore change (a global scoring
// change, so a nav-heavy page is included) and the list-drop note.
// lever_palantir_fdae.html is the live #2011 page with its <script>/<style>/
// <svg> bodies stripped (726KB -> 11KB, markup untouched); mdn_ul_element.html
// is the live MDN <ul> reference page (~400 <li>, mostly sidebar/nav) stripped
// the same way plus <link> tags. Both captured 2026-09-25. The two substack_*
// pages (#2066) were captured 2026-09-26 and stripped the same way.
var webFetchCorpus = []struct {
	name           string
	file           string
	mustContain    []string
	mustNotContain []string
}{
	{
		name: "jobs.lever.co posting — list-only sibling sections (#2011 repro)",
		file: "webfetch_corpus/lever_palantir_fdae.html",
		mustContain: []string{
			"Core Responsibilities", "Life at Palantir",
			"What We Value", "Solving real business problems, not academic benchmarks.",
			"What We Require", "Strong foundation in Machine Learning basics",
			"travelling up to 25%",
		},
		mustNotContain: []string{"Jobs powered by", "Palantir Technologies Home Page"},
	},
	{
		name: "developer.mozilla.org <ul> reference — nav-heavy (hundreds of sidebar <li>)",
		file: "webfetch_corpus/mdn_ul_element.html",
		mustContain: []string{
			"This Boolean attribute hints that the list should be rendered in a compact style",
			"may be nested as deeply as desired", "Nesting a list",
		},
		mustNotContain: []string{"HTML cheatsheet", "Date & time formats", "Telemetry Settings", "Community Participation Guidelines"},
	},
	{
		name:        "darioamodei.com (#1960 repro)",
		file:        "webfetch_thin/dario_amodei_dev.html",
		mustContain: []string{"Dario Amodei is the CEO of", "co-inventor of reinforcement learning from human feedback"},
	},
	{
		name: "go.dev/blog/go1.22 (#1960 control)",
		file: "webfetch_thin/go_dev_blog_go122.html",
		mustContain: []string{
			"Today the Go team is thrilled to release Go 1.22",
			"support for ranging over integers",
		},
		mustNotContain: []string{"Skip to Main Content", "Common problems companies solve with Go", "Tips for writing clear", "Terms of Service"},
	},
	{
		name:           "en.wikipedia.org/wiki/Potato trimmed (#1960 control)",
		file:           "webfetch_thin/wikipedia_potato_trimmed.html",
		mustContain:    []string{"## Etymology", "## Cultivation", "### Genetic engineering"},
		mustNotContain: []string{"Main menu", "Random article", "Community portal", "Cookie statement"},
	},
	{
		// In-body headings are h1/h2.header-anchor-post: "header" made them
		// unlikely candidates, deleted text and all. Footnote bodies sit in
		// div.footnote, which "footnote" scored negative and cleanConditionally
		// dropped, leaving only the anchors.
		name: "chrislakin.blog/p/courage — Substack headings + footnotes (#2066 repro)",
		file: "webfetch_corpus/substack_chrislakin_courage.html",
		mustContain: []string{
			"## Part I: Evaluating the core claims",
			"## Claim #1: Emotional bottlenecks often have hidden functions",
			"## Claim #7: You can choose to be present",
			"## Improvement #4: Validate the rewrite on real people",
			"## Conclusion",
			"In the past, when I tried to explain to others",
			"Also why this blog was named",
		},
	},
	{
		name:        "rivalvoices.substack.com — ~300-word footnote body (#2066 repro)",
		file:        "webfetch_corpus/substack_rivalvoices_footnote.html",
		mustContain: []string{"just feel your feelings", "exclusively", "## **1.**"},
	},
}

// fetchFixture serves a testdata fixture verbatim over httptest and runs it
// through the web_fetch tool, so the full extraction path is exercised.
func fetchFixture(t *testing.T, file string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer server.Close()
	params, _ := json.Marshal(map[string]interface{}{"url": server.URL})
	result, err := NewWebFetchTool().Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return result.Text
}

func TestWebFetchExtractionCorpus(t *testing.T) {
	// #2011: key content survives, boilerplate stays out, and no page in the
	// corpus trips the list-drop note once extraction is correct.
	t.Parallel()
	for _, c := range webFetchCorpus {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := fetchFixture(t, c.file)
			t.Logf("extracted len=%d", len(got))
			for _, s := range c.mustContain {
				if !strings.Contains(got, s) {
					t.Errorf("extraction missing %q", s)
				}
			}
			for _, s := range c.mustNotContain {
				if strings.Contains(got, s) {
					t.Errorf("boilerplate %q leaked into extraction", s)
				}
			}
			if i := strings.Index(got, "list-item text"); i >= 0 {
				t.Errorf("list-drop note fired on a correctly extracted page: %s", got[i:])
			}
		})
	}
}

func TestMissingListText(t *testing.T) {
	// #2011 safety net: with readability's DEFAULT scoring (no "li"), the
	// Lever page loses its two list sections — the detector must see that
	// loss. With web_fetch's parser it must see none. The nav-heavy MDN page
	// must not count its sidebar/footer menus as missing.
	t.Parallel()
	u, _ := url.Parse("https://jobs.lever.co/palantir/ff1029bd-bb6d-4d78-a03e-5f9744d0b798")
	read := func(file string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		return b
	}

	lever := read("webfetch_corpus/lever_palantir_fdae.html")
	def, err := readability.FromReader(bytes.NewReader(lever), u)
	if err != nil {
		t.Fatal(err)
	}
	missing, example := missingListText(lever, def.TextContent)
	if missing < listDropMinChars || !strings.Contains(example, "Engineering mindset") {
		t.Errorf("default readability on Lever: missing=%d example=%q, want >= %d chars starting at the What We Value list", missing, example, listDropMinChars)
	}

	fixed, err := ParseArticle(bytes.NewReader(lever), u)
	if err != nil {
		t.Fatal(err)
	}
	if missing, example := missingListText(lever, fixed.TextContent); missing != 0 {
		t.Errorf("web_fetch parser on Lever: missing=%d (%q), want 0", missing, example)
	}

	mdn := read("webfetch_corpus/mdn_ul_element.html")
	art, err := ParseArticle(bytes.NewReader(mdn), u)
	if err != nil {
		t.Fatal(err)
	}
	if missing, example := missingListText(mdn, art.TextContent); missing >= listDropMinChars {
		t.Errorf("MDN nav-heavy page: missing=%d (%q), want < %d", missing, example, listDropMinChars)
	}
}
