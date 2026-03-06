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

// TestHTTPRequestAutoBackgroundFast verifies fast requests don't trigger background mode
func TestHTTPRequestAutoBackgroundFast(t *testing.T) {
	// A fast request should complete before the threshold — no notification
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "fast response")
	}))
	defer srv.Close()

	var called bool
	tool := NewHTTPRequestTool(nil, nil, "", 5, 50*1024*1024, NewAsyncNotifier(func(sk, msg string, replyTo string) {
		called = true
	}))

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

// TestHTTPRequestAutoBackgroundSlow verifies slow requests auto-background after threshold
func TestHTTPRequestAutoBackgroundSlow(t *testing.T) {
	t.Parallel()
	// A slow request should auto-background after 1 second
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		fmt.Fprint(w, "slow response")
	}))
	defer srv.Close()

	completeCh := make(chan string, 1)
	tool := NewHTTPRequestTool(nil, nil, "", 1, 50*1024*1024, NewAsyncNotifier(func(sk, msg string, replyTo string) {
		completeCh <- msg
	}))

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

// TestHTTPRequestAutoBackgroundSessionKey verifies session key context is passed to notifier
func TestHTTPRequestAutoBackgroundSessionKey(t *testing.T) {
	t.Parallel()
	// Verify the session key from context reaches the notifier callback
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		fmt.Fprint(w, "done")
	}))
	defer srv.Close()

	type result struct {
		sk, msg string
	}
	ch := make(chan result, 1)
	tool := NewHTTPRequestTool(nil, nil, "", 1, 50*1024*1024, NewAsyncNotifier(func(sk, msg string, replyTo string) {
		ch <- result{sk, msg}
	}))

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

// TestHTTPRequestExplicitBackground verifies background=true returns immediately
func TestHTTPRequestExplicitBackground(t *testing.T) {
	// background=true should return immediately and deliver via notifier
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "bg response")
	}))
	defer srv.Close()

	completeCh := make(chan string, 1)
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, NewAsyncNotifier(func(sk, msg string, replyTo string) {
		completeCh <- msg
	}))

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

// TestHTTPRequestBackgroundNoNotifier verifies background=true runs sync without notifier
func TestHTTPRequestBackgroundNoNotifier(t *testing.T) {
	// background=true but no notifier — should run synchronously
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "sync response")
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)

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
