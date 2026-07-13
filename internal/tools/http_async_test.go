package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPRequestAutoBackgroundFast(t *testing.T) {
	// Proves that a fast-responding server completes synchronously and never triggers the async notifier, even when a background threshold is set.
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "fast response")
	}))
	defer srv.Close()

	var called bool
	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 5 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		called = true
	}), 0640)

	params, _ := json.Marshal(map[string]interface{}{
		"url": srv.URL,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "fast response") {
		t.Errorf("result = %q, want 'fast response'", result.Text)
	}
	if called {
		t.Error("notifier should not be called for fast requests")
	}
}

func TestHTTPRequestAutoBackgroundSlow(t *testing.T) {
	// Proves that when a request exceeds the auto-background threshold, the tool returns a "still running" message immediately and delivers the final result via the notifier.
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		fmt.Fprint(w, "slow response")
	}))
	defer srv.Close()

	completeCh := make(chan string, 1)
	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 1 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		completeCh <- msg
	}), 0640)

	params, _ := json.Marshal(map[string]interface{}{
		"url":     srv.URL,
		"timeout": 10,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should get the auto-background message
	if !strings.Contains(result.Text, "still running") {
		t.Errorf("expected auto-background message, got %q", result.Text)
	}

	// Wait for the request to complete
	select {
	case completed := <-completeCh:
		if !strings.Contains(completed, "slow response") {
			t.Errorf("expected 'slow response' in completed message, got %q", completed)
		}
		if !strings.Contains(completed, "[HTTP RESULT]") {
			t.Errorf("expected [HTTP RESULT] prefix, got %q", completed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for auto-backgrounded request")
	}
}

func TestHTTPRequestAutoBackgroundSessionKey(t *testing.T) {
	// Proves that the session key embedded in the context is propagated to the notifier callback when a request auto-backgrounds.
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		fmt.Fprint(w, "done")
	}))
	defer srv.Close()

	type result struct {
		sk, msg string
	}
	ch := make(chan result, 1)
	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 1 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		ch <- result{sk, msg}
	}), 0640)

	params, _ := json.Marshal(map[string]interface{}{
		"url":     srv.URL,
		"timeout": 10,
	})

	ctx := WithSessionKey(context.Background(), "agent:test:branch-42")
	out, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "still running") {
		t.Fatalf("expected auto-background message, got %q", out.Text)
	}

	select {
	case r := <-ch:
		if r.sk != "agent:test:branch-42" {
			t.Errorf("session key = %q, want %q", r.sk, "agent:test:branch-42")
		}
		if r.msg == "" {
			t.Error("message should not be empty")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for notifier callback")
	}
}

func TestHTTPRequestExplicitBackground(t *testing.T) {
	// Proves that background=true causes the tool to return immediately with a background acknowledgment, then deliver the response body via the notifier once complete.
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "bg response")
	}))
	defer srv.Close()

	completeCh := make(chan string, 1)
	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		completeCh <- msg
	}), 0640)

	params, _ := json.Marshal(map[string]interface{}{
		"url":        srv.URL,
		"background": true,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should get the background message
	if !strings.Contains(result.Text, "background") {
		t.Errorf("expected background message, got %q", result.Text)
	}

	// Wait for the request to complete
	select {
	case completed := <-completeCh:
		if !strings.Contains(completed, "bg response") {
			t.Errorf("expected 'bg response' in completed message, got %q", completed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for background request")
	}
}

func TestHTTPRequestBackgroundNoNotifier(t *testing.T) {
	// Proves that background=true falls back to synchronous execution and returns the response inline when no notifier is configured.
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "sync response")
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, nil, 0640)

	params, _ := json.Marshal(map[string]interface{}{
		"url":        srv.URL,
		"background": true,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "sync response") {
		t.Errorf("expected sync response, got %q", result.Text)
	}
}
