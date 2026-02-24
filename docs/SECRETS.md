# Secrets Management

## Overview

Clod stores credentials in `secrets.toml` (alongside `clod.toml`). Secrets are never injected into the agent's message context. They are resolved at tool execution time via `{{secret:NAME}}` templates in `http_request` headers/body, and redacted from all tool output.

## Managing Secrets

### Slash commands

- `/secrets list` — show all secret names (values are never displayed)
- `/secrets set section.key value` — add or update a secret
- `/secrets remove section.key` — delete a secret

### File format

```toml
[anthropic]
token = "sk-ant-..."

[telegram]
bot_token = "123:ABC"

[custom]
github_token = "ghp_..."
openrouter_key = "sk-or-v1-..."
```

Keys use `section.key` format. The `[anthropic]` and `[telegram]` sections are used by core wiring; `[custom]` is for user-defined secrets.

### Referencing secrets

Use `{{secret:section.key}}` in `http_request` headers/body:

```
http_request with headers: {"Authorization": "Bearer {{secret:custom.github_token}}"}
```

Templates are resolved before the request is sent. The secret value never appears in the agent's context — only the template string. Secret templates are **blocked in exec** — use `http_request` for any API call that needs credentials.

## Domain-Locked Secrets (`http_request`)

The `http_request` tool provides secure API calls with secrets that are domain-locked — each secret can only be sent to explicitly allowed hosts.

### `allowed_hosts` format

Add an `allowed_hosts` array to any section in `secrets.toml`:

```toml
[github]
token = "ghp_..."
allowed_hosts = ["api.github.com"]

[custom]
api_key = "sk-..."
allowed_hosts = ["api.example.com", "api.backup.example.com"]

[legacy]
old_key = "val"
# No allowed_hosts — can only be used in exec (deprecated)
```

### Agent usage

The agent uses `http_request` to make API calls with secrets:

```json
{
  "url": "https://api.github.com/user",
  "method": "GET",
  "headers": {
    "Authorization": "Bearer {{secret:github.token}}"
  }
}
```

### Security guarantees

- **No shell** — secrets are resolved in-process, never passed to `sh -c`. Shell encoding attacks are impossible.
- **Host validation** — before sending, each secret's target URL is checked against `allowed_hosts`. Requests to unlisted hosts are rejected.
- **Userinfo defense** — URLs like `https://api.example.com@evil.com/steal` are detected. The tool uses `url.Parse().Hostname()` which returns `evil.com`, not `api.example.com`.
- **Redirect blocking** — when secrets are present, cross-domain redirects are blocked. A server at `api.example.com` cannot redirect to `evil.com` to capture credentials.
- **Response redaction** — secret values in the response body are replaced with `[REDACTED]`, preventing the agent from seeing raw credentials echoed back.
- **Case-insensitive host matching** — per RFC 4343, host comparison is case-insensitive.

### Why not exec?

Regular secret templates (`{{secret:NAME}}`) are **blocked in exec** — the tool returns an error. Secrets must flow through `http_request`, which provides domain locking, redirect blocking, and response redaction. Exec commands run arbitrary shell code, making it impossible to guarantee secrets aren't leaked via pipes, subshells, or environment variables.

**Exception:** Bitwarden templates (`{{secret:bw.UUID}}`) are allowed in exec because they're approval-gated via aisudo — the user must explicitly approve each password fetch via Telegram.

Add `allowed_hosts` to the secret's section in `secrets.toml`. Secrets without `allowed_hosts` cannot be used in `http_request`.

## Security Model

### OS-level protection (primary)

Secrets are protected at the operating system level using Unix groups:

