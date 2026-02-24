package tools

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
// tempDir is used for auto-saving binary responses; if empty, os.TempDir() is used.
func NewHTTPRequestTool(store *secrets.Store, bwStore *bitwarden.Store, tempDir string) *Tool {
	return &Tool{
		Name:        "http_request",
		Description: "Make an HTTP request. Secrets referenced via {{secret:NAME}} in headers/body are resolved server-side and validated against allowed_hosts. Preferred over exec for API calls with secrets. Binary responses are auto-saved to files. Use save_to to save any response to a specific path.",
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
				},
				"save_to": {
					"type": "string",
					"description": "Save response body to this file path instead of returning it. Returns status and headers only. If save_from_json_path is also set, extracts and decodes that field from JSON response before saving."
				},
				"save_from_json_path": {
					"type": "string",
					"description": "Dot-separated JSON path to extract from response (e.g. 'data.0.url'). If the extracted value is a data: URI (data:image/png;base64,...), it is decoded to binary. Requires save_to."
				},
				"timeout": {
					"type": "integer",
					"description": "Request timeout in seconds (default 30, max 300)"
				}
			},
			"required": ["url"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return executeHTTPRequest(ctx, params, store, bwStore, tempDir)
		},
	}
}

func executeHTTPRequest(ctx context.Context, params json.RawMessage, store *secrets.Store, bwStore *bitwarden.Store, tempDir string) (string, error) {
	var p struct {
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
		Query   map[string]string `json:"query"`
		SaveTo           string            `json:"save_to"`
		SaveFromJSONPath string            `json:"save_from_json_path"`
		Timeout          int              `json:"timeout"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("parse params: %w", err)
	}

	if p.Method == "" {
		p.Method = "GET"
	}
	if p.SaveFromJSONPath != "" && p.SaveTo == "" {
		return "", fmt.Errorf("save_from_json_path requires save_to")
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

	timeout := 30 * time.Second
	if p.Timeout > 0 {
		timeout = time.Duration(p.Timeout) * time.Second
		if timeout > 300*time.Second {
			timeout = 300 * time.Second
		}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
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
	client := &http.Client{Timeout: timeout}
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

	if parsed, err := url.Parse(p.URL); err == nil {
		log.Debugf("http_request", "response %s %s status=%d body=%d", p.Method, parsed.Hostname(), resp.StatusCode, len(body))
	}

	contentType := resp.Header.Get("Content-Type")

	// Determine if we need to save to file
	savePath := p.SaveTo
	if savePath == "" && isBinaryContentType(contentType) {
		// Auto-save binary responses to temp file
		dir := tempDir
		if dir == "" {
			dir = os.TempDir()
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("create temp dir: %w", err)
		}
		ext := extensionForContentType(contentType)
		var randBytes [4]byte
		rand.Read(randBytes[:])
		savePath = filepath.Join(dir, "http-"+hex.EncodeToString(randBytes[:])+ext)
	}

	// Format response header block
	formatHeaders := func() string {
		var hdr strings.Builder
		fmt.Fprintf(&hdr, "HTTP %s\n", resp.Status)
		for _, h := range []string{"Content-Type", "Content-Length", "Location", "X-Request-Id"} {
			if v := resp.Header.Get(h); v != "" {
				fmt.Fprintf(&hdr, "%s: %s\n", h, v)
			}
		}
		return hdr.String()
	}

	if savePath != "" {
		saveData := body

		// Extract from JSON response if save_from_json_path is set
		if p.SaveFromJSONPath != "" {
			extracted, err := extractJSONPath(body, p.SaveFromJSONPath)
			if err != nil {
				return "", fmt.Errorf("extract %s from JSON: %w", p.SaveFromJSONPath, err)
			}
			// If it's a data: URI, decode it
			if decoded, err := decodeDataURI(extracted); err == nil {
				saveData = decoded
			} else {
				// Not a data URI — save the extracted string as-is
				saveData = []byte(extracted)
			}
		}

		if err := os.MkdirAll(filepath.Dir(savePath), 0755); err != nil {
			return "", fmt.Errorf("create parent dirs for save_to: %w", err)
		}
		if err := os.WriteFile(savePath, saveData, 0644); err != nil {
			return "", fmt.Errorf("write response to %s: %w", savePath, err)
		}
		log.Debugf("http_request", "saved %d bytes to %s", len(saveData), savePath)
		return fmt.Sprintf("%s\nSaved %d bytes to %s", formatHeaders(), len(saveData), savePath), nil
	}

	// Text response — return inline
	bodyStr := string(body)

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

	return formatHeaders() + "\n" + bodyStr, nil
}

// isBinaryContentType returns true for content types that are binary (not text).
func isBinaryContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	// Extract MIME type before any parameters (charset, boundary, etc.)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "" {
		return false
	}
	// Explicit text types
	if strings.HasPrefix(ct, "text/") {
		return false
	}
	// Common text-like application types
	textTypes := []string{
		"application/json",
		"application/xml",
		"application/javascript",
		"application/x-www-form-urlencoded",
		"application/graphql",
		"application/ld+json",
		"application/xhtml+xml",
		"application/atom+xml",
		"application/rss+xml",
		"application/soap+xml",
		"application/yaml",
		"application/toml",
	}
	for _, t := range textTypes {
		if ct == t {
			return false
		}
	}
	// Treat anything with +json or +xml suffix as text
	if strings.HasSuffix(ct, "+json") || strings.HasSuffix(ct, "+xml") {
		return false
	}
	// Everything else under image/, audio/, video/ is binary
	if strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "audio/") ||
		strings.HasPrefix(ct, "video/") || strings.HasPrefix(ct, "application/octet-stream") ||
		strings.HasPrefix(ct, "application/pdf") || strings.HasPrefix(ct, "application/zip") {
		return true
	}
	// Unknown application/* types — treat as binary to be safe
	if strings.HasPrefix(ct, "application/") {
		return true
	}
	return false
}

// extensionForContentType returns a file extension for common content types.
func extensionForContentType(ct string) string {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch ct {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml":
		return ".svg"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav":
		return ".wav"
	case "audio/ogg":
		return ".ogg"
	case "video/mp4":
		return ".mp4"
	case "application/pdf":
		return ".pdf"
	case "application/zip":
		return ".zip"
	case "application/octet-stream":
		return ".bin"
	default:
		return ".bin"
	}
}

// extractJSONPath extracts a string value from JSON using a dot-separated path.
// Array indices are supported (e.g. "data.0.url"). Returns the raw string value.
func extractJSONPath(data []byte, path string) (string, error) {
	var root interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		return "", fmt.Errorf("parse JSON: %w", err)
	}

	parts := strings.Split(path, ".")
	current := root
	for _, part := range parts {
		switch v := current.(type) {
		case map[string]interface{}:
			val, ok := v[part]
			if !ok {
				return "", fmt.Errorf("key %q not found", part)
			}
			current = val
		case []interface{}:
			idx, err := strconv.Atoi(part)
			if err != nil {
				return "", fmt.Errorf("expected array index, got %q", part)
			}
			if idx < 0 || idx >= len(v) {
				return "", fmt.Errorf("array index %d out of range (len %d)", idx, len(v))
			}
			current = v[idx]
		default:
			return "", fmt.Errorf("cannot index into %T at %q", current, part)
		}
	}

	switch v := current.(type) {
	case string:
		return v, nil
	default:
		// For non-string values, marshal back to JSON
		b, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("marshal extracted value: %w", err)
		}
		return string(b), nil
	}
}

// decodeDataURI decodes a data: URI (e.g. "data:image/png;base64,iVBOR...")
// into raw bytes. Returns an error if the string is not a valid data URI.
func decodeDataURI(s string) ([]byte, error) {
	if !strings.HasPrefix(s, "data:") {
		return nil, fmt.Errorf("not a data URI")
	}
	// Format: data:[<mediatype>][;base64],<data>
	commaIdx := strings.IndexByte(s, ',')
	if commaIdx < 0 {
		return nil, fmt.Errorf("malformed data URI: no comma")
	}
	meta := s[5:commaIdx] // between "data:" and ","
	payload := s[commaIdx+1:]

	if strings.HasSuffix(meta, ";base64") {
		return base64.StdEncoding.DecodeString(payload)
	}
	// Non-base64 data URI — return raw bytes
	return []byte(payload), nil
}
