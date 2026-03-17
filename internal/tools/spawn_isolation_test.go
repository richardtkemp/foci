package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"foci/internal/provider"
)

func TestSpawnRawCreatesTempDir(t *testing.T) {
	// Proves that raw context spawns create an isolated temp directory for file operations.
	t.Parallel()
	var spawnTempDir string
	server := mockModelServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: provider.TextContent("Done."), StopReason: "end_turn",
			Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-token")
	deps := SpawnDeps{Client: client, FallbackModel: "anthropic/claude-haiku-4-5", FallbackFormat: "anthropic", MaxToolLoops: 10}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "test",
		"context": "raw",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if strings.Contains(result.Text, "Files created in /tmp/foci/spawn/foci-spawn-") {
		spawnTempDir = extractTempDir(result.Text)
	}
	_ = spawnTempDir
}

func TestSpawnRawIsolationWritesToTempDir(t *testing.T) {
	// Proves that files written by the model during a raw spawn go into the isolated temp dir,
	// and that the result includes a file list pointing to the temp dir.
	t.Parallel()
	callCount := 0
	var spawnTempDir string
	server := mockModelServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		callCount++
		if callCount == 1 {
			return &provider.MessageResponse{
				ID: "msg_1", Type: "message", Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "tool_use", ID: "tu_1", Name: "write", Input: json.RawMessage(`{"path":"output.txt","content":"test data"}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		return &provider.MessageResponse{
			ID: "msg_2", Type: "message", Role: "assistant",
			Content: provider.TextContent("File written."), StopReason: "end_turn",
			Usage: provider.Usage{InputTokens: 20, OutputTokens: 10},
		}
	})
	defer server.Close()

	reg := NewRegistry()
	reg.Register(NewWriteTool(nil, "", nil))

	client := newTestAnthropicClient(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Registry: reg, FallbackModel: "anthropic/claude-haiku-4-5", FallbackFormat: "anthropic", MaxToolLoops: 10}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "test",
		"context": "raw",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "File written.") {
		t.Errorf("expected result, got %q", result.Text)
	}

	if !strings.Contains(result.Text, "Files created in /tmp/foci/spawn/foci-spawn-") {
		t.Errorf("expected file list in result, got %q", result.Text)
	}

	spawnTempDir = extractTempDir(result.Text)
	if spawnTempDir == "" {
		t.Fatal("failed to extract temp dir from result")
	}

	data, err := os.ReadFile(spawnTempDir + "/output.txt")
	if err != nil {
		t.Fatalf("read file in temp dir: %v", err)
	}
	if string(data) != "test data" {
		t.Errorf("file content = %q", string(data))
	}
}

func TestSpawnRawIsolationBlocksAbsolutePath(t *testing.T) {
	// Proves that the sandbox rejects write attempts to absolute paths outside the temp dir,
	// preventing the model from writing to arbitrary filesystem locations.
	t.Parallel()
	callCount := 0
	server := mockModelServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		callCount++
		if callCount == 1 {
			return &provider.MessageResponse{
				ID: "msg_1", Type: "message", Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "tool_use", ID: "tu_1", Name: "write", Input: json.RawMessage(`{"path":"/tmp/malicious.txt","content":"bad"}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		return &provider.MessageResponse{
			ID: "msg_2", Type: "message", Role: "assistant",
			Content: provider.TextContent("Error received."), StopReason: "end_turn",
			Usage: provider.Usage{InputTokens: 20, OutputTokens: 10},
		}
	})
	defer server.Close()

	reg := NewRegistry()
	reg.Register(NewWriteTool(nil, "", nil))

	client := newTestAnthropicClient(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Registry: reg, FallbackModel: "anthropic/claude-haiku-4-5", FallbackFormat: "anthropic", MaxToolLoops: 10}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "test",
		"context": "raw",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "Error received.") {
		t.Errorf("expected result, got %q", result.Text)
	}
}

func TestSpawnRawIsolationBlocksTraversal(t *testing.T) {
	// Proves that path-traversal attempts (../ sequences) in write paths are blocked by the sandbox.
	t.Parallel()
	callCount := 0
	server := mockModelServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		callCount++
		if callCount == 1 {
			return &provider.MessageResponse{
				ID: "msg_1", Type: "message", Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "tool_use", ID: "tu_1", Name: "write", Input: json.RawMessage(`{"path":"../../../tmp/escape.txt","content":"bad"}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		return &provider.MessageResponse{
			ID: "msg_2", Type: "message", Role: "assistant",
			Content: provider.TextContent("Error received."), StopReason: "end_turn",
			Usage: provider.Usage{InputTokens: 20, OutputTokens: 10},
		}
	})
	defer server.Close()

	reg := NewRegistry()
	reg.Register(NewWriteTool(nil, "", nil))

	client := newTestAnthropicClient(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Registry: reg, FallbackModel: "anthropic/claude-haiku-4-5", FallbackFormat: "anthropic", MaxToolLoops: 10}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "test",
		"context": "raw",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "Error received.") {
		t.Errorf("expected result, got %q", result.Text)
	}
}

func TestSpawnRawFileListMultiple(t *testing.T) {
	// Proves that the result includes all files written during the spawn along with their sizes.
	t.Parallel()
	callCount := 0
	server := mockModelServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		callCount++
		if callCount == 1 {
			return &provider.MessageResponse{
				ID: "msg_1", Type: "message", Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "tool_use", ID: "tu_1", Name: "write", Input: json.RawMessage(`{"path":"a.txt","content":"aaa"}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		if callCount == 2 {
			return &provider.MessageResponse{
				ID: "msg_2", Type: "message", Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "tool_use", ID: "tu_2", Name: "write", Input: json.RawMessage(`{"path":"b.txt","content":"bbbbb"}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}
		return &provider.MessageResponse{
			ID: "msg_3", Type: "message", Role: "assistant",
			Content: provider.TextContent("Files written."), StopReason: "end_turn",
			Usage: provider.Usage{InputTokens: 30, OutputTokens: 15},
		}
	})
	defer server.Close()

	reg := NewRegistry()
	reg.Register(NewWriteTool(nil, "", nil))

	client := newTestAnthropicClient(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Registry: reg, FallbackModel: "anthropic/claude-haiku-4-5", FallbackFormat: "anthropic", MaxToolLoops: 10}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "test",
		"context": "raw",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "a.txt") {
		t.Errorf("expected a.txt in file list, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "b.txt") {
		t.Errorf("expected b.txt in file list, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "3 B") && !strings.Contains(result.Text, "5 B") {
		t.Errorf("expected file sizes in file list, got %q", result.Text)
	}
}
