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
using API key from secrets (endpoint %q)
using CC credentials from ~/.claude/.credentials.json (endpoint %q, lazy)
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

`CCTokenSource` does lazy on-disk reads — it reads whatever Claude Code last wrote to `~/.claude/.credentials.json` at the point a request is made. There is no background refresh and no expiry margin; refreshing the OAuth token is Claude Code's job, not foci's. API keys are static and do not need refresh.

## Automated Re-login

Each Claude Code **account** has its own login. An account is a login location: `<config folder>/.credentials.json` on Linux, a Keychain item on macOS. Agents with no `CLAUDE_CONFIG_DIR` of their own share the host's account; an agent whose `backend_config.env` sets `CLAUDE_CONFIG_DIR` has its own (see [A second Claude Code account](#a-second-claude-code-account)). Two folders signed in to the same email are two accounts to foci.

When an account's OAuth credential can no longer be refreshed, its subprocess returns a 401 (`Failed to authenticate` / `Invalid authentication credentials`). Foci detects this and runs an automated re-login for that account — no human has to manually re-authenticate. The same flow also runs proactively at startup if an account's credential is already dead: the startup check probes each account in use once, with the environment and binary of the first agent on it (see the startup readiness probe in WIRING.md), so a boot with an expired token recovers without waiting for the first turn to fail.

The flow is also triggered manually with the `/login` command (ccstream backend only; see COMMANDS.md). `/login` in an agent's chat signs in that agent's account.

What you see:

1. The sign-in message arrives in the chat of the agent whose failure started the re-login — the chat that ran `/login` for the manual path, or the agent's default chat for the auto-401 path: `🔐 Sign in to Claude Code (account: <name>), then paste the code here.`, where "Sign in to Claude Code" is a link to the sign-in URL (on a chat that renders markdown; the URL is shown raw if it holds a character that would break the link). The account is named by its email once foci knows it (from the startup check), else by its folder. In the app it, and every other re-login message, is a system message.
2. You sign in via the browser and reply with the code from the page.
3. On success: `✅ Claude Code login completed (account: <name>).`

While an account's re-login is in progress, message processing for the agents on **that account** is paused; agents on other accounts run normally. It resumes once login completes, fails, or times out. A message in a chat of another agent on the same account gets the same link repeated there, and the code typed in any chat that has the link completes that login. When two accounts' logins are open at once, each chat's code goes to the login whose link that chat has. If it fails, the sign-in message changes to say the link has expired, and a separate notice gives the reason and tells you to send `/login` in the chat to try again.

The login runs an interactive `claude` in its own tmux session (`foci-cc-login` for the host account, `foci-cc-login-<hash>` for another), told the agent's `CLAUDE_CONFIG_DIR` and secure-storage folder, so the new login is written to that account and no other.

Sign-in expiry warnings (the claude.ai sign-in end date) are kept per account too: each notice names its account and goes to the master agent's chat when the master is on that account, else to the chat of an agent on it.

## A second Claude Code account

To run some agents on a different Claude account from the others, on the same host and at the same time:

1. Set `CLAUDE_CONFIG_DIR` in each such agent's `backend_config.env` to one absolute folder, e.g. `CLAUDE_CONFIG_DIR = "/home/foci/.claude-b"`. A relative path is ignored with a startup warning. Agents with the same folder share the account.
2. Restart foci. `backend_config.env` is read only at startup.
3. Do the one-time sign-in: send `/login` in one of those agents' chats (or let the startup check start it) and paste the code. Sign in with the account you want there.
4. Claude Code creates its own working files in the new folder, but not your settings. If the agent should share them, copy by hand from the host folder (`~/.claude`): `settings.json` (for example `defaultMode`, deny rules, `additionalDirectories`, `model`, plugins) and `skills/`. foci copies nothing.

Usage limits and holds are per account: one account at its limit holds only its own agents.

On macOS the agent gets its own Keychain item, named after its folder: on a separate-root install foci no longer forces the default item onto an agent that sets `CLAUDE_CONFIG_DIR` (set `CLAUDE_SECURESTORAGE_CONFIG_DIR` in its env too to choose the item yourself). The agent's existing chats do not move with it: their transcripts stay in the old folder, so they start fresh.
