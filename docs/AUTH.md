# Authentication Setup

Foci authenticates with Anthropic via OAuth PKCE. A single token handles both conversations (Messages API) and usage/mana queries — no separate admin key needed.

## Quick Start

```
foci auth
```

This opens an OAuth authorization URL, prompts for the code, and saves credentials to `~/.claude/.credentials.json`. The token auto-refreshes in the background while foci is running.

## How It Works

1. **`foci auth`** generates a PKCE challenge, prints an authorization URL
2. You open the URL in a browser and authenticate with your Claude account
3. Paste the authorization code back into the terminal
4. Foci exchanges the code for access + refresh tokens and saves them to disk
5. On startup, foci loads the credentials file and starts background token refresh

## Credentials File

Default: `~/.claude/.credentials.json`

Foci reads two formats:

**Foci-native format** (written by `foci auth`):
```json
{"access_token":"...","refresh_token":"...","expires_at":1771770729992}
```

**Claude Code format** (written by `claude` CLI):
```json
{"claudeAiOauth":{"accessToken":"...","refreshToken":"...","expiresAt":1771770729992}}
```

If you already use Claude Code, foci can use its credentials file directly — just point `credentials_file` at it.

## Configuration

In `foci.toml`:
```toml
[anthropic]
credentials_file = "~/.claude/.credentials.json"  # default
```

### `foci auth` flags

```
foci auth [--credentials-file PATH] [--config PATH]
```

- `--credentials-file` — save credentials to this path (overrides config)
- `--config` — read `credentials_file` from this foci.toml

## Static Token Override

For automation or when OAuth is impractical, you can use a static token in `secrets.toml`:

```toml
[anthropic]
token = "sk-ant-oat01-..."
```

When `anthropic.token` is set (in secrets.toml or foci.toml), foci uses it directly without OAuth. No auto-refresh occurs.

## Token Scopes

The OAuth flow requests these scopes:
- `org:create_api_key` — API key creation
- `user:profile` — user profile access
- `user:inference` — model inference (conversations)

## Auto-Refresh

When using OAuth credentials, foci refreshes the token ~5 minutes before expiry. The refresh runs in the background — no manual intervention needed. Updated credentials are atomically written to disk (temp file + rename, 0600 permissions).

## Migration from Two-Token Model

If you previously used `admin_key` or `oauth_token`, remove them from your config:

```toml
# secrets.toml — before
[anthropic]
token = "sk-ant-oat01-..."
admin_key = "sk-ant-api03-..."

# secrets.toml — after (remove both, use OAuth instead)
# Or keep just token for static override:
[anthropic]
token = "sk-ant-oat01-..."
```

Then run `foci auth` to set up OAuth credentials. The single OAuth token replaces both the conversation token and the admin key.
