// Package mcp provides an MCP client manager that connects to external
// MCP servers and exposes their tools as a single foci tool.
package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"foci/log"
	"foci/provider"
	"foci/tools"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerConfig describes one MCP server to connect to.
type ServerConfig struct {
	Name    string   `toml:"name"`
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
	Env     []string `toml:"env"`
	URL     string   `toml:"url"`
}

// serverConn is one connected MCP server with its cached tool list.
type serverConn struct {
	name    string
	session *mcp.ClientSession
	tools   []*mcp.Tool
}

// Manager manages connections to MCP servers and builds a foci tool
// that dispatches calls to them.
type Manager struct {
	mu      sync.Mutex
	servers []serverConn
}

// NewManager creates an empty MCP manager with no connections.
func NewManager() *Manager {
	return &Manager{}
}

// Connect connects to all configured MCP servers. Servers that fail to
// connect are logged and skipped — partial success is acceptable.
// The provided transport function, if non-nil, overrides the default
// transport creation (used for testing).
func (m *Manager) Connect(ctx context.Context, servers []ServerConfig) error {
	return m.connectWith(ctx, servers, nil)
}

// transportFactory creates a transport for a server config. Used for testing.
type transportFactory func(cfg ServerConfig) (mcp.Transport, error)

// connectWith is the internal connect implementation that accepts an optional
// transport factory for testing.
func (m *Manager) connectWith(ctx context.Context, servers []ServerConfig, tf transportFactory) error {
	for _, cfg := range servers {
		var transport mcp.Transport
		var err error

		if tf != nil {
			transport, err = tf(cfg)
		} else {
			transport, err = makeTransport(cfg)
		}
		if err != nil {
			log.Warnf("mcp", "failed to create transport for %q: %v", cfg.Name, err)
			continue
		}

		client := mcp.NewClient(&mcp.Implementation{Name: "foci", Version: "1.0.0"}, nil)
		session, err := client.Connect(ctx, transport, nil)
		if err != nil {
			log.Warnf("mcp", "failed to connect to %q: %v", cfg.Name, err)
			continue
		}

		result, err := session.ListTools(ctx, nil)
		if err != nil {
			log.Warnf("mcp", "failed to list tools from %q: %v", cfg.Name, err)
			session.Close()
			continue
		}

		m.mu.Lock()
		m.servers = append(m.servers, serverConn{
			name:    cfg.Name,
			session: session,
			tools:   result.Tools,
		})
		m.mu.Unlock()

		log.Infof("mcp", "connected to %q: %d tools", cfg.Name, len(result.Tools))
	}
	return nil
}

// makeTransport creates the appropriate transport for a server config.
func makeTransport(cfg ServerConfig) (mcp.Transport, error) {
	if cfg.URL != "" {
		return &mcp.StreamableClientTransport{Endpoint: cfg.URL}, nil
	}
	if cfg.Command == "" {
		return nil, fmt.Errorf("server %q has neither command nor url", cfg.Name)
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	if len(cfg.Env) > 0 {
		cmd.Env = append(cmd.Environ(), cfg.Env...)
	}
	return &mcp.CommandTransport{Command: cmd}, nil
}

// Close closes all connected MCP sessions.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []string
	for _, s := range m.servers {
		if err := s.session.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", s.name, err))
		}
	}
	m.servers = nil
	if len(errs) > 0 {
		return fmt.Errorf("closing MCP sessions: %s", strings.Join(errs, "; "))
	}
	return nil
}

// ServerCount returns the number of connected servers.
func (m *Manager) ServerCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.servers)
}

// ToolCount returns the total number of tools across all connected servers.
func (m *Manager) ToolCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.servers {
		n += len(s.tools)
	}
	return n
}

// Tool returns a foci tool that dispatches to MCP servers, or nil if
// no servers are connected.
func (m *Manager) Tool() *tools.Tool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.servers) == 0 {
		return nil
	}

	desc := m.buildDescription()

	return &tools.Tool{
		Name:        "mcp",
		Description: desc,
		Parameters:  mcpToolSchema,
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return m.execute(ctx, params)
		},
	}
}

