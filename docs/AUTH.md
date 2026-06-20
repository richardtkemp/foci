# Authentication Setup

Foci authenticates with LLM providers using API keys or Claude Code credentials. The setup wizard (`foci first-run`) configures credentials for your chosen provider.

## Quick Start

```
foci auth --provider anthropic --api-key sk-ant-...
```

Or interactively:
```
foci auth
```

This prompts for a provider and API key. The key is saved to `secrets.toml` and hot-reloaded if a gateway is running.

## Supported Providers

| Provider | Secret Key | Endpoint |
|----------|-----------|----------|
| Anthropic | `anthropic.api_key` | `anthropic` (auto) |
| Google Gemini | `gemini.api_key` | `gemini` (auto) |
| OpenAI | `openai.api_key` | `openai` (auto) |
| OpenRouter | `openrouter.api_key` | `openrouter` (needs explicit `endpoint` in model config) |
| Custom | `<name>.api_key` | User-defined |

## Credential Resolution (Anthropic)

For the Anthropic endpoint, foci checks credentials in this order:

1. **API key** — `anthropic.api_key` in `secrets.toml`. Set via `foci auth` or `foci first-run`.
2. **Claude Code credentials** — `~/.claude/.credentials.json`. Read-only fallback. Auto-refreshing.

The first source that succeeds is used. Startup log shows which source was selected:

```
using API key from secrets.toml
using Claude Code credentials from ~/.claude/.credentials.json (fallback, read-only, expires in 4h 15m)
```

Other providers (Gemini, OpenAI, OpenRouter) use API keys only — no fallback mechanism.

## Credentials in secrets.toml

```toml
[anthropic]
api_key = "sk-ant-api03-..."

[gemini]
api_key = "AI..."

[openai]
api_key = "sk-..."

[openrouter]
api_key = "sk-or-..."
```

## Claude Code Fallback (Anthropic only)

If no Anthropic API key is configured, foci falls back to Claude Code's credentials at `~/.claude/.credentials.json`. This is read-only — foci never writes to Claude Code's file. When the OAuth token expires beyond refresh, the subprocess returns a 401; foci detects this and starts an automated re-login (see [Automated Re-login](#automated-re-login)).

### `foci auth` flags

```
foci auth [--config PATH] [--addr HOST:PORT] [--provider NAME] [--api-key KEY]
```

- `--provider` — provider name: `anthropic`, `gemini`, `openai`, `openrouter` (default: `anthropic`)
- `--api-key` — API key (prompted interactively if omitted)
- `--config` — path to foci.toml (to find `secrets.toml` in the same directory)
- `--addr` — gateway address for credential hot-reload notification (env: `FOCI_ADDR`, default: `127.0.0.1:18791`)

## Credential Hot-Reload

When `foci auth` saves a new key, it sends a `POST /-/reload-credentials` request to the running gateway. The gateway re-reads `secrets.toml` and swaps to the new credentials immediately — in-flight API calls complete with the old token, subsequent calls use the new one.

If the gateway isn't running, `foci auth` prints a note and the new credentials take effect on next startup.

## Auto-Refresh

When using Claude Code credentials fallback, foci refreshes the token ~5 minutes before expiry. The refresh runs in the background — no manual intervention needed. API keys are static and do not need refresh.

## Automated Re-login

When the shared Claude Code OAuth credential can no longer be refreshed, the subprocess returns a 401 (`Failed to authenticate` / `Invalid authentication credentials`). Foci detects this and runs an automated re-login that re-authenticates the Claude Code OAuth credentials — no human has to run `claude /login` on the host.

The flow is also triggered manually with the `/login` command (ccstream backend only; see COMMANDS.md), useful for exercising it without waiting for a real token expiry.

What you see:

1. The URL message arrives in the chat that triggered the re-login — the chat that ran `/login` for the manual path, or the agent's default chat for the auto-401 path: `🔐 Sign in to re-authenticate Claude Code, then paste the code back to me:` followed by the sign-in URL.
2. You sign in via the browser and reply with the code from the page.
3. On success: `✅ Login completed.`

While a re-login is in progress, message processing for delegated agents is paused; it resumes once login completes, fails, or times out. If it fails, foci tells you to run `claude /login` on the host to recover.
