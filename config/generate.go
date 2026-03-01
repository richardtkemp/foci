package config

import (
	"fmt"
	"strings"
)

// SetupOptions holds the inputs collected by the setup wizard.
type SetupOptions struct {
	AgentID     string   // agent identifier (e.g. "fotini")
	Model       string   // model ID (e.g. "claude-sonnet-4-6")
	SystemFiles []string // workspace-relative character file paths
	AllowedUsers []string // Telegram user IDs
}

// SecretsOptions holds credentials for secrets.toml generation.
type SecretsOptions struct {
	AgentID string // agent identifier (matches bot name in [telegram.bots.<id>])

	// Anthropic auth — one of these groups should be set
	OAuthAccessToken  string
	OAuthRefreshToken string
	OAuthExpiresAt    int64
	SetupToken        string // static API key (alternative to OAuth)

	// Telegram
	BotToken string
}

// GenerateConfig produces a minimal foci.toml containing only the values
// that differ from defaults. The output is valid TOML ready to write to disk.
func GenerateConfig(opts SetupOptions) string {
	var b strings.Builder

	// [defaults]
	if opts.Model != "" {
		b.WriteString("[defaults]\n")
		b.WriteString(fmt.Sprintf("model = %q\n", opts.Model))
		b.WriteString("\n")
	}

	// [[agents]]
	b.WriteString("[[agents]]\n")
	b.WriteString(fmt.Sprintf("id = %q\n", opts.AgentID))
	if len(opts.SystemFiles) > 0 {
		b.WriteString("system_files = [\n")
		for _, f := range opts.SystemFiles {
			b.WriteString(fmt.Sprintf("  %q,\n", f))
		}
		b.WriteString("]\n")
	}
	b.WriteString("\n")

	// [telegram]
	if len(opts.AllowedUsers) > 0 {
		b.WriteString("[telegram]\n")
		quoted := make([]string, len(opts.AllowedUsers))
		for i, u := range opts.AllowedUsers {
			quoted[i] = fmt.Sprintf("%q", u)
		}
		b.WriteString(fmt.Sprintf("allowed_users = [%s]\n", strings.Join(quoted, ", ")))
	}

	return b.String()
}

// GenerateSecrets produces a secrets.toml containing credentials.
// The output is valid TOML ready to write to disk.
func GenerateSecrets(opts SecretsOptions) string {
	var b strings.Builder

	// [anthropic]
	hasAnthropicOAuth := opts.OAuthAccessToken != "" && opts.OAuthRefreshToken != ""
	hasSetupToken := opts.SetupToken != ""

	if hasAnthropicOAuth || hasSetupToken {
		b.WriteString("[anthropic]\n")
		if hasAnthropicOAuth {
			b.WriteString(fmt.Sprintf("oauth_access_token = %q\n", opts.OAuthAccessToken))
			b.WriteString(fmt.Sprintf("oauth_refresh_token = %q\n", opts.OAuthRefreshToken))
			if opts.OAuthExpiresAt > 0 {
				b.WriteString(fmt.Sprintf("oauth_expires_at = %d\n", opts.OAuthExpiresAt))
			}
		} else {
			b.WriteString(fmt.Sprintf("setup_token = %q\n", opts.SetupToken))
		}
		b.WriteString("\n")
	}

	// [telegram.bots.<id>]
	if opts.BotToken != "" && opts.AgentID != "" {
		b.WriteString(fmt.Sprintf("[telegram.bots.%s]\n", opts.AgentID))
		b.WriteString(fmt.Sprintf("token = %q\n", opts.BotToken))
	}

	return b.String()
}
