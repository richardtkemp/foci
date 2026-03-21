package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"foci/internal/messages"
	"foci/internal/provider"
)

// RepairOrphans scans all session files and repairs any that end with an
// assistant message containing tool_use blocks without a following tool_result.
// This happens when the process is killed mid-tool-call: the defer flush writes
// the assistant message but no tool_result is ever created, leaving the session
// structurally invalid for the Anthropic API.
// Returns the number of repaired sessions and any error.
func (s *Store) RepairOrphans() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	repaired := 0

	err := filepath.Walk(s.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if isArchiveFile(filepath.Base(path)) {
			return nil
		}

		// Convert file path back to session key
		rel, err := filepath.Rel(s.dir, path)
		if err != nil {
			return nil
		}
		rel = strings.TrimSuffix(rel, ".jsonl")
		key := pathToKey(rel)

		msgs, err := s.loadUnlocked(key)
		if err != nil || len(msgs) == 0 {
			return nil
		}

		last := msgs[len(msgs)-1]
		if last.Role != "assistant" {
			return nil
		}

		toolUseIDs := messages.ToolUseIDs(last)
		if len(toolUseIDs) == 0 {
			return nil
		}

		// Build synthetic tool_result + assistant ack to maintain role alternation.
		// Without the assistant ack, the next HandleMessage would append a user
		// message after this user(tool_result), creating consecutive users.
		var results []provider.ContentBlock
		for _, id := range toolUseIDs {
			results = append(results, provider.ToolResultBlock(id, "Tool call interrupted", true))
		}
		repairMsg := provider.Message{Role: "user", Content: results}
		ackMsg := provider.Message{Role: "assistant", Content: provider.TextContent("(tool call interrupted)")}

		if err := s.appendUnlocked(key, repairMsg); err != nil {
			return fmt.Errorf("repair %s: %w", key, err)
		}
		if err := s.appendUnlocked(key, ackMsg); err != nil {
			return fmt.Errorf("repair ack %s: %w", key, err)
		}
		repaired++
		return nil
	})

	if err != nil && !os.IsNotExist(err) {
		return repaired, err
	}
	return repaired, nil
}

