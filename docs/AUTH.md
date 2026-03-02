# Authentication Setup

Foci authenticates with Anthropic using a setup token, API key, or Claude Code credentials. A single token handles both conversations (Messages API) and usage/mana queries.

## Quick Start

```
foci auth
```

This prompts you to run `claude setup-token` in another terminal, then paste the token. The token is saved to `secrets.toml`.

## Token Resolution Order

Foci checks credentials in this order:

1. **Setup token** — `anthropic.setup_token` in `secrets.toml`. Written by `foci auth`. Static (no refresh needed).
2. **API key** — `anthropic.api_key` in `secrets.toml`. Standard Anthropic API key.
3. **Claude Code credentials** — `~/.claude/.credentials.json`. Read-only fallback. Auto-refreshing.

The first source that succeeds is used. Startup log shows which source was selected:

```
using setup-token from secrets.toml
using API key from secrets.toml
using Claude Code credentials from ~/.claude/.credentials.json (fallback, read-only, expires in 4h 15m)
```

## How It Works

1. **`foci auth`** prints instructions to run `claude setup-token`
2. You run `claude setup-token` in another terminal (requires a Claude Code session)
3. Paste the token back into the foci prompt
4. Foci validates the token (prefix `sk-ant-oat01-`, minimum 80 chars) and saves it to `secrets.toml`
5. If a foci gateway is running, the new credentials are **hot-reloaded immediately** — no restart needed

## Credentials in secrets.toml

`foci auth` writes this field under `[anthropic]`:

```toml
[anthropic]
setup_token = "sk-ant-oat01-..."
```

Alternatively, use a standard API key:

```toml
[anthropic]
api_key = "sk-ant-api03-..."
```

## Claude Code Fallback

If no setup token or API key is configured, foci falls back to Claude Code's credentials at `~/.claude/.credentials.json`. This is read-only — foci never writes to Claude Code's file. Token refreshes update in-memory state only.

If you already use Claude Code, foci can read its credentials as a fallback — but running `foci auth` for a dedicated token is recommended.

### `foci auth` flags

```
foci auth [--config PATH] [--addr HOST:PORT]
```

- `--config` — path to foci.toml (to find `secrets.toml` in the same directory)
- `--addr` — gateway address for credential hot-reload notification (env: `FOCI_ADDR`, default: `127.0.0.1:18791`)

## Credential Hot-Reload

When `foci auth` saves a new token, it sends a `POST /-/reload-credentials` request to the running gateway. The gateway re-reads `secrets.toml` and swaps to the new credentials immediately — in-flight API calls complete with the old token, subsequent calls use the new one.

If the gateway isn't running, `foci auth` prints a note and the new credentials take effect on next startup.

## Auto-Refresh

When using Claude Code credentials fallback, foci refreshes the token ~5 minutes before expiry. The refresh runs in the background — no manual intervention needed. Setup tokens and API keys are static and do not need refresh.
