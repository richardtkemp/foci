# Bitwarden Vault Integration

## Overview

In addition to static secrets in `secrets.toml`, Foci can dynamically access credentials stored in a Bitwarden vault via the `bw` CLI. This provides a larger, centrally-managed credential store with approval-gated access.

## How It Works

The integration uses a dedicated `bitwarden` system user and the `aisudo` privilege escalation system:

```
Agent → bitwarden_search tool → Store.Search() → cached metadata (no aisudo needed)
Agent → bitwarden_unlock tool → Store.GetPassword() → aisudo → Telegram approval → bw get password
Agent → http_request with {{secret:bw.UUID}} → Store.Resolve() → cached value (already unlocked)
```

## Two-Tier Security Model

**Tier 1: Metadata (auto-approved)**

`sudo -u bitwarden bw list items` is allowlisted in aisudo — it runs without Telegram approval. This refreshes item names, URIs, folders, and usernames. No passwords are included in this data.

Metadata is refreshed on a configurable interval (default 15 minutes). The `bitwarden_search` tool queries this cached metadata.

**Tier 2: Passwords (approval-required)**

`sudo -u bitwarden bw get password <id>` is NOT allowlisted — it goes through aisudo's Telegram approval workflow. The agent's `bitwarden_unlock` tool call blocks until:
- Dick approves on Telegram → password is fetched and cached
- Dick denies on Telegram → agent gets "unlock denied by administrator" error
- aisudo times out → agent gets a timeout error

## TTL Lifecycle

1. Agent calls `bitwarden_unlock` with an item ID
2. aisudo sends Telegram notification to Dick for approval
3. On approval, `bw get password` runs as the `bitwarden` user
4. Password is cached in memory with a TTL (default 30 minutes)
5. Subsequent `{{secret:bw.UUID}}` references resolve from cache — no re-approval needed
6. After TTL expires, a background cleanup goroutine removes the cached value
7. Next reference requires a fresh unlock (new aisudo approval)

## Template Syntax

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

## Host Restriction via URI Fields

Each Bitwarden vault item can have URI fields (e.g., `https://api.github.com`). These function like `allowed_hosts` for static secrets — the `http_request` tool validates that the target URL's host matches one of the item's URIs before sending the request.

This means a GitHub token stored in Bitwarden with URI `https://api.github.com` cannot be sent to `https://evil.com` — even if the agent tries.

Items without URIs cannot be used in `http_request` (same as static secrets without `allowed_hosts`).

## Dedicated System User

The `bw` CLI runs as a dedicated `bitwarden` system user, not root. This user:
- Has its own home directory with BW CLI config and session state
- Is created during setup: `sudo useradd --system --create-home --shell /usr/sbin/nologin bitwarden`
- Never interacts with `secrets.toml` (separate security domain)

## Configuration

```toml
[bitwarden]
enabled = true
session_file = "/home/bitwarden/.bw_session"
refresh_interval = "15m"
secret_ttl = "30m"
cleanup_interval = "1m"
```

See [CONFIG.md](CONFIG.md) for full option reference.

## Slash Commands

- `/bitwarden setup` — check prerequisites (bw CLI, bitwarden user, login status), create system user if needed
- `/bitwarden status` — show current state: enabled/disabled, item count, cache age, unlocked secrets

## Setup

1. Install the Bitwarden CLI: `npm install -g @bitwarden/cli` or [download](https://bitwarden.com/help/cli/)
2. Run `/bitwarden setup` from a Telegram session — it will create the system user and check prerequisites
3. Log in as the bitwarden user: `sudo -u bitwarden bw login`
4. Unlock the vault and save the session key to a file:
   ```bash
   sudo -u bitwarden bw unlock --raw | sudo -u bitwarden tee /home/bitwarden/.bw_session
   sudo -u bitwarden chmod 600 /home/bitwarden/.bw_session
   ```
   The session file is owned by `bitwarden:bitwarden`, mode `600` — only the bitwarden user can read it. Foci never reads this file; each `bw` command reads it fresh at execution time.
5. Set `enabled = true` in `foci.toml` and restart Foci
