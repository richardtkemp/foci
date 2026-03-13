// OAuth token management for Anthropic API authentication.
package anthropic

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	// Setup token validation constants.
	SetupTokenPrefix    = "sk-ant-oat01-"
	SetupTokenMinLength = 80
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

// ValidateSetupToken checks that a token has the expected prefix and minimum length.
func ValidateSetupToken(token string) error {
	if token == "" {
		return fmt.Errorf("token is empty")
	}
	if !strings.HasPrefix(token, SetupTokenPrefix) {
		return fmt.Errorf("expected token starting with %s", SetupTokenPrefix)
	}
	if len(token) < SetupTokenMinLength {
		return fmt.Errorf("token looks too short; paste the full setup-token")
	}
	return nil
}

// RunSetupTokenFlow runs the interactive setup-token flow: instructs the user
// to run `claude setup-token`, reads the token from stdin, validates it,
// and saves it to the secrets store as anthropic.setup_token.
func RunSetupTokenFlow(store SecretsStore) error {
	fmt.Println("Run this command in another terminal:")
	fmt.Println()
	fmt.Println("  claude setup-token")
	fmt.Println()
	fmt.Print("Paste the token: ")

	reader := bufio.NewReader(os.Stdin)
	raw, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	token := strings.TrimSpace(raw)

	if err := ValidateSetupToken(token); err != nil {
		return err
	}

	store.Set("anthropic.setup_token", token)
	if err := store.Save(); err != nil {
		return fmt.Errorf("save token: %w", err)
	}

	return nil
}
