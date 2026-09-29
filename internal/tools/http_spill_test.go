package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// bigBodyServer returns an httptest server that replies with `size` bytes of
// 'a' as text/plain.
func bigBodyServer(t *testing.T, size int) *httptest.Server {
	t.Helper()
	body := strings.Repeat("a", size)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}))
}

func TestHTTPRequestSpillsLargeBody(t *testing.T) {
	// Proves that a text response larger than the inline preview is NOT
	// discarded: the inline result carries a preview, ResultFile points at the
	// full body on disk, and ResultSize is the full length. (The fix for the
	// old LimitReader-discard behaviour.)
	// The preview is the 1MB text default: an explicit max_response_bytes is a
	// hard cap and never spills (#2098).
	t.Parallel()
	const preview = 1 << 20           // default inline text limit
	const ceiling = 4 << 20           // http_max_spill_bytes
	const bodySize = preview + 5*1024 // just over preview, well under ceiling
	srv := bigBodyServer(t, bodySize)
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, t.TempDir(), func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return ceiling }, func() int64 { return 0 }, nil, 0640)
	params, _ := json.Marshal(map[string]any{"url": srv.URL})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.ResultFile == "" {
		t.Fatal("expected ResultFile to be set for an over-preview body")
	}
	if result.ResultSize != bodySize {
		t.Errorf("ResultSize = %d, want %d (full body length)", result.ResultSize, bodySize)
	}
	// Inline preview is bounded by the preview threshold (plus the small header block).
	if len(result.Text) > preview+512 {
		t.Errorf("inline text = %d bytes, want roughly <= preview (%d)", len(result.Text), preview)
	}
	// ...and actually carries the head of the body, not an empty string (#2102).
	if !strings.HasSuffix(result.Text, strings.Repeat("a", preview)) {
		t.Errorf("inline text = %d bytes, want it to end with the %d-byte body preview", len(result.Text), preview)
	}
	// Full body is recoverable from disk, intact.
	data, err := os.ReadFile(result.ResultFile)
	if err != nil {
		t.Fatalf("read spill file: %v", err)
	}
	if len(data) != bodySize {
		t.Errorf("spill file = %d bytes, want %d (full body)", len(data), bodySize)
	}
}

func TestHTTPRequestSmallBodyInline(t *testing.T) {
	// Proves the common case is unchanged: a response under the preview
	// threshold is returned fully inline with no spill file.
	t.Parallel()
	srv := bigBodyServer(t, 200)
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, t.TempDir(), func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 1 << 20 }, func() int64 { return 0 }, nil, 0640)
	params, _ := json.Marshal(map[string]any{"url": srv.URL})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ResultFile != "" {
		t.Errorf("small body should not spill, got ResultFile=%q", result.ResultFile)
	}
	if !strings.Contains(result.Text, strings.Repeat("a", 200)) {
		t.Error("small body should be fully inline")
	}
}

func TestHTTPRequestCeilingTruncates(t *testing.T) {
	// Proves the DoS ceiling bounds disk use: a body larger than
	// http_max_spill_bytes is capped on disk at the ceiling (not unbounded),
	// while ResultSize still reports the full source length seen.
	// The ceiling must exceed the 1MB default preview for anything to spill.
	t.Parallel()
	const ceiling = 2 << 20  // small ceiling, above the preview
	const bodySize = 3 << 20 // exceeds the ceiling
	srv := bigBodyServer(t, bodySize)
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, t.TempDir(), func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return ceiling }, func() int64 { return 0 }, nil, 0640)
	params, _ := json.Marshal(map[string]any{"url": srv.URL})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ResultFile == "" {
		t.Fatal("expected spill file")
	}
	data, err := os.ReadFile(result.ResultFile)
	if err != nil {
		t.Fatalf("read spill file: %v", err)
	}
	if int64(len(data)) > ceiling {
		t.Errorf("spill file = %d bytes, must not exceed ceiling %d", len(data), ceiling)
	}
}

func TestHTTPRequestMaxResponseBytesBelowBodyTruncates(t *testing.T) {
	// Proves an explicit max_response_bytes smaller than the body is a hard cap
	// (#2098): the result carries at most that many body bytes and no spill file,
	// so the full body can't reach the caller via ResultFile either.
	t.Parallel()
	const maxBytes = 400
	const bodySize = 64 * 1024
	srv := bigBodyServer(t, bodySize)
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, t.TempDir(), func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 1 << 20 }, func() int64 { return 0 }, nil, 0640)
	params, _ := json.Marshal(map[string]any{
		"url":                srv.URL,
		"max_response_bytes": maxBytes,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ResultFile != "" {
		t.Errorf("ResultFile = %q, want none: the body must be capped, not spilled", result.ResultFile)
	}
	if got := strings.Count(result.Text, "a"); got != maxBytes {
		t.Errorf("body bytes in result = %d, want %d", got, maxBytes)
	}
}

func TestHTTPRequestInlinePreviewCappedByMaxResultChars(t *testing.T) {
	// Proves the inline preview of a spilled text body is bounded by the
	// agent's max_result_chars guard, not the 1MB text default (#2107): ~1MB
	// inline is ~250k tokens. The full body still spills to disk.
	t.Parallel()
	const maxResultChars = 15000
	const bodySize = 200 * 1024 // well over the guard, under the 1MB default
	srv := bigBodyServer(t, bodySize)
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, t.TempDir(), func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 4 << 20 }, func() int64 { return maxResultChars }, nil, 0640)
	params, _ := json.Marshal(map[string]any{"url": srv.URL})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Text) != maxResultChars {
		t.Errorf("inline text = %d bytes, want the %d-byte max_result_chars head", len(result.Text), maxResultChars)
	}
	if result.ResultFile == "" {
		t.Fatal("expected the full body to spill to ResultFile")
	}
	if result.ResultSize != bodySize {
		t.Errorf("ResultSize = %d, want %d", result.ResultSize, bodySize)
	}
	data, err := os.ReadFile(result.ResultFile)
	if err != nil {
		t.Fatalf("read spill file: %v", err)
	}
	if len(data) != bodySize {
		t.Errorf("spill file = %d bytes, want the full %d-byte body", len(data), bodySize)
	}
}

func TestHTTPRequestInlinePreviewCapAppliesUnderMaxResponseBytes(t *testing.T) {
	// An explicit max_response_bytes above max_result_chars is still a hard cap
	// on retained bytes (#2098), but it no longer raises the inline preview past
	// the guard: the head stays at max_result_chars and the rest, up to the cap,
	// spills (#2107).
	t.Parallel()
	const maxResultChars = 1000
	const maxBytes = 5000
	srv := bigBodyServer(t, 64*1024)
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, t.TempDir(), func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 4 << 20 }, func() int64 { return maxResultChars }, nil, 0640)
	params, _ := json.Marshal(map[string]any{"url": srv.URL, "max_response_bytes": maxBytes})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Text) != maxResultChars {
		t.Errorf("inline text = %d bytes, want %d", len(result.Text), maxResultChars)
	}
	if result.ResultFile == "" {
		t.Fatal("expected the body past the preview to spill to ResultFile")
	}
	data, err := os.ReadFile(result.ResultFile)
	if err != nil {
		t.Fatalf("read spill file: %v", err)
	}
	if len(data) != maxBytes {
		t.Errorf("spill file = %d bytes, want the %d-byte max_response_bytes cap", len(data), maxBytes)
	}
}
