package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newTestServer creates a test MCP server with one tool and returns
// the paired in-memory transports (server-side, client-side).
func newTestServer(ctx context.Context, t *testing.T) (*mcp.InMemoryTransport, *mcp.InMemoryTransport) {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "Echoes the input",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{
					"type":        "string",
					"description": "Message to echo",
				},
			},
			"required": []string{"message"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "echo: " + args.Message},
			},
		}, nil
	})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	// Connect server side in background.
	go func() {
		_, err := server.Connect(ctx, serverTransport, nil)
		if err != nil && ctx.Err() == nil {
			t.Logf("server connect error: %v", err)
		}
	}()

	return serverTransport, clientTransport
}

func TestNewManager_NoServers(t *testing.T) {
	m := NewManager()
	defer m.Close()

	if err := m.Connect(context.Background(), nil); err != nil {
		t.Fatalf("Connect with no servers: %v", err)
	}
	if m.ServerCount() != 0 {
		t.Errorf("ServerCount = %d, want 0", m.ServerCount())
	}
	if m.ToolCount() != 0 {
		t.Errorf("ToolCount = %d, want 0", m.ToolCount())
	}
	if tool := m.Tool(); tool != nil {
		t.Error("Tool() should be nil with no servers")
	}
}

func TestConnect_EndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, clientTransport := newTestServer(ctx, t)

	m := NewManager()
	defer m.Close()

	err := m.connectWith(ctx, []ServerConfig{{Name: "test"}}, func(cfg ServerConfig) (mcp.Transport, error) {
		return clientTransport, nil
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if m.ServerCount() != 1 {
		t.Fatalf("ServerCount = %d, want 1", m.ServerCount())
	}
	if m.ToolCount() != 1 {
		t.Fatalf("ToolCount = %d, want 1", m.ToolCount())
	}

	tool := m.Tool()
	if tool == nil {
		t.Fatal("Tool() returned nil")
	}
	if tool.Name != "mcp" {
		t.Errorf("tool.Name = %q, want %q", tool.Name, "mcp")
	}
}

func TestToolCall_Success(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, clientTransport := newTestServer(ctx, t)

	m := NewManager()
	defer m.Close()

	m.connectWith(ctx, []ServerConfig{{Name: "test"}}, func(cfg ServerConfig) (mcp.Transport, error) {
		return clientTransport, nil
	})

	tool := m.Tool()
	if tool == nil {
		t.Fatal("Tool() returned nil")
	}

	params, _ := json.Marshal(mcpParams{
		Server:    "test",
		Tool:      "echo",
		Arguments: json.RawMessage(`{"message": "hello"}`),
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Text != "echo: hello" {
		t.Errorf("result.Text = %q, want %q", result.Text, "echo: hello")
	}
}

func TestToolCall_UnknownServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, clientTransport := newTestServer(ctx, t)

	m := NewManager()
	defer m.Close()

	m.connectWith(ctx, []ServerConfig{{Name: "test"}}, func(cfg ServerConfig) (mcp.Transport, error) {
		return clientTransport, nil
	})

	tool := m.Tool()
	params, _ := json.Marshal(mcpParams{
		Server: "nonexistent",
		Tool:   "echo",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "unknown MCP server") {
		t.Errorf("expected unknown server error, got %q", result.Text)
	}
}

func TestToolCall_UnknownTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, clientTransport := newTestServer(ctx, t)

	m := NewManager()
	defer m.Close()

	m.connectWith(ctx, []ServerConfig{{Name: "test"}}, func(cfg ServerConfig) (mcp.Transport, error) {
		return clientTransport, nil
	})

	tool := m.Tool()
	params, _ := json.Marshal(mcpParams{
		Server: "test",
		Tool:   "nonexistent",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "has no tool") {
		t.Errorf("expected unknown tool error, got %q", result.Text)
	}
}

func TestToolCall_ServerError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := mcp.NewServer(&mcp.Implementation{Name: "err-server"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "fail",
		Description: "Always fails",
		InputSchema: map[string]any{"type": "object"},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{Text: "something went wrong"},
			},
		}, nil
	})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go server.Connect(ctx, serverTransport, nil)

	m := NewManager()
	defer m.Close()

	m.connectWith(ctx, []ServerConfig{{Name: "err"}}, func(cfg ServerConfig) (mcp.Transport, error) {
		return clientTransport, nil
	})

	tool := m.Tool()
	params, _ := json.Marshal(mcpParams{
		Server: "err",
		Tool:   "fail",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "error:") {
		t.Errorf("expected error prefix, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "something went wrong") {
		t.Errorf("expected error message, got %q", result.Text)
	}
}

func TestClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, clientTransport := newTestServer(ctx, t)

	m := NewManager()

	m.connectWith(ctx, []ServerConfig{{Name: "test"}}, func(cfg ServerConfig) (mcp.Transport, error) {
		return clientTransport, nil
	})

	if m.ServerCount() != 1 {
		t.Fatalf("ServerCount = %d, want 1", m.ServerCount())
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if m.ServerCount() != 0 {
		t.Errorf("ServerCount after Close = %d, want 0", m.ServerCount())
	}
}

func TestDescription_IncludesServerAndToolInfo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, clientTransport := newTestServer(ctx, t)

	m := NewManager()
	defer m.Close()

	m.connectWith(ctx, []ServerConfig{{Name: "myserver"}}, func(cfg ServerConfig) (mcp.Transport, error) {
		return clientTransport, nil
	})

	tool := m.Tool()
	if tool == nil {
		t.Fatal("Tool() returned nil")
	}

	if !strings.Contains(tool.Description, "myserver") {
		t.Errorf("description missing server name, got:\n%s", tool.Description)
	}
	if !strings.Contains(tool.Description, "echo") {
		t.Errorf("description missing tool name, got:\n%s", tool.Description)
	}
	if !strings.Contains(tool.Description, "Echoes the input") {
		t.Errorf("description missing tool description, got:\n%s", tool.Description)
	}
}

func TestMultipleServers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create two separate servers.
	makeServer := func(name, toolName, toolDesc string) *mcp.InMemoryTransport {
		server := mcp.NewServer(&mcp.Implementation{Name: name}, nil)
		server.AddTool(&mcp.Tool{
			Name:        toolName,
			Description: toolDesc,
			InputSchema: map[string]any{"type": "object"},
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: fmt.Sprintf("from %s/%s", name, toolName)},
				},
			}, nil
		})
		st, ct := mcp.NewInMemoryTransports()
		go server.Connect(ctx, st, nil)
		return ct
	}

	ct1 := makeServer("server1", "tool_a", "Tool A")
	ct2 := makeServer("server2", "tool_b", "Tool B")

	transports := map[string]mcp.Transport{
		"server1": ct1,
		"server2": ct2,
	}

	m := NewManager()
	defer m.Close()

	m.connectWith(ctx, []ServerConfig{
		{Name: "server1"},
		{Name: "server2"},
	}, func(cfg ServerConfig) (mcp.Transport, error) {
		t, ok := transports[cfg.Name]
		if !ok {
			return nil, fmt.Errorf("unknown server %q", cfg.Name)
		}
		return t, nil
	})

	if m.ServerCount() != 2 {
		t.Fatalf("ServerCount = %d, want 2", m.ServerCount())
	}
	if m.ToolCount() != 2 {
		t.Fatalf("ToolCount = %d, want 2", m.ToolCount())
	}

	tool := m.Tool()

	// Call tool on server1.
	params, _ := json.Marshal(mcpParams{Server: "server1", Tool: "tool_a"})
	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute server1: %v", err)
	}
	if result.Text != "from server1/tool_a" {
		t.Errorf("server1 result = %q, want %q", result.Text, "from server1/tool_a")
	}

	// Call tool on server2.
	params, _ = json.Marshal(mcpParams{Server: "server2", Tool: "tool_b"})
	result, err = tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute server2: %v", err)
	}
	if result.Text != "from server2/tool_b" {
		t.Errorf("server2 result = %q, want %q", result.Text, "from server2/tool_b")
	}
}