// mcpToolSchema is the JSON Schema for the mcp tool parameters.
var mcpToolSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"server": {
			"type": "string",
			"description": "The MCP server name"
		},
		"tool": {
			"type": "string",
			"description": "The tool name on the server"
		},
		"arguments": {
			"type": "object",
			"description": "Arguments to pass to the tool"
		}
	},
	"required": ["server", "tool"]
}`)

// buildDescription creates a dynamic tool description listing all servers and their tools.
// Caller must hold m.mu.
func (m *Manager) buildDescription() string {
	var b strings.Builder
	b.WriteString("Call a tool on a connected MCP server.\n\nAvailable servers and tools:\n")

	for _, s := range m.servers {
		fmt.Fprintf(&b, "\n## %s\n", s.name)

		// Sort tools by name for deterministic output.
		sorted := make([]*mcp.Tool, len(s.tools))
		copy(sorted, s.tools)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

		for _, t := range sorted {
			fmt.Fprintf(&b, "\n### %s\n", t.Name)
			if t.Description != "" {
				fmt.Fprintf(&b, "%s\n", t.Description)
			}
			if t.InputSchema != nil {
				schemaJSON, err := json.Marshal(t.InputSchema)
				if err == nil && string(schemaJSON) != "null" && string(schemaJSON) != "{}" {
					fmt.Fprintf(&b, "Parameters: %s\n", schemaJSON)
				}
			}
		}
	}

	return b.String()
}

// mcpParams is the parsed input for the mcp tool.
type mcpParams struct {
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// execute dispatches a tool call to the appropriate MCP server.
func (m *Manager) execute(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
	var p mcpParams
	if err := json.Unmarshal(params, &p); err != nil {
		return tools.ToolResult{}, fmt.Errorf("invalid mcp tool params: %w", err)
	}

	m.mu.Lock()
	var conn *serverConn
	for i := range m.servers {
		if m.servers[i].name == p.Server {
			conn = &m.servers[i]
			break
		}
	}
	m.mu.Unlock()

	if conn == nil {
		return tools.TextResult(fmt.Sprintf("error: unknown MCP server %q", p.Server)), nil
	}

	// Verify the tool exists on this server.
	found := false
	for _, t := range conn.tools {
		if t.Name == p.Tool {
			found = true
			break
		}
	}
	if !found {
		return tools.TextResult(fmt.Sprintf("error: server %q has no tool %q", p.Server, p.Tool)), nil
	}

	// Parse arguments into a map for the SDK.
	var args map[string]any
	if len(p.Arguments) > 0 {
		if err := json.Unmarshal(p.Arguments, &args); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid tool arguments: %w", err)
		}
	}

	result, err := conn.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      p.Tool,
		Arguments: args,
	})
	if err != nil {
		return tools.TextResult(fmt.Sprintf("error: MCP tool call failed: %v", err)), nil
	}

	return convertResult(result), nil
}

// convertResult converts an MCP CallToolResult to a foci ToolResult.
func convertResult(result *mcp.CallToolResult) tools.ToolResult {
	var textParts []string
	var extraBlocks []provider.ContentBlock

	for _, c := range result.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			textParts = append(textParts, v.Text)
		case *mcp.ImageContent:
			extraBlocks = append(extraBlocks, provider.ImageBlock(
				v.MIMEType,
				base64.StdEncoding.EncodeToString(v.Data),
			))
		default:
			// Other content types (audio, resource, etc.) — render as text.
			data, err := json.Marshal(c)
			if err == nil {
				textParts = append(textParts, string(data))
			}
		}
	}

	text := strings.Join(textParts, "\n")
	if result.IsError && text != "" {
		text = "error: " + text
	}

	return tools.ToolResult{
		Text:        text,
		ExtraBlocks: extraBlocks,
	}
}
