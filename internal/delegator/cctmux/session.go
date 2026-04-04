package cctmux

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ccSessionsDir is the directory where Claude Code stores active session PID files.
const ccSessionsDir = ".claude/sessions"

// ccProjectsDir is the directory where Claude Code stores per-project session data.
const ccProjectsDir = ".claude/projects"

// pidEntry is the JSON structure in ~/.claude/sessions/<pid>.json.
type pidEntry struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"`
	Kind      string `json:"kind"`
}

// sessionEntry is a single line from the Claude Code session JSONL file.
// Fields are a superset — not all are present on every entry type.
type sessionEntry struct {
	Type       string          `json:"type"`              // "user", "assistant", "system", "progress", etc.
	Subtype    string          `json:"subtype,omitempty"` // e.g. "turn_duration" for system entries
	UUID       string          `json:"uuid"`
	ParentUUID string          `json:"parentUuid"`
	Timestamp  string          `json:"timestamp"`
	SessionID  string          `json:"sessionId"`
	CWD        string          `json:"cwd,omitempty"`
	Message    *messagePayload `json:"message,omitempty"`

	// system/turn_duration fields
	DurationMs   int `json:"durationMs,omitempty"`
	MessageCount int `json:"messageCount,omitempty"`
}

// messagePayload is the message field within a session entry.
type messagePayload struct {
	Role       string          `json:"role"`
	Model      string          `json:"model,omitempty"`
	Content    json.RawMessage `json:"content"` // string for user text, array for blocks
	StopReason *string         `json:"stop_reason"`
	Usage      *usagePayload   `json:"usage,omitempty"`
}

// usagePayload holds token usage from an assistant message.
type usagePayload struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// contentBlock is a single block in assistant message content arrays.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`          // tool_use: unique ID
	Name      string          `json:"name,omitempty"`        // tool_use: tool name
	Input     json.RawMessage `json:"input,omitempty"`       // tool_use: arguments
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result: references tool_use ID
	Content   json.RawMessage `json:"content,omitempty"`     // tool_result
	IsError   bool            `json:"is_error,omitempty"`    // tool_result
}

// parseContentBlocks attempts to parse the message content as an array of blocks.
// Returns nil if content is a plain string (user text messages).
func parseContentBlocks(raw json.RawMessage) []contentBlock {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] != '[' {
		return nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return blocks
}

// discoverSessionFile finds the Claude Code session JSONL file for a given PID.
// It reads the PID entry from ~/.claude/sessions/<pid>.json and derives the
// project slug from the CWD to locate the session file.
func discoverSessionFile(pid int, workDir string) (string, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("home dir: %w", err)
	}

	pidFile := filepath.Join(home, ccSessionsDir, fmt.Sprintf("%d.json", pid))
	return discoverSessionFileFrom(pidFile, home, workDir)
}

// discoverSessionFileFrom is the testable core of discoverSessionFile.
func discoverSessionFileFrom(pidFile, homeDir, workDir string) (string, string, error) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return "", "", fmt.Errorf("read PID file %s: %w", pidFile, err)
	}

	var entry pidEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return "", "", fmt.Errorf("parse PID file: %w", err)
	}

	if entry.SessionID == "" {
		return "", "", fmt.Errorf("PID file has no sessionId")
	}

	slug := projectSlug(workDir)
	jsonlPath := filepath.Join(homeDir, ccProjectsDir, slug, entry.SessionID+".jsonl")
	return entry.SessionID, jsonlPath, nil
}

// projectSlug converts a workspace path to Claude Code's project directory name.
// e.g. "/home/rich/git/foci" → "-home-rich-git-foci"
func projectSlug(path string) string {
	return strings.ReplaceAll(path, "/", "-")
}

