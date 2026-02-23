# Secrets Management

## Overview

Clod stores credentials in `secrets.toml` (alongside `clod.toml`). Secrets are never injected into the agent's message context. They are resolved at tool execution time via `{{secret:NAME}}` templates and redacted from tool output.

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

Use `{{secret:section.key}}` in exec commands:

```
curl -H "Authorization: Bearer {{secret:custom.github_token}}" https://api.github.com/user
```

Templates are resolved before the command is executed. The secret value never appears in the agent's context — only the template string.

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
