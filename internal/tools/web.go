package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	readability "github.com/go-shiori/go-readability"

	"foci/internal/log"
)

func NewWebFetchTool() *Tool {
	return &Tool{
		Name:        "web_fetch",
		ExecExport:  true,
		Positional:  []string{"url"},
		Description: "Fetch a URL and return its content as clean Markdown (article extracted via readability).",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"url": {
					"type": "string",
					"description": "URL to fetch"
				},
				"raw": {
					"type": "boolean",
					"description": "Return raw HTML instead of Markdown. Not recommended — only use when you specifically need HTML (default false)"
				}
			},
			"required": ["url"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return webFetch(ctx, params)
		},
	}
}

func NewWebSearchTool(braveAPIKey string) *Tool {
	return &Tool{
		Name:        "web_search",
		ExecExport:  true,
		Positional:  []string{"query"},
		Description: "Search the web using Brave Search API. Returns titles, URLs, and descriptions.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "Search query"
				}
			},
			"required": ["query"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return webSearch(ctx, params, braveAPIKey)
		},
	}
}

// readabilityFromReader is the readability entry point, indirected through a
// package var so tests can substitute a slow/blocking parse.
var readabilityFromReader = readability.FromReader

// parseReadableWithTimeout runs readability extraction under a wall-clock
// timeout. readability/html.Parse is not context-cancellable, so on timeout the
// background goroutine is left to finish (and exit) on its own via the buffered
// channel — bounded in practice because the network body is capped at 1 MiB and
// the x/net bump removes the known infinite-loop inputs.
func parseReadableWithTimeout(body []byte, parsed *url.URL, timeout time.Duration) (readability.Article, error) {
	type result struct {
		article readability.Article
		err     error
	}
	ch := make(chan result, 1)
	// Capture the parse func in the caller goroutine before spawning, so the
	// background goroutine never reads the readabilityFromReader package var —
	// otherwise a test that swaps that var would race the goroutine's read.
	parse := readabilityFromReader
	go func() {
		a, err := parse(bytes.NewReader(body), parsed)
		ch <- result{a, err}
	}()
	select {
	case r := <-ch:
		return r.article, r.err
	case <-time.After(timeout):
		return readability.Article{}, fmt.Errorf("readability parse exceeded %s", timeout)
	}
}

func webFetch(ctx context.Context, params json.RawMessage) (ToolResult, error) {
	p, err := UnmarshalParams[struct {
		URL string `json:"url"`
		Raw bool   `json:"raw"`
	}](params)
	if err != nil {
		return ToolResult{}, err
	}

	parsed, err := url.Parse(p.URL)
	if err == nil {
		log.Debugf("web_fetch", "session=%s fetch url=%s raw=%v", SessionKeyFromContext(ctx), parsed.Hostname(), p.Raw)
	}

	// Use the shared SSRF-safe client: web_fetch is the default builtin and is
	// reachable both by any agent and by untrusted fetched content (prompt
	// injection), so it must validate the resolved IP and re-check redirects in
	// every mode. (P1-3.)
	client := newSafeClient(defaultFetchTimeout, defaultMaxRedirects)
	req, err := http.NewRequestWithContext(ctx, "GET", p.URL, nil)
	if err != nil {
		return ToolResult{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "Foci/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return ToolResult{}, fmt.Errorf("fetch URL: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return ToolResult{}, fmt.Errorf("read response: %w", err)
	}

	// Raw mode: return unprocessed HTML
	if p.Raw {
		return TextResult(string(body)), nil
	}

	// Try readability extraction, then convert to markdown. The parse is
	// bounded by an independent wall-clock timeout (P2-1 defence-in-depth).
	var htmlContent string
	article, err := parseReadableWithTimeout(body, parsed, defaultFetchParseTimeout)
	if err == nil && strings.TrimSpace(article.Content) != "" {
		htmlContent = article.Content
	} else {
		// Fallback: convert full HTML body to markdown
		htmlContent = string(body)
	}

	md, err := htmltomarkdown.ConvertString(htmlContent)
	if err != nil {
		// Last resort: return raw text content from readability if available
		if article.TextContent != "" {
			md = article.TextContent
		} else {
			md = string(body)
		}
	}

	return TextResult(md), nil
}

func webSearch(ctx context.Context, params json.RawMessage, apiKey string) (ToolResult, error) {
	p, err := UnmarshalParams[struct {
		Query string `json:"query"`
	}](params)
	if err != nil {
		return ToolResult{}, err
	}

	if apiKey == "" {
		return ToolResult{}, fmt.Errorf("brave_api_key not configured")
	}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.search.brave.com/res/v1/web/search", nil)
	if err != nil {
		return ToolResult{}, fmt.Errorf("create request: %w", err)
	}

	q := req.URL.Query()
	q.Set("q", p.Query)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("X-Subscription-Token", apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return ToolResult{}, fmt.Errorf("search request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ToolResult{}, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return ToolResult{}, fmt.Errorf("search API error (status %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return ToolResult{}, fmt.Errorf("parse search results: %w", err)
	}

	log.Debugf("web_search", "session=%s search query=%q results=%d", SessionKeyFromContext(ctx), p.Query, len(result.Web.Results))

	var out strings.Builder
	for i, r := range result.Web.Results {
		fmt.Fprintf(&out, "%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Description)
	}

	if out.Len() == 0 {
		return TextResult("No results found."), nil
	}
	return TextResult(out.String()), nil
}
