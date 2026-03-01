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

	"foci/anthropic"
)

func TestSummaryTool_MissingParams(t *testing.T) {
	client := anthropic.NewClientWithBase("http://unused", "test-key")
	tool := NewSummaryTool(client)

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
	client := anthropic.NewClientWithBase("http://unused", "test-key")
	tool := NewSummaryTool(client)

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
	tmp := filepath.Join(t.TempDir(), "empty.txt")
	os.WriteFile(tmp, []byte{}, 0644)

	client := anthropic.NewClientWithBase("http://unused", "test-key")
	tool := NewSummaryTool(client)

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
	tmp := filepath.Join(t.TempDir(), "binary.dat")
	data := []byte("some text\x00more binary data")
	os.WriteFile(tmp, data, 0644)

	client := anthropic.NewClientWithBase("http://unused", "test-key")
	tool := NewSummaryTool(client)

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
	tmp := filepath.Join(t.TempDir(), "test.go")
	fileContent := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"
	os.WriteFile(tmp, []byte(fileContent), 0644)

	var gotReq anthropic.MessageRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		json.NewDecoder(r.Body).Decode(&gotReq)

		resp := anthropic.MessageResponse{
			ID:   "msg_test",
			Type: "message",
			Role: "assistant",
			Content: []anthropic.ContentBlock{
				{Type: "text", Text: "This is a Go hello world program."},
			},
			Model:      "claude-haiku-4-5",
			Usage:      anthropic.Usage{InputTokens: 100, OutputTokens: 20},
			StopReason: "end_turn",
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-key")
	tool := NewSummaryTool(client)

	params, _ := json.Marshal(map[string]string{
		"file":   tmp,
		"prompt": "What does this program do?",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != "This is a Go hello world program." {
		t.Errorf("result = %q, want %q", result, "This is a Go hello world program.")
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

	msgText := anthropic.TextOf(gotReq.Messages[0].Content)
	if !strings.Contains(msgText, fileContent) {
		t.Error("request message does not contain file content")
	}
	if !strings.Contains(msgText, "What does this program do?") {
		t.Error("request message does not contain prompt")
	}
}