1. **Group `clod-secrets`** — a dedicated group that owns `secrets.toml`
2. **File ownership** — `secrets.toml` is owned by `root:clod-secrets` with permissions `0660`
3. **Supplementary groups** — the systemd unit grants `SupplementaryGroups=clod-secrets` so the main clod process can read and write secrets
4. **Group dropping** — all child processes spawned by the exec tool, tmux tool, and script commands have the `clod-secrets` group removed from their supplementary group list. All other groups (e.g. `docker`, `git`, `sudo`) are preserved. The OS denies access to `secrets.toml` because the child no longer has `clod-secrets`
5. **CAP_SETGID** — the systemd unit grants `AmbientCapabilities=CAP_SETGID` so the process can call `setgroups()` to drop groups on child processes

This means even if an AI agent constructs a command to read `secrets.toml` using encoding tricks, glob patterns, interpreter string construction, or any other bypass technique, the OS kernel denies access. The protection is not bypassable from userspace.

### Defence in depth

Several additional layers provide redundancy:

- **`Redact()`** — all tool output is scanned for secret values. Any occurrence is replaced with `[REDACTED]`. This catches accidental leaks even if a secret appears in unexpected output. Values shorter than 4 characters are not redacted to avoid false positives.

- **`IsBlockedCommand()` / `IsBlockedPath()`** — the exec tool rejects commands that reference blocked paths (including `secrets.toml` and `/proc/self/environ`). This is a string-match check that catches obvious attempts but is not the primary protection.

- **No context injection** — secrets are stored in Go structs, never in the agent's message history. The agent sees `{{secret:NAME}}` templates, not values.

### Why encoding attacks don't work

Red team testing showed that an AI agent could bypass string-based defences by:
- Hex/base64 encoding the path or filename
- Using shell glob patterns (`secret?.toml`, `s*ts.toml`)
- Constructing the path via interpreter string operations (`$'s''ecrets.toml'`)
- Reading via `/proc/self/fd/` or other indirect paths

None of these bypass OS-level file permissions. The child process simply does not have the `clod-secrets` group, so `open()` returns `EACCES` regardless of how the path is specified.

## Startup Security Checks

At startup, clod verifies:
- `secrets.toml` is owned by `root` (uid 0)
- `secrets.toml` group is `clod-secrets`
- `secrets.toml` permissions are `0660`
- The process has `clod-secrets` in its supplementary groups

If any check fails, a WARN message is logged with the specific issue and a suggested fix command. Checks never prevent startup.

### Suppressing checks

Set `skip_security_checks = true` in `clod.toml` to disable startup checks (e.g. for development environments).

## Setup

### Using setup.sh

`setup.sh` handles all security setup automatically:
- Creates the `clod-secrets` group
- Adds the `clod` user to the group
- Sets `secrets.toml` ownership to `root:clod-secrets` with mode `0660`
- Configures the systemd unit with `SupplementaryGroups=clod-secrets` and `AmbientCapabilities=CAP_SETGID`

Running `setup.sh` on an existing install upgrades the security model idempotently.

### Manual setup

If not using `setup.sh`:

```bash
# Create group
sudo groupadd clod-secrets

# Add clod user to group
sudo usermod -aG clod-secrets clod

# Set file ownership and permissions
sudo chown root:clod-secrets /home/clod/config/secrets.toml
sudo chmod 0660 /home/clod/config/secrets.toml

# Update systemd unit (add to [Service] section)
# SupplementaryGroups=clod-secrets
# AmbientCapabilities=CAP_SETGID

# Reload and restart
sudo systemctl daemon-reload
sudo systemctl restart clod
```

### Verifying

After setup, check that the startup log shows no security warnings:

```bash
journalctl -u clod | grep -i security
```

You can also verify from within a session:
- Run `/secrets list` to confirm secrets are accessible
- The exec tool should not be able to read `secrets.toml` (the agent will get a permission denied error if it tries)

## Bitwarden Vault Integration

### Overview

In addition to static secrets in `secrets.toml`, Clod can dynamically access credentials stored in a Bitwarden vault via the `bw` CLI. This provides a larger, centrally-managed credential store with approval-gated access.

### How It Works

The integration uses a dedicated `bitwarden` system user and the `aisudo` privilege escalation system:

