package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"foci/log"
	"foci/secrets"
	"foci/secrets/bitwarden"
)

type fileAttachment struct {
	FieldName string `json:"field_name"`
	FilePath  string `json:"file_path"`
	Filename  string `json:"filename"`
}

// NewHTTPRequestTool creates an http_request tool with domain-locked secret resolution.
// Secrets referenced via {{secret:NAME}} are resolved server-side and validated
// against per-secret allowed_hosts before the request is sent. If store is nil,
// requests without secrets work normally but secret templates will fail.
// tempDir is used for auto-saving binary responses; if empty, os.TempDir() is used.
// autoBackgroundSecs is the threshold after which a running request is auto-backgrounded
// (0 disables). notifier delivers results when an auto-backgrounded request finishes.
// maxUploadFileSize is the max file size in bytes for multipart uploads (0 = 50MB default).
func NewHTTPRequestTool(store *secrets.Store, bwStore *bitwarden.Store, tempDir string, autoBackgroundSecs int, maxUploadFileSize int64, notifier *AsyncNotifier) *Tool {
	return &Tool{
		Name:        "http_request",
		ExecExport:  true,
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
					"description": "Request body. Use {{secret:NAME}} for credentials. Mutually exclusive with body_file and files."
				},
				"body_file": {
					"type": "string",
					"description": "Read request body from this file path instead of inline body. Supports {{secret:NAME}} in file contents. Mutually exclusive with body and files. Use for large payloads that are impractical as inline strings."
				},
				"files": {
					"type": "array",
					"description": "File attachments for multipart/form-data upload. When files are present, the request is sent as multipart/form-data. Mutually exclusive with body.",
					"items": {
						"type": "object",
						"properties": {
							"field_name": {
								"type": "string",
								"description": "Form field name (e.g. 'document', 'photo', 'file')"
							},
							"file_path": {
								"type": "string",
								"description": "Path to the file to upload"
							},
							"filename": {
								"type": "string",
								"description": "Override filename sent in the multipart header (optional, defaults to basename of file_path)"
							}
						},
						"required": ["field_name", "file_path"]
					}
				},
				"form_fields": {
					"type": "object",
					"description": "Additional form fields for multipart/form-data requests. Values support {{secret:NAME}} templates. Requires files.",
					"additionalProperties": { "type": "string" }
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
				},
				"max_response_bytes": {
					"type": "integer",
					"description": "Max response body size in bytes. Default 1MB for text, 10MB when save_to is set. Overrides both."
				},
				"background": {
					"type": "boolean",
					"description": "If true, run the request in the background immediately and deliver the result asynchronously."
				}
			},
			"required": ["url"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return executeHTTPRequest(ctx, params, store, bwStore, tempDir, autoBackgroundSecs, maxUploadFileSize, notifier)
		},
	}
}

