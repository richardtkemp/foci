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
		Description: "Fetch a URL and return its content as clean Markdown (article extracted via readability). Prefer the default Markdown mode; only set raw=true when you specifically need HTML.",
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

func webFetch(ctx context.Context, params json.RawMessage) (ToolResult, error) {
	var p struct {
		URL string `json:"url"`
		Raw bool   `json:"raw"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ToolResult{}, fmt.Errorf("parse params: %w", err)
	}

	parsed, err := url.Parse(p.URL)
	if err == nil {
		log.Debugf("web_fetch", "session=%s fetch url=%s raw=%v", SessionKeyFromContext(ctx), parsed.Hostname(), p.Raw)
	}

	client := &http.Client{Timeout: 30 * time.Second}
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

	// Try readability extraction, then convert to markdown
	var htmlContent string
	article, err := readability.FromReader(bytes.NewReader(body), parsed)
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
	var p struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ToolResult{}, fmt.Errorf("parse params: %w", err)
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