```
Agent → bitwarden_search tool → Store.Search() → cached metadata (no aisudo needed)
Agent → bitwarden_unlock tool → Store.GetPassword() → aisudo → Telegram approval → bw get password
Agent → http_request with {{secret:bw.UUID}} → Store.Resolve() → cached value (already unlocked)
```

### Two-Tier Security Model

**Tier 1: Metadata (auto-approved)**

`sudo -u bitwarden bw list items` is allowlisted in aisudo — it runs without Telegram approval. This refreshes item names, URIs, folders, and usernames. No passwords are included in this data.

Metadata is refreshed on a configurable interval (default 15 minutes). The `bitwarden_search` tool queries this cached metadata.

**Tier 2: Passwords (approval-required)**

`sudo -u bitwarden bw get password <id>` is NOT allowlisted — it goes through aisudo's Telegram approval workflow. The agent's `bitwarden_unlock` tool call blocks until:
- Dick approves on Telegram → password is fetched and cached
- Dick denies on Telegram → agent gets "unlock denied by administrator" error
- aisudo times out → agent gets a timeout error

### TTL Lifecycle

1. Agent calls `bitwarden_unlock` with an item ID
2. aisudo sends Telegram notification to Dick for approval
3. On approval, `bw get password` runs as the `bitwarden` user
4. Password is cached in memory with a TTL (default 30 minutes)
5. Subsequent `{{secret:bw.UUID}}` references resolve from cache — no re-approval needed
6. After TTL expires, a background cleanup goroutine removes the cached value
7. Next reference requires a fresh unlock (new aisudo approval)

### Template Syntax

```
{{secret:bw.ITEM_UUID}}
```

Examples:
```json
{
  "url": "https://api.github.com/user",
  "headers": {
    "Authorization": "Bearer {{secret:bw.abc12345-6789-def0-1234-567890abcdef}}"
  }
}
```

The `bw.` prefix distinguishes bitwarden references from static secrets (`custom.key`). Both can be used in the same request.

### Host Restriction via URI Fields

Each Bitwarden vault item can have URI fields (e.g., `https://api.github.com`). These function like `allowed_hosts` for static secrets — the `http_request` tool validates that the target URL's host matches one of the item's URIs before sending the request.

This means a GitHub token stored in Bitwarden with URI `https://api.github.com` cannot be sent to `https://evil.com` — even if the agent tries.

Items without URIs cannot be used in `http_request` (same as static secrets without `allowed_hosts`).

### Dedicated System User

The `bw` CLI runs as a dedicated `bitwarden` system user, not root. This user:
- Has its own home directory with BW CLI config and session state
- Is created during setup: `sudo useradd --system --create-home --shell /usr/sbin/nologin bitwarden`
- Never interacts with `secrets.toml` (separate security domain)

### Configuration

```toml
[bitwarden]
enabled = true
session_file = "/home/bitwarden/.bw_session"
refresh_interval = "15m"
secret_ttl = "30m"
cleanup_interval = "1m"
```

See [CONFIG.md](CONFIG.md) for full option reference.

### Slash Commands

- `/bitwarden setup` — check prerequisites (bw CLI, bitwarden user, login status), create system user if needed
- `/bitwarden status` — show current state: enabled/disabled, item count, cache age, unlocked secrets

### Setup

1. Install the Bitwarden CLI: `npm install -g @bitwarden/cli` or [download](https://bitwarden.com/help/cli/)
2. Run `/bitwarden setup` from a Telegram session — it will create the system user and check prerequisites
3. Log in as the bitwarden user: `sudo -u bitwarden bw login`
4. Unlock the vault and save the session key to a file:
   ```bash
   sudo -u bitwarden bw unlock --raw | sudo -u bitwarden tee /home/bitwarden/.bw_session
   sudo -u bitwarden chmod 600 /home/bitwarden/.bw_session
   ```
   The session file is owned by `bitwarden:bitwarden`, mode `600` — only the bitwarden user can read it. Clod never reads this file; each `bw` command reads it fresh at execution time.
5. Set `enabled = true` in `clod.toml` and restart Clod
