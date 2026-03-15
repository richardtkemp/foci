// Package messages provides shared message-inspection utilities for working
// with provider.Message and provider.ContentBlock values.
package messages

import "foci/internal/provider"

// HasToolUse returns true if the message contains any tool_use content blocks.
func HasToolUse(msg provider.Message) bool {
	return BlocksHaveToolUse(msg.Content)
}

// BlocksHaveToolUse returns true if any content block is a tool_use.
// This is the []ContentBlock variant of HasToolUse for callers that
// work with content blocks directly (e.g. response translation).
func BlocksHaveToolUse(blocks []provider.ContentBlock) bool {
	for _, b := range blocks {
		if b.Type == "tool_use" {
			return true
		}
	}
	return false
}

// ToolUseIDs returns the IDs of all tool_use blocks in the message.
func ToolUseIDs(msg provider.Message) []string {
	var ids []string
	for _, b := range msg.Content {
		if b.Type == "tool_use" {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

// ToolResultIDs returns the tool_use_id values of all tool_result blocks
// in the message as a set for O(1) lookup.
func ToolResultIDs(msg provider.Message) map[string]bool {
	ids := make(map[string]bool)
	for _, b := range msg.Content {
		if b.Type == "tool_result" {
			ids[b.ToolUseID] = true
		}
	}
	return ids
}
