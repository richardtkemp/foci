# Authentication Setup

Foci authenticates with Anthropic via OAuth PKCE. A single token handles both conversations (Messages API) and usage/mana queries.

## Quick Start

```
foci auth
```

This opens an OAuth authorization URL, prompts for the code, and saves credentials to `~/.config/foci/oauth.json`. The token auto-refreshes in the background while foci is running.

## Token Resolution Order

Foci checks credentials in this order:

1. **Foci OAuth credentials** — `credentials_file` (default `~/.config/foci/oauth.json`). Written by `foci auth`. Auto-refreshing.
2. **Static setup-token** — `anthropic.setup_token` in `secrets.toml` or `foci.toml`. No auto-refresh.
3. **Claude Code credentials** — `~/.claude/.credentials.json`. Read-only fallback. Auto-refreshing.

The first source that succeeds is used. Startup log shows which source was selected:

```
using foci OAuth token from /home/foci/.config/foci/oauth.json (expires in 2h 30m, auto-refresh active)
using static setup-token from secrets.toml
using Claude Code credentials from ~/.claude/.credentials.json (fallback, read-only, expires in 4h 15m)
```

## How It Works

1. **`foci auth`** generates a PKCE challenge, prints an authorization URL
2. You open the URL in a browser and authenticate with your Claude account
3. Paste the authorization code back into the terminal
4. Foci exchanges the code for access + refresh tokens and saves them to `~/.config/foci/oauth.json`
5. On startup, foci loads the credentials file and starts background token refresh

## Credentials File

Default: `~/.config/foci/oauth.json`

Foci reads two formats:

**Foci-native format** (written by `foci auth`):
```json
{"access_token":"...","refresh_token":"...","expires_at":1771770729992}
```

**Claude Code format** (read-only fallback from `~/.claude/.credentials.json`):
```json
{"claudeAiOauth":{"accessToken":"...","refreshToken":"...","expiresAt":1771770729992}}
```

Foci never writes to Claude Code's credentials file. If you already use Claude Code, foci can read its credentials as a fallback — but running `foci auth` for a dedicated token is recommended.

## Configuration

In `foci.toml`:
```toml
[anthropic]
credentials_file = "~/.config/foci/oauth.json"  # default
```

### `foci auth` flags

```
foci auth [--credentials-file PATH] [--config PATH]
```

- `--credentials-file` — save credentials to this path (overrides config)
- `--config` — read `credentials_file` from this foci.toml

## Static Setup-Token Override

For automation or when OAuth is impractical, you can use a static setup-token in `secrets.toml`:

```toml
[anthropic]
setup_token = "sk-ant-oat01-..."
```

This is checked after foci's own OAuth file. If `~/.config/foci/oauth.json` exists and is valid, it takes priority over the static token.

## Token Scopes

The OAuth flow requests these scopes:
- `org:create_api_key` — API key creation
- `user:profile` — user profile access
- `user:inference` — model inference (conversations)

## Auto-Refresh

When using OAuth credentials (either foci's own or Claude Code fallback), foci refreshes the token ~5 minutes before expiry. The refresh runs in the background — no manual intervention needed. Updated credentials are atomically written to disk (temp file + rename, 0600 permissions).

## Migration

If you previously used `anthropic.token` in secrets.toml, rename it to `setup_token`:

```toml
# Before
[anthropic]
token = "sk-ant-oat01-..."

# After
[anthropic]
setup_token = "sk-ant-oat01-..."
```

Or better: run `foci auth` and remove the static token entirely.
