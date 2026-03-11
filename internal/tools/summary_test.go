package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/anthropic"
	"foci/internal/provider"
)

func TestSummaryTool_MissingParams(t *testing.T) {
	t.Parallel()
	client := anthropic.NewClientWithBase("http://unused", "test-key")
	tool := NewSummaryTool(client, nil, "anthropic/claude-haiku-4-5", nil)

	tests := []struct {
		name   string
		params map[string]string
		want   string
	}{
		{"missing file", map[string]string{"prompt": "summarize"}, "file parameter is required"},
		{"missing prompt", map[string]string{"file": "/tmp/x"}, "prompt parameter is required"},
		{"both empty", map[string]string{"file": "", "prompt": ""}, "file parameter is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params, _ := json.Marshal(tt.params)
			_, err := tool.Execute(context.Background(), params)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tt.want)
			}
		})
	}
}

func TestSummaryTool_FileNotFound(t *testing.T) {
	t.Parallel()
	client := anthropic.NewClientWithBase("http://unused", "test-key")
	tool := NewSummaryTool(client, nil, "anthropic/claude-haiku-4-5", nil)

	params, _ := json.Marshal(map[string]string{
		"file":   "/tmp/nonexistent-summary-test-file-xyz",
		"prompt": "summarize",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "read file") {
		t.Errorf("error = %q, want to contain 'read file'", err.Error())
	}
}

func TestSummaryTool_EmptyFile(t *testing.T) {
	t.Parallel()
	tmp := filepath.Join(t.TempDir(), "empty.txt")
	os.WriteFile(tmp, []byte{}, 0644)

	client := anthropic.NewClientWithBase("http://unused", "test-key")
	tool := NewSummaryTool(client, nil, "anthropic/claude-haiku-4-5", nil)

	params, _ := json.Marshal(map[string]string{
		"file":   tmp,
		"prompt": "summarize",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "file is empty") {
		t.Errorf("error = %q, want to contain 'file is empty'", err.Error())
	}
}

func TestSummaryTool_BinaryFile(t *testing.T) {
	t.Parallel()
	tmp := filepath.Join(t.TempDir(), "binary.dat")
	data := []byte("some text\x00more binary data")
	os.WriteFile(tmp, data, 0644)

	client := anthropic.NewClientWithBase("http://unused", "test-key")
	tool := NewSummaryTool(client, nil, "anthropic/claude-haiku-4-5", nil)

	params, _ := json.Marshal(map[string]string{
		"file":   tmp,
		"prompt": "summarize",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "binary") {
		t.Errorf("error = %q, want to contain 'binary'", err.Error())
	}
}

func TestSummaryTool_Success(t *testing.T) {
	t.Parallel()
	tmp := filepath.Join(t.TempDir(), "test.go")
	fileContent := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"
	os.WriteFile(tmp, []byte(fileContent), 0644)

	var gotReq provider.MessageRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		json.NewDecoder(r.Body).Decode(&gotReq)

		resp := provider.MessageResponse{
			ID:   "msg_test",
			Type: "message",
			Role: "assistant",
			Content: []provider.ContentBlock{
				{Type: "text", Text: "This is a Go hello world program."},
			},
			Model:      "claude-haiku-4-5",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 20},
			StopReason: "end_turn",
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	aliases := map[string]string{"haiku": "anthropic/claude-haiku-4-5"}
	tool := NewSummaryTool(client, nil, "anthropic/claude-haiku-4-5", aliases)

	params, _ := json.Marshal(map[string]string{
		"file":   tmp,
		"prompt": "What does this program do?",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Text != "This is a Go hello world program." {
		t.Errorf("result = %q, want %q", result.Text, "This is a Go hello world program.")
	}

	// Verify the request sent to the API
	if gotReq.Model != "claude-haiku-4-5" {
		t.Errorf("model = %q, want %q", gotReq.Model, "claude-haiku-4-5")
	}
	if gotReq.MaxTokens != 4096 {
		t.Errorf("max_tokens = %d, want %d", gotReq.MaxTokens, 4096)
	}
	if len(gotReq.Messages) != 1 {
		t.Fatalf("messages count = %d, want 1", len(gotReq.Messages))
	}

	msgText := provider.TextOf(gotReq.Messages[0].Content)
	if !strings.Contains(msgText, fileContent) {
		t.Error("request message does not contain file content")
	}
	if !strings.Contains(msgText, "What does this program do?") {
		t.Error("request message does not contain prompt")
	}
}

func TestSummaryTool_ModelAlias(t *testing.T) {
	t.Parallel()
	tmp := filepath.Join(t.TempDir(), "test.txt")
	os.WriteFile(tmp, []byte("hello"), 0644)

	var gotModel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req provider.MessageRequest
		json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model

		resp := provider.MessageResponse{
			ID:      "msg_test",
			Type:    "message",
			Role:    "assistant",
			Content: []provider.ContentBlock{{Type: "text", Text: "ok"}},
			Usage:   provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	aliases := map[string]string{
		"haiku": "anthropic/claude-haiku-4-5-custom",
	}
	client := anthropic.NewClientWithBase(server.URL, "test-key")
	tool := NewSummaryTool(client, nil, "anthropic/claude-haiku-4-5", aliases)

	params, _ := json.Marshal(map[string]string{"file": tmp, "prompt": "summarize"})
	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotModel != "claude-haiku-4-5-custom" {
		t.Errorf("model = %q, want %q", gotModel, "claude-haiku-4-5-custom")
	}
}
