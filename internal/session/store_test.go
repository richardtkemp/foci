package session

import (
	"foci/internal/provider"
)

func msg(role, text string) provider.Message {
	return provider.Message{
		Role:    role,
		Content: provider.TextContent(text),
	}
}

func toolUseMsg(ids ...string) provider.Message {
	var blocks []provider.ContentBlock
	for _, id := range ids {
		blocks = append(blocks, provider.ContentBlock{
			Type:  "tool_use",
			ID:    id,
			Name:  "shell",
			Input: []byte(`{"command":"ls"}`),
		})
	}
	return provider.Message{Role: "assistant", Content: blocks}
}
