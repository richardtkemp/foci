# Secrets Management

## Overview

Foci stores credentials in `secrets.toml` (alongside `foci.toml`). Secrets are never injected into the agent's message context. They are resolved at tool execution time via `{{secret:NAME}}` templates in `http_request` headers/body, and redacted from all tool output.

## Managing Secrets

### CLI

Manage secrets from the command line without a running gateway:

```bash
foci secrets list                          # list secret names (no values)
foci secrets get <section.key>             # print value to stdout
foci secrets set <section.key> <value>     # add or update
foci secrets delete <section.key>          # remove
```

Use `--config <path>` to specify a custom `foci.toml` location (secrets.toml is resolved alongside it). Default: `~/config/secrets.toml`.

The `get` subcommand prints the raw value with no decoration, so it's pipe-friendly: `foci secrets get anthropic.token | pbcopy`.

See [CLI.md](CLI.md#secrets--manage-secrets) for full details.

### Slash commands (inside agent sessions)

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

Keys use `section.key` format. The `[anthropic]`, `[telegram]`, and `[http]` sections are used by core wiring; `[custom]` is for user-defined secrets.

Each section supports optional control arrays:

```toml
[custom]
api_key = "sk-..."
other_key = "sk-other-..."
allowed_hosts = ["api.example.com"]
allowed_in_body = ["api_key"]    # only api_key can appear in request body; other_key cannot
```

`allowed_in_body` lists the key names within the section that may appear in `http_request` body, body_file, or form_fields. Secrets not listed are restricted to headers only. See [Body Restriction](#body-restricted-secrets) below.

### `http.api_key` — HTTP API authentication

All TCP HTTP endpoints (including `/voice`) require authentication via `http.api_key`. This key is **auto-generated** on first startup as a 5-word passphrase (~52 bits entropy, e.g. `maple-thunder-basket-olive-crane`) and saved to `secrets.toml`.

**Same-user auth (Unix socket):** The gateway also listens on a Unix domain socket at `~/data/foci-gw.sock` (configurable via `[http] socket_path`). Connections over this socket are authenticated by the kernel using `SO_PEERCRED` — no API key is needed. The CLI auto-discovers the socket and prefers it, so **same-user access requires no credentials at all**. This eliminates the need to store secrets in crontab or child process environments.

**TCP auth (remote/cross-user):** For remote access or connections from a different user, the API key is required. The `foci` CLI reads the key from `--api-key` flag or `FOCI_API_KEY` env var and sends it as `Authorization: Bearer <key>`.

**Crontab:** No API key needed — the CLI auto-discovers the Unix socket at `~/data/foci-gw.sock`. The gateway also injects `FOCI_GW_SOCK` (socket path, not a secret) into child process environments.

**Request format:** Either `Authorization: Bearer <key>` header or `api_key=<key>` query param (for WebSocket compat). Unix socket connections skip key validation entirely.

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

### Body-Restricted Secrets

By default, secrets resolve **only in headers**. Request body, body_file, and form_fields are blocked unless the key is explicitly listed in `allowed_in_body` for its section.

**Threat model:** When a secret is in a request body, it is visible in the third party's request logs, load balancer logs, and any middleware between foci and the target API. Headers marked `Authorization` are typically excluded from logs by convention. Body content has no such protection.

```toml
[custom]
api_key = "sk-..."
other_key = "sk-other-..."
allowed_hosts = ["api.example.com"]
allowed_in_body = ["api_key"]    # only api_key is permitted in body
```

With this config:
- `{{secret:custom.api_key}}` in headers: **allowed** (always)
- `{{secret:custom.api_key}}` in body: **allowed** (listed in `allowed_in_body`)
- `{{secret:custom.other_key}}` in body: **blocked** (not listed)
- Bitwarden secrets in body: **always blocked** (no per-item config mechanism)

**Managing via `/secrets`:**
- `/secrets body <section>` — view current allowed_in_body
- `/secrets body <section> add <key>` — allow a key in body
- `/secrets body <section> remove <key>` — revoke body permission
- `/secrets body <section> clear` — remove all body permissions

`allowed_in_body` follows the same patterns as `allowed_hosts` and supports per-agent overrides via `[agents.ID.section]` sections.

### Security guarantees

- **No shell** — secrets are resolved in-process, never passed to `sh -c`. Shell encoding attacks are impossible.
- **Host validation** — before sending, each secret's target URL is checked against `allowed_hosts`. Requests to unlisted hosts are rejected.
- **Userinfo defense** — URLs like `https://api.example.com@evil.com/steal` are detected. The tool uses `url.Parse().Hostname()` which returns `evil.com`, not `api.example.com`.
- **Redirect blocking** — when secrets are present, cross-domain redirects are blocked. A server at `api.example.com` cannot redirect to `evil.com` to capture credentials.
- **Body restriction** — secrets resolve only in headers by default. Body/body_file/form_fields require the key to be listed in `allowed_in_body`. Bitwarden secrets are never permitted in bodies.
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
3. **Process-only group grant** — the `foci-secrets` group is granted to the **foci-gw process**, never to the foci *user*. systemd uses `SupplementaryGroups=foci-secrets`, Docker uses `setpriv --groups foci-secrets`, and the no-systemd path uses `runuser --supp-group`. The foci user is deliberately **not** a member in `/etc/group` (setup.sh and the Dockerfile no longer run `usermod -aG`; setup.sh removes any pre-existing membership). This is what lets the gw read and write secrets while preventing children from re-acquiring the group via the setuid `sg`/`newgrp` tools, which consult `/etc/group`.
4. **Group dropping** — every subprocess foci-gw spawns goes through `procx.Spawn` / `procx.SpawnSetsid` (`internal/procx`), which removes the `foci-secrets` group from the child's supplementary group list while preserving all other groups (e.g. `docker`, `git`). This covers delegated Claude Code agents, exec/tmux tools, MCP servers, TTS, document conversion, the credential-refresh probe — everything. The OS denies access to `secrets.toml` because the child no longer has `foci-secrets`.
5. **CAP_SETGID, ambient-cleared** — the systemd unit grants `AmbientCapabilities=CAP_SETGID` so foci-gw can `setgroups()` to drop the group on children. Critically, after the startup probe confirms the credential works, `procx` clears the process **ambient** capability set (`PR_CAP_AMBIENT_CLEAR_ALL`). Ambient capabilities survive `execve` for non-root processes, so without this a child would inherit effective `CAP_SETGID` and could simply add `foci-secrets` back. Clearing ambient (while keeping permitted/effective on the parent) means the fork-time drop still works but the exec'd child holds no `CAP_SETGID`. The unit also sets `NoNewPrivileges=yes` (blocks setuid `sg`/`newgrp`/`sudo` for the whole tree) and `CapabilityBoundingSet=CAP_SETGID`. (Note: `NoNewPrivileges` disables the sudo-based Bitwarden aisudo integration below; this is acceptable while Bitwarden is unused — revisit the directive if it is revived.)
6. **forbidigo lint guard** — `.golangci.yml` bans raw `exec.Command` / `exec.CommandContext` repo-wide, with `internal/procx/procx.go` as the sole allowed caller. Any future code path that bypasses `procx.Spawn` fails `golangci-lint`. This prevents the security property from regressing under future refactors.

7. **Fail closed** — `procx.Setup()` returns an error if foci-gw holds the `foci-secrets` group but cannot establish the credential drop (the `CAP_SETGID` probe failed, or supplementary groups couldn't be read). Startup then **aborts** (`log.Fatalf`) rather than running on with every subprocess silently inheriting `foci-secrets`. Set `skip_security_checks = true` to override on a host where this is acceptable (the gateway then logs a loud INSECURE warning and continues). The legitimate "nothing to drop" cases — running as root, the group not existing, or the process not being a member — are not errors.

This means even if an AI agent constructs a command to read `secrets.toml` using encoding tricks, glob patterns, interpreter string construction, or any other bypass technique, the OS kernel denies access from a spawned subprocess. The two historical escapes from this boundary — ambient `CAP_SETGID` surviving `execve`, and the foci user being a permanent `foci-secrets` member (re-acquirable via `sg`/`newgrp`) — are both closed (see items 3 and 5). Note this protects *subprocesses*; in-process tools are covered separately below.

### In-process file tools

Some tools run **inside** foci-gw (the main agent's `read`/`write`/`edit`, and the `ExecExport` tools `summary`/`http_request` invoked over the exec bridge), so the OS group-drop does not apply to them — they hold the gateway's credentials. These tools must therefore enforce the boundary themselves: every file-touching path goes through `fileScope.resolveFileArg` (`internal/tools/files.go`), which canonicalises the path (resolving symlinks), enforces isolated-directory containment for sandboxed spawns, and rejects blocked paths via `Store.IsBlockedPath`. This covers `read`/`write`/`edit`, `summary`, and `http_request`'s `body_file` / `files[]` / `save_to`, so none of them can read `secrets.toml` or `/proc/self/environ` even though they execute at gateway privilege.

### Defence in depth

Several additional layers provide redundancy:

- **`Redact()`** — all tool output is scanned for secret values. Any occurrence is replaced with `[REDACTED]`. This catches accidental leaks even if a secret appears in unexpected output. Values shorter than 4 characters are not redacted to avoid false positives. Like `IsBlockedCommand`, this is **advisory/best-effort defence-in-depth, not a boundary**: it matches verbatim byte sequences, so an adversary who can encode, chunk, or otherwise transform a secret before it reaches output can evade it. The real boundaries are the OS group-drop (subprocesses) and `IsBlockedPath` (in-process file tools), which prevent the secret from being read in the first place.

- **`IsBlockedPath()`** — canonical-path check (absolute, symlink-resolved; component-aligned matching, not substring) used by the in-process file tools to refuse blocked paths such as `secrets.toml` and `/proc/self/environ`. This is an enforcement boundary for those tools (see *In-process file tools* above), not merely advisory.

- **`IsBlockedCommand()`** — a substring scan the exec tool applies to a command line referencing blocked paths. Because it inspects an unparsed string (not a resolved filesystem path) it is advisory only — it catches obvious attempts but the OS group-drop is the real boundary for subprocesses.

- **No context injection** — secrets are stored in Go structs, never in the agent's message history. The agent sees `{{secret:NAME}}` templates, not values.

- **MCP server env scrubbing** — MCP servers are third-party subprocesses, so unlike the agent's own shell (which is deliberately given `FOCI_SOCK` etc. so the `foci_*` tools work) they are **not** handed the gateway's full environment. `makeTransport` (`internal/mcp/mcp.go`) sets `cmd.Env` to a minimal allowlist (`PATH`, `HOME`, `LANG`/`LC_*`, …) plus the explicit `env` from `mcp.toml`, so a server can't read `FOCI_GW_SOCK` / `FOCI_SOCK` (the control + exec-bridge sockets) or operator vars from inherited env. Anything a server needs must be declared in its `mcp.toml` `env`.

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


