# Authentication Setup

Foci authenticates with Anthropic via OAuth PKCE. A single token handles both conversations (Messages API) and usage/mana queries.

## Quick Start

```
foci auth
```

This opens an OAuth authorization URL, prompts for the code, and saves credentials to `secrets.toml`. The token auto-refreshes in the background while foci is running.

## Token Resolution Order

Foci checks credentials in this order:

1. **Foci OAuth credentials** — `oauth_access_token`, `oauth_refresh_token`, `oauth_expires_at` in `secrets.toml`. Written by `foci auth`. Auto-refreshing.
2. **Static setup-token** — `anthropic.setup_token` in `secrets.toml` or `foci.toml`. No auto-refresh.
3. **Claude Code credentials** — `~/.claude/.credentials.json`. Read-only fallback. Auto-refreshing.

The first source that succeeds is used. Startup log shows which source was selected:

```
using foci OAuth token from secrets.toml (expires in 2h 30m, auto-refresh active)
using static setup-token from secrets.toml
using Claude Code credentials from ~/.claude/.credentials.json (fallback, read-only, expires in 4h 15m)
```

## How It Works

1. **`foci auth`** generates a PKCE challenge, prints an authorization URL
2. You open the URL in a browser and authenticate with your Claude account
3. Paste the authorization code back into the terminal
4. Foci exchanges the code for access + refresh tokens and saves them to `secrets.toml`
5. On startup, foci loads credentials from `secrets.toml` and starts background token refresh

## Credentials in secrets.toml

`foci auth` writes these fields under `[anthropic]`:

```toml
[anthropic]
# Written by foci auth, auto-updated on refresh
oauth_access_token = "sk-ant-oat01-..."
oauth_refresh_token = "sk-ant-ort01-..."
oauth_expires_at = 1772334580401
```

These are managed automatically — you don't need to edit them manually.

## Claude Code Fallback

If no OAuth credentials exist in `secrets.toml` and no static setup-token is configured, foci falls back to Claude Code's credentials at `~/.claude/.credentials.json`. This is read-only — foci never writes to Claude Code's file. Token refreshes update in-memory state only.

If you already use Claude Code, foci can read its credentials as a fallback — but running `foci auth` for a dedicated token is recommended.

### `foci auth` flags

```
foci auth [--config PATH]
```

- `--config` — path to foci.toml (to find `secrets.toml` in the same directory)

## Static Setup-Token Override

For automation or when OAuth is impractical, you can use a static setup-token in `secrets.toml`:

```toml
[anthropic]
setup_token = "sk-ant-oat01-..."
```

This is checked after foci's own OAuth credentials. If `oauth_access_token` exists and is valid, it takes priority over the static token.

## Token Scopes

The OAuth flow requests these scopes:
- `org:create_api_key` — API key creation
- `user:profile` — user profile access
- `user:inference` — model inference (conversations)

## Auto-Refresh

When using OAuth credentials (either foci's own or Claude Code fallback), foci refreshes the token ~5 minutes before expiry. The refresh runs in the background — no manual intervention needed. For foci's own credentials, updated tokens are written back to `secrets.toml` via the secrets store.

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
