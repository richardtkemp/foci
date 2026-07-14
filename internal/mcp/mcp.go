// Package mcp provides an MCP client manager that connects to external
// MCP servers and exposes their tools as a single foci tool.
package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"foci/internal/log"
	"foci/internal/procx"
	"foci/internal/provider"
	"foci/internal/tools"

	"github.com/BurntSushi/toml"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	mcpLog = log.NewComponentLogger("mcp")
)

// ServerConfig describes one MCP server to connect to.
type ServerConfig struct {
	Name    string   `toml:"name"`
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
	Env     []string `toml:"env"`
	URL     string   `toml:"url"`
	Agents  []string `toml:"agents"` // if non-empty, only these agent IDs use this server
}

// MCPConfig is the top-level structure of mcp.toml.
type MCPConfig struct {
	Servers []ServerConfig `toml:"servers"`
}

// LoadConfig reads mcp.toml from the given directory.
// Returns empty config (no error) if the file doesn't exist.
func LoadConfig(dir string) (MCPConfig, error) {
	path := filepath.Join(dir, "mcp.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return MCPConfig{}, nil
		}
		return MCPConfig{}, fmt.Errorf("read mcp.toml: %w", err)
	}
	var cfg MCPConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return MCPConfig{}, fmt.Errorf("parse mcp.toml: %w", err)
	}
	return cfg, nil
}

// ServersForAgent returns the subset of servers available to the given agent ID.
// Servers with an empty Agents list are available to all agents.
func (c MCPConfig) ServersForAgent(agentID string) []ServerConfig {
	var result []ServerConfig
	for _, s := range c.Servers {
		if len(s.Agents) == 0 {
			result = append(result, s)
			continue
		}
		for _, a := range s.Agents {
			if a == agentID {
				result = append(result, s)
				break
			}
		}
	}
	return result
}

// serverConn is one connected MCP server with its cached tool list.
type serverConn struct {
	name    string
	session *mcp.ClientSession
	tools   []*mcp.Tool
}

// Manager manages connections to MCP servers and builds a foci tool
// that dispatches calls to them. When configDir and agentID are set,
// the manager re-reads mcp.toml on every tool call and reconnects
// if the server list has changed.
type Manager struct {
	mu        sync.RWMutex
	servers   []serverConn
	configDir string           // directory containing mcp.toml
	agentID   string           // agent ID for server filtering
	current   []ServerConfig   // last-applied server configs (for change detection)
	tf        transportFactory // nil in production, set for testing
}

// NewManagerForAgent creates an MCP manager that dynamically re-reads
// mcp.toml from configDir on every tool call, filtering servers for agentID.
func NewManagerForAgent(configDir, agentID string) *Manager {
	return &Manager{
		configDir: configDir,
		agentID:   agentID,
	}
}

// transportFactory creates a transport for a server config. Used for testing.
type transportFactory func(cfg ServerConfig) (mcp.Transport, error)

// connectWith is the internal connect implementation that accepts an optional
// transport factory for testing. Per-server failures are logged as warnings
// and the loop continues to the next server — this is a best-effort fan-out,
// so there is no aggregate error to return.
func (m *Manager) connectWith(ctx context.Context, servers []ServerConfig, tf transportFactory) {
	for _, cfg := range servers {
		var transport mcp.Transport
		var err error

		if tf != nil {
			transport, err = tf(cfg)
		} else {
			transport, err = makeTransport(cfg)
		}
		if err != nil {
			mcpLog.Warnf("failed to create transport for %q: %v", cfg.Name, err)
			continue
		}

		client := mcp.NewClient(&mcp.Implementation{Name: "foci", Version: "1.0.0"}, nil)
		session, err := client.Connect(ctx, transport, nil)
		if err != nil {
			mcpLog.Warnf("failed to connect to %q: %v", cfg.Name, err)
			continue
		}

		result, err := session.ListTools(ctx, nil)
		if err != nil {
			mcpLog.Warnf("failed to list tools from %q: %v", cfg.Name, err)
			_ = session.Close() // best effort cleanup
			continue
		}

		m.mu.Lock()
		m.servers = append(m.servers, serverConn{
			name:    cfg.Name,
			session: session,
			tools:   result.Tools,
		})
		m.mu.Unlock()

		mcpLog.Infof("connected to %q: %d tools", cfg.Name, len(result.Tools))
	}
}

// makeTransport creates the appropriate transport for a server config.
func makeTransport(cfg ServerConfig) (mcp.Transport, error) {
	if cfg.URL != "" {
		return &mcp.StreamableClientTransport{Endpoint: cfg.URL}, nil
	}
	if cfg.Command == "" {
		return nil, fmt.Errorf("server %q has neither command nor url", cfg.Name)
	}
	cmd := procx.Spawn(context.Background(), cfg.Command, cfg.Args...)
	// MCP servers are third-party subprocesses. Never inherit the gateway's
	// full environment — that would hand them FOCI_GW_SOCK / FOCI_SOCK (the
	// unauthenticated control + exec-bridge sockets) and any secret-bearing
	// operator vars. Give them a minimal allowlist plus the explicit env from
	// mcp.toml instead.
	cmd.Env = allowlistedEnv(cfg.Env)
	return &mcp.CommandTransport{Command: cmd}, nil
}

