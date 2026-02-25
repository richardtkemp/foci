package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"clod/secrets/bitwarden"
)

// NewBitwardenSearchTool creates a tool that searches cached Bitwarden vault metadata.
// Never returns secret values — only item names, IDs, URIs, and usernames.
func NewBitwardenSearchTool(store *bitwarden.Store) *Tool {
	return &Tool{
		Strict:      true,
		Name:        "bitwarden_search",
		Description: "Search Bitwarden vault items by name, URI, folder, or username. Returns metadata only (never passwords). Use bitwarden_unlock to unlock a specific item for use in http_request.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "Search term (substring, case-insensitive)"
				}
			},
			"required": ["query"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return "", fmt.Errorf("parse params: %w", err)
			}
			if p.Query == "" {
				return "", fmt.Errorf("query is required")
			}

			results := store.Search(p.Query)
			if len(results) == 0 {
				return "No matching items found.", nil
			}

			var sb strings.Builder
			fmt.Fprintf(&sb, "Found %d item(s):\n", len(results))
			for _, item := range results {
				fmt.Fprintf(&sb, "\n- **%s** (ID: %s)", item.Name, item.ID)
				if item.Username != "" {
					fmt.Fprintf(&sb, "\n  Username: %s", item.Username)
				}
				if len(item.URIs) > 0 {
					fmt.Fprintf(&sb, "\n  URIs: %s", strings.Join(item.URIs, ", "))
				}
				if item.Folder != "" {
					fmt.Fprintf(&sb, "\n  Folder: %s", item.Folder)
				}
			}
			return sb.String(), nil
		},
	}
}

// NewBitwardenUnlockTool creates a tool that unlocks a Bitwarden vault item.
// The unlock call goes through aisudo, which requires Telegram approval.
// On success, the password is cached for the configured TTL.
// The tool NEVER returns the actual password value.
func NewBitwardenUnlockTool(store *bitwarden.Store) *Tool {
	return &Tool{
		Strict:      true,
		Name:        "bitwarden_unlock",
		Description: "Unlock a Bitwarden vault item by ID. Requires administrator approval via Telegram. Once unlocked, use {{secret:bw.ID}} in http_request headers/body. The password value is never returned — only a confirmation that it's available.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {
					"type": "string",
					"description": "Bitwarden vault item ID (from bitwarden_search results)"
				}
			},
			"required": ["id"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return "", fmt.Errorf("parse params: %w", err)
			}
			if p.ID == "" {
				return "", fmt.Errorf("id is required")
			}

			// Look up item metadata for the response message
			item := store.ItemByID(p.ID)
			itemName := p.ID
			if item != nil {
				itemName = item.Name
			}

			// This call blocks until aisudo approval or denial
			_, err := store.GetPassword(p.ID)
			if err != nil {
				return "", err
			}

			// Never return the actual value
			var sb strings.Builder
			fmt.Fprintf(&sb, "Unlocked item %q (%s).", itemName, p.ID)
			fmt.Fprintf(&sb, "\n\nUse {{secret:bw.%s}} in http_request headers or body to reference this credential.", p.ID)
			if item != nil && len(item.URIs) > 0 {
				fmt.Fprintf(&sb, "\nAllowed hosts (from vault URIs): %s", strings.Join(item.URIs, ", "))
			}
			return sb.String(), nil
		},
	}
}
