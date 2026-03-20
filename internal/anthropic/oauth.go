// OAuth token management for Anthropic API authentication.
package anthropic

import (
	"encoding/json"
	"fmt"
)

// SecretsStore is the subset of secrets.Store used for credential management.
type SecretsStore interface {
	Get(name string) (string, bool)
	Set(name, value string)
	Save() error
}

// OAuthCredentials holds the tokens from an OAuth flow.
type OAuthCredentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"` // unix milliseconds
}

// parseCredentials tries foci-native format first, then Claude Code format.
func parseCredentials(data []byte) (OAuthCredentials, error) {
	// Try foci-native format: {"access_token":"...","refresh_token":"...","expires_at":...}
	var native OAuthCredentials
	if err := json.Unmarshal(data, &native); err == nil && native.AccessToken != "" {
		return native, nil
	}

	// Try Claude Code format: {"claudeAiOauth":{"accessToken":"...","refreshToken":"...","expiresAt":...}}
	var claude struct {
		ClaudeAiOauth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &claude); err == nil && claude.ClaudeAiOauth.AccessToken != "" {
		return OAuthCredentials{
			AccessToken:  claude.ClaudeAiOauth.AccessToken,
			RefreshToken: claude.ClaudeAiOauth.RefreshToken,
			ExpiresAt:    claude.ClaudeAiOauth.ExpiresAt,
		}, nil
	}

	return OAuthCredentials{}, fmt.Errorf("unrecognized credentials format")
}

