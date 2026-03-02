# Secrets Management

## Overview

Foci stores credentials in `secrets.toml` (alongside `foci.toml`). Secrets are never injected into the agent's message context. They are resolved at tool execution time via `{{secret:NAME}}` templates in `http_request` headers/body, and redacted from all tool output.

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

Templates are resolved before the request is sent. The secret value never appears in the agent's context — only the template string. Secret templates are **blocked in exec** — use `http_request` or the `foci_http_request` shell function (available inside exec) for any API call that needs credentials. The shell function passes `{{secret:NAME}}` as a literal string to the server for resolution, so the secret never touches the shell.

## Domain-Locked Secrets (`http_request`)

The `http_request` tool provides secure API calls with secrets that are domain-locked — each secret can only be sent to explicitly allowed hosts. Secrets without `allowed_hosts` cannot be used in `http_request` at all; the request will be rejected.

### `allowed_hosts` format

Add an `allowed_hosts` array to any section in `secrets.toml`:

```toml
[github]
token = "ghp_..."
allowed_hosts = ["api.github.com"]

[custom]
api_key = "sk-..."
allowed_hosts = ["api.example.com", "api.backup.example.com"]

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

Regular secret templates (`{{secret:NAME}}`) are **blocked in exec** — the tool returns an error. Secrets must flow through `http_request` (or the `foci_http_request` shell function inside exec), which provides domain locking, redirect blocking, and response redaction. Exec commands run arbitrary shell code, making it impossible to guarantee secrets aren't leaked via pipes, subshells, or environment variables.

**Exception:** `foci_http_request` in exec — the shell function passes `{{secret:NAME}}` as a literal string argument to the server-side http_request tool. The secret is resolved in-process with full domain locking, never exposed to the shell.

Add `allowed_hosts` to the secret's section in `secrets.toml`. Secrets without `allowed_hosts` cannot be used in `http_request`.

## Security Model

### OS-level protection (primary)

Secrets are protected at the operating system level using Unix groups:

1. **Group `foci-secrets`** — a dedicated group that owns `secrets.toml`
2. **File ownership** — `secrets.toml` is owned by `root:foci-secrets` with permissions `0660`
3. **Supplementary groups** — the systemd unit grants `SupplementaryGroups=foci-secrets` so the main foci process can read and write secrets
4. **Group dropping** — all child processes spawned by the exec tool, tmux tool, and script commands have the `foci-secrets` group removed from their supplementary group list. All other groups (e.g. `docker`, `git`, `sudo`) are preserved. The OS denies access to `secrets.toml` because the child no longer has `foci-secrets`
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

None of these bypass OS-level file permissions. The child process simply does not have the `foci-secrets` group, so `open()` returns `EACCES` regardless of how the path is specified.

## Startup Security Checks

At startup, foci verifies:
- `secrets.toml` is owned by `root` (uid 0)
- `secrets.toml` group is `foci-secrets`
- `secrets.toml` permissions are `0660`
- The process has `foci-secrets` in its supplementary groups

If any check fails, a WARN message is logged with the specific issue and a suggested fix command. Checks never prevent startup.

### Suppressing checks

Set `skip_security_checks = true` in `foci.toml` to disable startup checks (e.g. for development environments).

## Setup

### Using setup.sh

`setup.sh` handles all security setup automatically:
- Creates the `foci-secrets` group
- Adds the `foci` user to the group
- Sets `secrets.toml` ownership to `root:foci-secrets` with mode `0660`
- Configures the systemd unit with `SupplementaryGroups=foci-secrets` and `AmbientCapabilities=CAP_SETGID`

Running `setup.sh` on an existing install upgrades the security model idempotently.

### Manual setup

If not using `setup.sh`:

```bash
# Create group
sudo groupadd foci-secrets

# Add foci user to group
sudo usermod -aG foci-secrets foci

# Set file ownership and permissions
sudo chown root:foci-secrets /home/foci/config/secrets.toml
sudo chmod 0660 /home/foci/config/secrets.toml

# Update systemd unit (add to [Service] section)
# SupplementaryGroups=foci-secrets
# AmbientCapabilities=CAP_SETGID

# Reload and restart
sudo systemctl daemon-reload
sudo systemctl restart foci
```

### Verifying

After setup, check that the startup log shows no security warnings:

```bash
journalctl -u foci | grep -i security
```

You can also verify from within a session:
- Run `/secrets list` to confirm secrets are accessible
- The exec tool should not be able to read `secrets.toml` (the agent will get a permission denied error if it tries)