func executeHTTPRequest(ctx context.Context, params json.RawMessage, store *secrets.Store, bwStore *bitwarden.Store, tempDir string, autoBackgroundSecs int, maxUploadFileSize int64, notifier *AsyncNotifier) (string, error) {
	var p struct {
		URL        string            `json:"url"`
		Method     string            `json:"method"`
		Headers    map[string]string `json:"headers"`
		Body       string            `json:"body"`
		BodyFile   string            `json:"body_file"`
		Files      []fileAttachment  `json:"files"`
		FormFields map[string]string `json:"form_fields"`
		Query      map[string]string `json:"query"`
		SaveTo           string            `json:"save_to"`
		SaveFromJSONPath string            `json:"save_from_json_path"`
		Timeout          int              `json:"timeout"`
		MaxResponseBytes int64            `json:"max_response_bytes"`
		Background       bool             `json:"background"`
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
	// Mutual exclusivity: body, body_file, files
	bodySourceCount := 0
	if p.Body != "" {
		bodySourceCount++
	}
	if p.BodyFile != "" {
		bodySourceCount++
	}
	if len(p.Files) > 0 {
		bodySourceCount++
	}
	if bodySourceCount > 1 {
		return "", fmt.Errorf("body, body_file, and files are mutually exclusive")
	}
	if len(p.FormFields) > 0 && len(p.Files) == 0 {
		return "", fmt.Errorf("form_fields requires files")
	}

	// Read body_file contents early so secrets can be scanned
	if p.BodyFile != "" {
		info, err := os.Stat(p.BodyFile)
		if err != nil {
			return "", fmt.Errorf("body_file %q: %w", p.BodyFile, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("body_file %q is a directory", p.BodyFile)
		}
		if info.Size() > maxUploadFileSize {
			return "", fmt.Errorf("body_file %q is %d bytes, exceeds %dMB limit", p.BodyFile, info.Size(), maxUploadFileSize/(1024*1024))
		}
		data, err := os.ReadFile(p.BodyFile)
		if err != nil {
			return "", fmt.Errorf("read body_file %q: %w", p.BodyFile, err)
		}
		p.Body = string(data)
	}

	// Collect all secret refs from headers, body, and form_fields
	var allText strings.Builder
	for _, v := range p.Headers {
		allText.WriteString(v)
		allText.WriteByte(' ')
	}
	allText.WriteString(p.Body)
	for _, v := range p.FormFields {
		allText.WriteByte(' ')
		allText.WriteString(v)
	}

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

	// Resolve secrets in form_fields values
	resolvedFormFields := make(map[string]string, len(p.FormFields))
	for k, v := range p.FormFields {
		if store != nil && len(regularRefs) > 0 {
			resolved, err := store.Resolve(v)
			if err != nil {
				return "", fmt.Errorf("resolve form_field %q: %w", k, err)
			}
			v = resolved
		}
		if bwStore != nil && hasBWSecrets {
			resolved, err := bwStore.Resolve(v)
			if err != nil {
				return "", fmt.Errorf("resolve bw form_field %q: %w", k, err)
			}
			v = resolved
		}
		resolvedFormFields[k] = v
	}

	// Build request body
	var bodyReader io.Reader
	var multipartContentType string
	if len(p.Files) > 0 {
		buf, contentType, err := buildMultipartBody(p.Files, resolvedFormFields, maxUploadFileSize)
		if err != nil {
			return "", err
		}
		bodyReader = buf
		multipartContentType = contentType
	} else if p.Body != "" {
		bodyReader = strings.NewReader(p.Body)
	}

	timeout := 30 * time.Second
	if p.Timeout > 0 {
		timeout = time.Duration(p.Timeout) * time.Second
		if timeout > 300*time.Second {
			timeout = 300 * time.Second
		}
	}

	// Build request without timeout context — the timeout is applied later
	// either via context.WithTimeout (auto-background path) or directly on the client.
	req, err := http.NewRequestWithContext(ctx, p.Method, reqURL, bodyReader)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("User-Agent", "Foci/1.0")
	for k, v := range resolvedHeaders {
		req.Header.Set(k, v)
	}
	if multipartContentType != "" {
		req.Header.Set("Content-Type", multipartContentType)
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

	// doAndProcess performs the HTTP call and processes the response.
	// It uses its own context (may be detached from agent turn for auto-background).
	doAndProcess := func(reqCtx context.Context) (string, error) {
		reqWithCtx := req.WithContext(reqCtx)
		resp, err := client.Do(reqWithCtx)
		if err != nil {
			return "", fmt.Errorf("request failed: %w", err)
		}
		defer resp.Body.Close()

		return processHTTPResponse(resp, p.URL, p.Method, p.SaveTo, p.SaveFromJSONPath, p.MaxResponseBytes, tempDir, store, bwStore)
	}

	displayURL := p.URL
	if parsed, err := url.Parse(p.URL); err == nil {
		displayURL = p.Method + " " + parsed.Hostname() + parsed.Path
	}

	// Explicit background mode: fire immediately
	if p.Background && notifier != nil {
		sk := SessionKeyFromContext(ctx)
		bgCtx, bgCancel := context.WithTimeout(context.Background(), timeout)
		notifier.MarkPending(sk)
		go func() {
			defer bgCancel()
			defer notifier.MarkDone(sk)
			result, err := doAndProcess(bgCtx)
			var msg string
			if err != nil {
				msg = fmt.Sprintf("[HTTP RESULT] Request failed:\n%s\n\nError: %s", displayURL, err)
			} else {
				msg = fmt.Sprintf("[HTTP RESULT] Request completed:\n%s\n\n%s", displayURL, result)
			}
			notifier.Notify(sk, msg)
		}()
		return fmt.Sprintf("Request running in background. Results will be delivered when complete.\n%s", displayURL), nil
	}

	// Auto-background: start the request and wait with a timer
	if autoBackgroundSecs > 0 && notifier != nil {
		sk := SessionKeyFromContext(ctx)
		bgCtx, bgCancel := context.WithTimeout(context.Background(), timeout)

		type httpResult struct {
			output string
			err    error
		}
		done := make(chan httpResult, 1)
		go func() {
			out, err := doAndProcess(bgCtx)
			done <- httpResult{out, err}
		}()

		threshold := time.Duration(autoBackgroundSecs) * time.Second
		select {
		case r := <-done:
			bgCancel()
			if r.err != nil {
				return "", r.err
			}
			return r.output, nil

		case <-time.After(threshold):
			log.Infof("http_request", "auto-backgrounding after %v: %s", threshold, displayURL)
			notifier.MarkPending(sk)
			go func() {
				defer bgCancel()
				defer notifier.MarkDone(sk)
				r := <-done
				var msg string
				if r.err != nil {
					msg = fmt.Sprintf("[HTTP RESULT] Request failed:\n%s\n\nError: %s", displayURL, r.err)
				} else {
					msg = fmt.Sprintf("[HTTP RESULT] Request completed:\n%s\n\n%s", displayURL, r.output)
				}
				notifier.Notify(sk, msg)
			}()
			return fmt.Sprintf("Request still running (exceeded %ds threshold). Results will be delivered when complete.\n%s", autoBackgroundSecs, displayURL), nil

		case <-ctx.Done():
			// Agent turn cancelled — let the request continue in background
			go func() {
				defer bgCancel()
				<-done
			}()
			return "", ctx.Err()
		}
	}

	// No auto-background — run directly
	return doAndProcess(ctx)
}

// processHTTPResponse reads and formats an HTTP response.
func processHTTPResponse(resp *http.Response, reqURL, method, saveTo, saveFromJSONPath string, maxResponseBytes int64, tempDir string, store *secrets.Store, bwStore *bitwarden.Store) (string, error) {
	// Read response — 10MB when saving to file, 1MB when returning to context
	bodyLimit := int64(1024 * 1024)
	if saveTo != "" || isBinaryContentType(resp.Header.Get("Content-Type")) {
		bodyLimit = 10 * 1024 * 1024
	}
	if maxResponseBytes > 0 {
		bodyLimit = maxResponseBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if parsed, err := url.Parse(reqURL); err == nil {
		log.Debugf("http_request", "response %s %s status=%d body=%d", method, parsed.Hostname(), resp.StatusCode, len(body))
	}

	contentType := resp.Header.Get("Content-Type")

	// Determine if we need to save to file
	savePath := saveTo
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
		if saveFromJSONPath != "" {
			extracted, err := extractJSONPath(body, saveFromJSONPath)
			if err != nil {
				return "", fmt.Errorf("extract %s from JSON: %w", saveFromJSONPath, err)
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

// buildMultipartBody constructs a multipart/form-data body from file attachments
// and form fields. Returns the buffer, Content-Type with boundary, and any error.
func buildMultipartBody(files []fileAttachment, formFields map[string]string, maxFileSize int64) (*bytes.Buffer, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	// Write text form fields first
	for k, v := range formFields {
		if err := w.WriteField(k, v); err != nil {
			return nil, "", fmt.Errorf("write form field %q: %w", k, err)
		}
	}

	// Write file parts
	for _, f := range files {
		if f.FieldName == "" {
			return nil, "", fmt.Errorf("file attachment missing field_name")
		}
		if f.FilePath == "" {
			return nil, "", fmt.Errorf("file attachment missing file_path")
		}

		// Validate file exists and check size
		info, err := os.Stat(f.FilePath)
		if err != nil {
			return nil, "", fmt.Errorf("file %q: %w", f.FilePath, err)
		}
		if info.IsDir() {
			return nil, "", fmt.Errorf("file %q is a directory", f.FilePath)
		}
		if info.Size() > maxFileSize {
			return nil, "", fmt.Errorf("file %q is %d bytes, exceeds %dMB limit", f.FilePath, info.Size(), maxFileSize/(1024*1024))
		}

		filename := f.Filename
		if filename == "" {
			filename = filepath.Base(f.FilePath)
		}

		part, err := w.CreateFormFile(f.FieldName, filename)
		if err != nil {
			return nil, "", fmt.Errorf("create form file %q: %w", f.FieldName, err)
		}

		file, err := os.Open(f.FilePath)
		if err != nil {
			return nil, "", fmt.Errorf("open %q: %w", f.FilePath, err)
		}
		_, err = io.Copy(part, file)
		file.Close()
		if err != nil {
			return nil, "", fmt.Errorf("write file %q: %w", f.FilePath, err)
		}
	}

	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart writer: %w", err)
	}

	return &buf, w.FormDataContentType(), nil
}

// isPrivateIP checks whether a hostname resolves to a private/loopback/link-local
// address. Used to block SSRF in isolated spawn contexts.
func isPrivateIP(hostname string) bool {
	// Check well-known names first
	if hostname == "localhost" {
		return true
	}

	ips, err := net.LookupHost(hostname)
	if err != nil {
		return false // can't resolve — let the request fail naturally
	}

	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return true
		}
		// AWS/cloud metadata endpoint
		if ip.Equal(net.ParseIP("169.254.169.254")) {
			return true
		}
	}
	return false
}

// NewIsolatedHTTPRequestTool creates an http_request tool that blocks requests
// to private/loopback/link-local IP addresses. Used in spawn none-mode to prevent SSRF.
func NewIsolatedHTTPRequestTool(base *Tool) *Tool {
	return &Tool{
		Name:        base.Name,
		Description: base.Description,
		Parameters:  base.Parameters,
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var p struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return "", fmt.Errorf("parse input: %w", err)
			}

			parsed, err := url.Parse(p.URL)
			if err != nil {
				return "", fmt.Errorf("invalid URL: %w", err)
			}

			hostname := parsed.Hostname()
			if isPrivateIP(hostname) {
				return "", fmt.Errorf("requests to private/loopback addresses are blocked in isolated mode")
			}

			return base.Execute(ctx, input)
		},
	}
}
