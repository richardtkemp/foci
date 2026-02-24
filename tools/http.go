package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"clod/log"
	"clod/secrets"
	"clod/secrets/bitwarden"
)

// NewHTTPRequestTool creates an http_request tool with domain-locked secret resolution.
// Secrets referenced via {{secret:NAME}} are resolved server-side and validated
// against per-secret allowed_hosts before the request is sent. If store is nil,
// requests without secrets work normally but secret templates will fail.
func NewHTTPRequestTool(store *secrets.Store, bwStore *bitwarden.Store) *Tool {
	return &Tool{
		Name:        "http_request",
		Description: "Make an HTTP request. Secrets referenced via {{secret:NAME}} in headers/body are resolved server-side and validated against allowed_hosts. Preferred over exec for API calls with secrets.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"url": {
					"type": "string",
					"description": "Request URL"
				},
				"method": {
					"type": "string",
					"description": "HTTP method (default GET)",
					"enum": ["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"]
				},
				"headers": {
					"type": "object",
					"description": "Request headers as key-value pairs. Use {{secret:NAME}} for credentials.",
					"additionalProperties": { "type": "string" }
				},
				"body": {
					"type": "string",
					"description": "Request body. Use {{secret:NAME}} for credentials."
				},
				"query": {
					"type": "object",
					"description": "Query parameters as key-value pairs",
					"additionalProperties": { "type": "string" }
				}
			},
			"required": ["url"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return executeHTTPRequest(ctx, params, store, bwStore)
		},
	}
}

func executeHTTPRequest(ctx context.Context, params json.RawMessage, store *secrets.Store, bwStore *bitwarden.Store) (string, error) {
	var p struct {
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
		Query   map[string]string `json:"query"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("parse params: %w", err)
	}

	if p.Method == "" {
		p.Method = "GET"
	}

	// Collect all secret refs from headers and body
	var allText strings.Builder
	for _, v := range p.Headers {
		allText.WriteString(v)
		allText.WriteByte(' ')
	}
	allText.WriteString(p.Body)

	secretRefs := secrets.FindSecretRefs(allText.String())
	hasSecrets := len(secretRefs) > 0

	// Split refs into regular (custom.key) and bitwarden (bw.UUID) groups
	var regularRefs, bwRefs []string
	for _, name := range secretRefs {
		if bitwarden.IsBitwardenRef(name) {
			bwRefs = append(bwRefs, name)
		} else {
			regularRefs = append(regularRefs, name)
		}
	}
	hasBWSecrets := len(bwRefs) > 0

	if parsed, err := url.Parse(p.URL); err == nil {
		log.Debugf("http_request", "request %s %s secrets=%d (bw=%d)", p.Method, parsed.Hostname(), len(secretRefs), len(bwRefs))
	}

	// Validate regular secrets against allowed_hosts
	if len(regularRefs) > 0 {
		if store == nil {
			return "", fmt.Errorf("secrets referenced but no secret store configured")
		}
		for _, name := range regularRefs {
			if err := store.CheckHostAllowed(name, p.URL); err != nil {
				return "", fmt.Errorf("secret host check: %w", err)
			}
		}
	}

	// Validate bitwarden secrets against vault item URIs
	if hasBWSecrets {
		if bwStore == nil {
			return "", fmt.Errorf("bitwarden secrets referenced but bitwarden is not configured")
		}
		for _, name := range bwRefs {
			id := bitwarden.ExtractID(name)
			if err := bwStore.CheckHostAllowed(id, p.URL); err != nil {
				return "", fmt.Errorf("bitwarden host check: %w", err)
			}
		}
	}

	// Resolve secret templates in headers and body
	resolvedHeaders := make(map[string]string, len(p.Headers))
	if hasSecrets {
		for k, v := range p.Headers {
			// Resolve regular secrets first
			if store != nil && len(regularRefs) > 0 {
				resolved, err := store.Resolve(v)
				if err != nil {
					return "", fmt.Errorf("resolve header %q: %w", k, err)
				}
				v = resolved
			}
			// Then resolve bitwarden secrets
			if bwStore != nil && hasBWSecrets {
				resolved, err := bwStore.Resolve(v)
				if err != nil {
					return "", fmt.Errorf("resolve bw header %q: %w", k, err)
				}
				v = resolved
			}
			resolvedHeaders[k] = v
		}
		if p.Body != "" {
			if store != nil && len(regularRefs) > 0 {
				resolved, err := store.Resolve(p.Body)
				if err != nil {
					return "", fmt.Errorf("resolve body: %w", err)
				}
				p.Body = resolved
			}
			if bwStore != nil && hasBWSecrets {
				resolved, err := bwStore.Resolve(p.Body)
				if err != nil {
					return "", fmt.Errorf("resolve bw body: %w", err)
				}
				p.Body = resolved
			}
		}
	} else {
		for k, v := range p.Headers {
			resolvedHeaders[k] = v
		}
	}

	// Build URL with query params
	reqURL := p.URL
	if len(p.Query) > 0 {
		parsed, err := url.Parse(reqURL)
		if err != nil {
			return "", fmt.Errorf("parse URL: %w", err)
		}
		q := parsed.Query()
		for k, v := range p.Query {
			q.Set(k, v)
		}
		parsed.RawQuery = q.Encode()
		reqURL = parsed.String()
	}

	// Build request
	var bodyReader io.Reader
	if p.Body != "" {
		bodyReader = strings.NewReader(p.Body)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, p.Method, reqURL, bodyReader)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("User-Agent", "Clod/1.0")
	for k, v := range resolvedHeaders {
		req.Header.Set(k, v)
	}

	// Build client — block cross-domain redirects when secrets are present
	client := &http.Client{Timeout: 30 * time.Second}
	if hasSecrets {
		originalHost := req.URL.Hostname()
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if !strings.EqualFold(req.URL.Hostname(), originalHost) {
				return fmt.Errorf("blocked cross-domain redirect from %q to %q (secrets present)", originalHost, req.URL.Hostname())
			}
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response (limit to 1MB)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	bodyStr := string(body)

	if parsed, err := url.Parse(p.URL); err == nil {
		log.Debugf("http_request", "response %s %s status=%d body=%d", p.Method, parsed.Hostname(), resp.StatusCode, len(bodyStr))
	}

	// Redact secrets from response
	if store != nil {
		bodyStr = store.Redact(bodyStr)
	}
	if bwStore != nil {
		bodyStr = bwStore.Redact(bodyStr)
	}

	// Truncate body to 50k chars
	const maxBodyLen = 50_000
	if len(bodyStr) > maxBodyLen {
		bodyStr = bodyStr[:maxBodyLen] + "\n... (truncated)"
	}

	// Format response with selected headers
	var result strings.Builder
	fmt.Fprintf(&result, "HTTP %s\n", resp.Status)
	for _, hdr := range []string{"Content-Type", "Location", "X-Request-Id"} {
		if v := resp.Header.Get(hdr); v != "" {
			fmt.Fprintf(&result, "%s: %s\n", hdr, v)
		}
	}
	result.WriteByte('\n')
	result.WriteString(bodyStr)

	return result.String(), nil
}
