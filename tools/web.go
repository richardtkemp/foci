package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

func NewWebFetchTool() *Tool {
	return &Tool{
		Name:        "web_fetch",
		Description: "Fetch a URL and return its text content (HTML tags stripped).",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"url": {
					"type": "string",
					"description": "URL to fetch"
				}
			},
			"required": ["url"]
		}`),
		Execute: webFetch,
	}
}

func NewWebSearchTool(braveAPIKey string) *Tool {
	return &Tool{
		Name:        "web_search",
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
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return webSearch(ctx, params, braveAPIKey)
		},
	}
}

var htmlTagRegex = regexp.MustCompile(`<[^>]*>`)

func webFetch(ctx context.Context, params json.RawMessage) (string, error) {
	var p struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("parse params: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", p.URL, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "Clod/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch URL: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	text := htmlTagRegex.ReplaceAllString(string(body), "")
	// Collapse whitespace
	text = regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")

	const maxLen = 50_000
	if len(text) > maxLen {
		text = text[:maxLen] + "\n... (truncated)"
	}

	return text, nil
}

func webSearch(ctx context.Context, params json.RawMessage, apiKey string) (string, error) {
	var p struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("parse params: %w", err)
	}

	if apiKey == "" {
		return "", fmt.Errorf("brave_api_key not configured")
	}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.search.brave.com/res/v1/web/search", nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	q := req.URL.Query()
	q.Set("q", p.Query)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("X-Subscription-Token", apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("search request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("search API error (status %d): %s", resp.StatusCode, string(body))
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
		return "", fmt.Errorf("parse search results: %w", err)
	}

	var out strings.Builder
	for i, r := range result.Web.Results {
		fmt.Fprintf(&out, "%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Description)
	}

	if out.Len() == 0 {
		return "No results found.", nil
	}
	return out.String(), nil
}