// mcpEnvAllowlist names the environment variables an MCP server subprocess may
// inherit from the gateway. LC_* is matched by prefix in allowlistedEnv. Servers
// that need anything else must be given it explicitly via mcp.toml `env`.
var mcpEnvAllowlist = map[string]bool{
	"PATH":    true,
	"HOME":    true,
	"USER":    true,
	"LOGNAME": true,
	"SHELL":   true,
	"TERM":    true,
	"TZ":      true,
	"LANG":    true,
	"TMPDIR":  true,
}

// allowlistedEnv builds a minimal environment for an MCP server subprocess: the
// allowlisted subset of the gateway's own environment plus the explicit extras
// from mcp.toml. The gateway's full env is never inherited wholesale, so a
// third-party MCP server cannot read FOCI_* socket paths or operator secrets.
func allowlistedEnv(extra []string) []string {
	env := make([]string, 0, len(mcpEnvAllowlist)+len(extra))
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if mcpEnvAllowlist[name] || strings.HasPrefix(name, "LC_") {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
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

// Tool returns a foci tool that dispatches to MCP servers, or nil if
// no servers are connected and no config dir is set for dynamic loading.
func (m *Manager) Tool() *tools.Tool {
	m.mu.Lock()
	hasDynamic := m.configDir != ""
	hasServers := len(m.servers) > 0
	m.mu.Unlock()

	if !hasServers && !hasDynamic {
		return nil
	}

	desc := "Call a tool on a connected MCP server. Re-reads mcp.toml on each call."
	if hasServers {
		m.mu.Lock()
		desc = m.buildDescription()
		m.mu.Unlock()
	}

	return &tools.Tool{
		Name:        "mcp",
		Description: desc,
		Parameters:  mcpToolSchema,
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return m.execute(ctx, params)
		},
	}
}

// refreshServers re-reads mcp.toml and reconnects if the server list changed.
// Caller must NOT hold m.mu.
func (m *Manager) refreshServers(ctx context.Context) {
	if m.configDir == "" {
		return
	}

	cfg, err := LoadConfig(m.configDir)
	if err != nil {
		mcpLog.Warnf("reload mcp.toml: %v", err)
		return
	}

	servers := cfg.ServersForAgent(m.agentID)

	m.mu.RLock()
	changed := !serverConfigsEqual(m.current, servers)
	m.mu.RUnlock()

	if !changed {
		return
	}

	mcpLog.Infof("agent %s: mcp.toml changed, reconnecting", m.agentID)

	// Close existing connections.
	_ = m.Close() // best effort cleanup before reconnecting

	if len(servers) == 0 {
		m.mu.Lock()
		m.current = nil
		m.mu.Unlock()
		return
	}

	// Connect with new config. Per-server errors are logged as warnings
	// inside connectWith; this is a best-effort fan-out.
	m.connectWith(ctx, servers, m.tf)

	m.mu.Lock()
	m.current = servers
	m.mu.Unlock()
}

// serverConfigsEqual compares two server config slices for equality.
func serverConfigsEqual(a, b []ServerConfig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Command != b[i].Command ||
			a[i].URL != b[i].URL || !stringsEqual(a[i].Args, b[i].Args) ||
			!stringsEqual(a[i].Env, b[i].Env) || !stringsEqual(a[i].Agents, b[i].Agents) {
			return false
		}
	}
	return true
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// toolExists checks if a tool with the given name exists in the list.
func toolExists(tools []*mcp.Tool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
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
// Re-reads mcp.toml before each call if configDir is set.
func (m *Manager) execute(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
	m.refreshServers(ctx)

	var p mcpParams
	if err := json.Unmarshal(params, &p); err != nil {
		return tools.ToolResult{}, fmt.Errorf("invalid mcp tool params: %w", err)
	}

	// Hold the read lock for the whole tool call. A concurrent refreshServers
	// closes sessions on an mcp.toml change via the write lock, so it now
	// drains behind any in-flight call instead of closing the session
	// mid-call; the session we resolve here can't be retired under us.
	m.mu.RLock()
	defer m.mu.RUnlock()

	var conn *serverConn
	for i := range m.servers {
		if m.servers[i].name == p.Server {
			conn = &m.servers[i]
			break
		}
	}
	if conn == nil {
		return tools.TextResult("error: unknown MCP server " + p.Server), nil
	}

	// Verify the tool exists on this server.
	if !toolExists(conn.tools, p.Tool) {
		return tools.TextResult("error: server " + p.Server + " has no tool " + p.Tool), nil
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
