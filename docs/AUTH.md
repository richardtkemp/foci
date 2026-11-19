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

Each Claude Code **account** has its own login. An account is a login location: `<config folder>/.credentials.json` on Linux, a Keychain item on macOS. Agents whose `backend_config` lists no `accounts` share the host's account; an agent that lists a config folder there is on that folder's account (see [A second Claude Code account](#a-second-claude-code-account)). Each session of an agent is on one account, fixed when the session is created; its branches, forks and spawns are on the same account. Two folders signed in to the same email are two accounts to foci.

When an account's OAuth credential can no longer be refreshed, its subprocess returns a 401 (`Failed to authenticate` / `Invalid authentication credentials`). Foci detects this and runs an automated re-login for that account — no human has to manually re-authenticate. The same flow also runs proactively at startup if an account's credential is already dead: the startup check probes each account in use once, with the environment and binary of the first agent on it (see the startup readiness probe in WIRING.md), so a boot with an expired token recovers without waiting for the first turn to fail.

The flow is also triggered manually with the `/login` command (ccstream backend only; see COMMANDS.md). `/login` in a chat signs in the account of that chat's session.

What you see:

1. The sign-in message arrives in the chat of the agent whose failure started the re-login — the chat that ran `/login` for the manual path, or the agent's default chat for the auto-401 path: `🔐 Sign in to Claude Code (account: <name>), then paste the code here.`, where "Sign in to Claude Code" is a link to the sign-in URL (on a chat that renders markdown; the URL is shown raw if it holds a character that would break the link). The account is named by its email once foci knows it (from the startup check), else by its folder. In the app it, and every other re-login message, is a system message.
2. You sign in via the browser and reply with the code from the page.
3. On success: `✅ Claude Code login completed (account: <name>).`

While an account's re-login is in progress, message processing for the agents on **that account** is paused; agents on other accounts run normally. It resumes once login completes, fails, or times out. A message in a chat of another agent on the same account gets the same link repeated there, and the code typed in any chat that has the link completes that login. When two accounts' logins are open at once, each chat's code goes to the login whose link that chat has. If it fails, the sign-in message changes to say the link has expired, and a separate notice gives the reason and tells you to send `/login` in the chat to try again.

The login runs an interactive `claude` in its own tmux session (`foci-cc-login` for the host account, `foci-cc-login-<hash>` for another), told the account's config folder (as `CLAUDE_CONFIG_DIR`) and secure-storage folder, so the new login is written to that account and no other.

Sign-in expiry warnings (the claude.ai sign-in end date) are kept per account too: each notice names its account and goes to the master agent's chat when the master is on that account, else to the chat of an agent on it.

## A second Claude Code account

To run some agents on a different Claude account from the others, on the same host and at the same time:

1. List one absolute Claude Code config folder in each such agent's `backend_config.accounts`, e.g. `accounts = ["/home/foci/.claude-b"]`. foci starts that agent's Claude Code with the folder as its `CLAUDE_CONFIG_DIR`. A relative path is ignored with a startup warning, and a folder listed twice is used once. Agents with the same folder share the account. An agent may list several folders: see [Several accounts for one agent](#several-accounts-for-one-agent).
2. Restart foci. `backend_config.accounts` is read only at startup.
3. Do the one-time sign-in: send `/login` in one of those agents' chats (or let the startup check start it) and paste the code. Sign in with the account you want there.
4. Claude Code creates its own working files in the new folder, but not your settings. If the agent should share them, copy by hand from the host folder (`~/.claude`): `settings.json` (for example `defaultMode`, deny rules, `additionalDirectories`, `model`, plugins) and `skills/`. foci copies nothing.

Usage limits and holds are per account: one account at its limit holds only its own agents.

On macOS the account gets its own Keychain item, named after its folder: on a separate-root install foci does not force the default item onto an account with a folder of its own. The agent's existing chats do not move with it: their transcripts stay in the old folder, so they start fresh.

## Several accounts for one agent

An agent may list several folders, one per account (#3186): `accounts = ["/home/foci/.claude", "/home/foci/.claude-b"]`. Each **new** chat or session takes the account with the most usage left, and keeps it for its whole life, resumes and restarts included; its branches, forks and spawns are on the same account. A session never moves to another account (it is for load balancing).

How the account of a new session is chosen:

1. An account that needs a new login is passed over while another one works. When every account needs one, the first in the list is taken and its login starts as for any session.
2. An account on a usage hold or stop is passed over while another is open. When every account is held, the one whose hold lifts first is taken.
3. Of the open accounts, the least used wins: an account's use is its fullest usage window (5-hour or 7-day), from the last figures its sessions reported (no fresh read). An account with no figure yet counts as unused. A tie goes to the first in the list.

Each account keeps its own login, sign-in expiry warnings, usage hold, rate-limit gate and usage figures: a hold on one account holds only the sessions on it, and its start and end notices go only to the chats whose session is on it (the default chat and the open app chats). `/usage`, `/mana`, `/login` and `/limit_reset` in a chat act on that chat's account.

**The host's account in the list.** To keep the login an agent has today and add another, list the host's Claude Code folder first. It is the host's account (no new login): foci starts its sessions exactly as an agent with no list. The host's folder is the one in the startup log line `Claude Code runs with CLAUDE_CONFIG_DIR=...` on a separate-root install (`<install root>/.claude`, e.g. `/Users/me/foci/.claude`), else `~/.claude`. On macOS any other folder, `~/.claude` on a separate-root install included, is an account of its own: Claude Code names its Keychain item after that folder, so it needs its own sign-in.

**Adding accounts to an agent that has chats.** A chat made before the agent had a list is recorded at startup on the FIRST account in the list, so list the folder the agent used until now first. A chat stored on an account that is no longer in the list starts fresh on the chosen account, and its chat is told once.

`backend_config.accounts` is the only way to give an agent an account (#3186). `CLAUDE_CONFIG_DIR` and `CLAUDE_SECURESTORAGE_CONFIG_DIR` in `backend_config.env` are ignored, with one startup warning that says what to write instead. To move an agent configured the old way, replace

```toml
[agents.backend_config]
env = { CLAUDE_CONFIG_DIR = "/home/foci/.claude-b" }
```

with

```toml
[agents.backend_config]
accounts = ["/home/foci/.claude-b"]
```

The folder, its login and its transcripts stay as they are, so the agent's chats resume.
