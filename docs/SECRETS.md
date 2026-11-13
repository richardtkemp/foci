# Secrets Management

## Overview

Foci stores credentials in `secrets.toml` (alongside `foci.toml`). Secrets are never injected into the agent's message context. They are resolved at tool execution time via `{{secret:NAME}}` templates in `http_request` headers/body. Output redaction is best-effort and does not cover every path: on a delegated backend such as Claude Code, the output of the backend's own tools reaches the model unscrubbed. See [Where output is redacted](#where-output-is-redacted).

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

The `get` subcommand prints the raw value with no decoration, so it's pipe-friendly: `foci secrets get anthropic.api_key | pbcopy`.

See [CLI.md](CLI.md#secrets--manage-secrets) for full details.

### Slash commands (inside agent sessions)

- `/secrets list` — show all secret names (values are never displayed)
- `/secrets set section.key value` — add or update a secret
- `/secrets remove section.key` — delete a secret

### Agent requests (`foci_secrets`, #3090)

An agent cannot write `secrets.toml` and never handles a value. It asks with the `secrets` tool (`foci_secrets` in a shell):

```bash
foci_secrets list                                            # names and rules, never values; no dialog
foci_secrets set hetzner.api_key --allowed-hosts api.hetzner.cloud --allowed-agents clutch
foci_secrets remove hetzner.api_key
foci_secrets hosts hetzner add|remove <host>   |  foci_secrets hosts hetzner clear
foci_secrets body hetzner add|remove <key>     |  foci_secrets body hetzner clear
foci_secrets agents hetzner allow|deny a1,a2   |  foci_secrets agents hetzner clear
```

Every action except `list` opens a dialog in the foci app, and only there: with no app device able to show it, the request fails at once. The dialog names the asking agent and shows the section's rule before and after. For `set` the user taps Approve and then types the value in a masked field; the value goes over the app's socket straight into the store and is never logged, echoed or shown to the agent. The call returns at once; the agent receives the outcome (`applied`, `denied`, `cancelled`, `timed_out` after 30 minutes, or `failed`) as a later message. `--allowed-hosts` replaces the section's `allowed_hosts`; `--allowed-agents` sets `allowed_agents` (and removes any `denied_agents`: a section may not carry both). Omitted, each stays as it is. `list` reads the global sections, not the per-agent `[agents.<id>.*]` tables. Requests are in memory: a gateway restart drops a pending one silently.

### File format

```toml
[anthropic]
api_key = "sk-ant-..."

[telegram]
scout = "123:ABC"   # key is the agent ID (e.g. telegram.scout)

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

### Metadata keys are not secrets

`allowed_hosts`, `allowed_in_body`, `allowed_agents` and `denied_agents` configure a section. Their values are never redacted and never listed as secret names, whatever their TOML form, in global sections and in `[agents.<id>.*]` tables. `/secrets set` refuses these keys.

Write them as arrays of strings. Other forms behave as follows:

| Form | Example | Behaviour |
|---|---|---|
| String | `allowed_agents = "scout, researcher"` | Read as a comma-separated list, with a WARN in the log on each load. An empty string is an empty list, and `allowed_agents = ""` denies every agent. The next `Save` writes an array. |
| Any other type, or an array with a non-string element | `allowed_agents = 5` | The file is rejected: `foci-gw` does not start, and a reload keeps the last good contents. The error names the section and the key. |
| `allowed_agents` / `denied_agents` in an `[agents.<id>.*]` table, any form | | Ignored, with a WARN. These keys apply only to global sections. |

### `allowed_agents` / `denied_agents`

Each section can restrict which agents may use its secrets. The two fields are **mutually exclusive** — set one or the other, never both:

```toml
[github]
token = "ghp_..."
allowed_hosts = ["api.github.com"]
allowed_agents = ["scout", "researcher"]   # only these agents can use github.token

[internal]
admin_key = "sk-..."
allowed_hosts = ["internal.example.com"]
denied_agents = ["temp-bot"]               # all agents except temp-bot can use internal.admin_key
```

### `app.fcm_*` — FCM push credential

The `[app]` platform's offline wake-push (FCM v1) reads its Google service-account credential as **decomposed fields**, never the raw JSON file and never a file path — the credential content stays out of `foci.toml` and off disk entirely. Field names mirror the standard Google service-account JSON schema (public schema, not secret):

```toml
[app]
fcm_project_id     = "my-firebase-project"
fcm_client_email   = "firebase-adminsdk-xxxxx@my-firebase-project.iam.gserviceaccount.com"
fcm_private_key    = "-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----\n"
fcm_private_key_id = "abc123..."   # optional; sets the JWT "kid" header
fcm_token_uri      = "..."         # optional; defaults to Google's OAuth token endpoint
```

`fcm_project_id`, `fcm_client_email`, and `fcm_private_key` are required — if any is missing, foci falls back to `[platforms.app].fcm_credentials` (a JSON file path in `foci.toml`), then the legacy `app.fcm_credentials` secret (also a file path), then disables push. `fcm_private_key` accepts either real newlines or literal `\n` escapes (the value copied straight out of the downloaded JSON key file) — both are normalized on read.

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

For HTTP Basic auth the credentials must be base64-encoded, which the agent cannot do to a secret it never sees. Use `basic_auth` instead of an `Authorization` header: templates resolve first, then `user:password` is encoded (checked like a header secret):

```
http_request with basic_auth: "{{secret:companies_house.api_key}}:"
foci_http_request https://api.example.com/x --basic-auth '{{secret:custom.user}}:{{secret:custom.pass}}'
```

Templates are resolved before the request is sent. The secret value never appears in the agent's context — only the template string. Secret templates are **blocked in exec** — use `http_request` or the `foci_http_request` shell function (available inside exec) for any API call that needs credentials. The shell function passes `{{secret:NAME}}` as a literal string to the server for resolution, so the secret never touches the shell.

## Hot Reload (no restart)

Edits to `secrets.toml` — by hand, by `foci secrets set`, or by the `/secrets` command — take effect on the **next use**, without a restart (#1269). Every read from a secrets store checks the file first (one `os.Stat`: modification time at full resolution, size, and inode identity, so an editor that replaces the file by rename is caught too) and re-parses it when it changed. No watcher, timer or restart is involved.

A reload covers:

- **Added, changed and removed values** — a later `{{secret:NAME}}` for a removed secret fails with the usual "unknown secret" error.
- **`allowed_hosts` and `allowed_in_body`** — host and body rules follow the current file.
- **`allowed_agents` / `denied_agents` and `[agents.<id>.*]` overrides** — every agent's view re-filters against the new file. The per-agent stores the tools already hold are live views over the shared file; nothing is rebuilt or rewired.
- **`Redact`** — uses the current values, so a newly added or changed value is redacted immediately.
- **Trace scrubbing** — every exported trace field is redacted with the current values, including `[agents.<id>.*]` override values, plus every value seen since startup (a value removed from the file stays scrubbed — a rotated-out credential can still appear in later output). The tracing endpoint and the Langfuse keys still need a restart.

If the changed file cannot be read, does not parse, or fails validation (e.g. a section with both `allowed_agents` and `denied_agents`, or a metadata key of the wrong type), the store keeps the **last good contents** and logs **one** warning per file state (not one per use); it retries as soon as the file changes again. A `secrets.toml` that did not exist at startup is picked up when it is created. The secrets file path and the default blocked paths stay blocked across reloads.

In-process edits (`/secrets set` and friends) mutate the shared store directly and reach every agent on the next use. `Save` holds the store's write lock across its write, so concurrent readers never observe a half-written file from the gateway itself; an external writer without such coordination (an editor, `foci secrets set` in another process) can still produce a transient torn read, but the completed write is picked up on the next use. Mutators re-read the file before applying, so an unseen external edit is merged with — not silently overwritten by — the next in-process change.

**Still needs a restart** (read once at startup to build clients and tokens): provider API keys and clients (`anthropic.*`, OpenAI/Gemini tokens — bound into provider clients at startup), bot tokens, `brave.api_key`, voice keys, `http.api_key` (bound to the gateway's own HTTP auth at startup), and the tracing endpoint and Langfuse keys.

**The system prompt's secret list only changes on restart.** The agent's system prompt lists secret names once, at startup, so the prompt and its cache are untouched: a newly added secret works immediately, but its NAME only appears in the prompt after a restart. A resumed Claude Code session sees it only at its next compaction (`docs/CACHING.md`, #2908).

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

**Managing via `/secrets`:**
- `/secrets hosts <section>` — view current allowed_hosts
- `/secrets hosts <section> add <host>` — allow a host
- `/secrets hosts <section> remove <host>` — revoke host permission
- `/secrets hosts <section> clear` — remove all host permissions

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
- **Response redaction** — secret values in the inline response body and the header block are replaced with `[REDACTED]`, so a credential the server echoes back does not appear in the result. This covers a body of any size: the file a large body spills to is scrubbed too. A text body saved with `save_to` is scrubbed the same way. A binary body (a `save_to` of a binary content type, a decoded `save_from_json_path` data URI, or an auto-saved binary response) cannot be scrubbed without corrupting it: if it holds a secret value, the request fails, no file is written, and the error names no value (see [Where output is redacted](#where-output-is-redacted)).
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
5. **CAP_SETGID, ambient-cleared** — the systemd unit grants `AmbientCapabilities=CAP_SETGID` so foci-gw can `setgroups()` to drop the group on children. Critically, after the startup probe confirms the credential works, `procx` clears the process **ambient** capability set (`PR_CAP_AMBIENT_CLEAR_ALL`) on **every OS thread** via `syscall.AllThreadsSyscall`. Ambient capabilities survive `execve` for non-root processes, so without this a child would inherit effective `CAP_SETGID` and could simply add `foci-secrets` back. Clearing ambient (while keeping permitted/effective on the parent) means the fork-time drop still works but the exec'd child holds no `CAP_SETGID`. The all-threads part is load-bearing: capability sets are per-thread and Go forks a child from whichever thread the spawning goroutine is on, so a plain `prctl(2)` cleared only one of foci-gw's ~22 threads and nearly every child kept the cap (#2007). `AllThreadsSyscall` returns `ENOTSUP` in a cgo-linked binary, so the Makefile builds with `CGO_ENABLED=0`; a cgo build fails closed at startup (item 7). The unit also sets `NoNewPrivileges=yes` (blocks setuid `sg`/`newgrp`/`sudo` for the whole tree) and `CapabilityBoundingSet=CAP_SETGID`. (Note: `NoNewPrivileges` disables the sudo-based Bitwarden aisudo integration below; this is acceptable while Bitwarden is unused — revisit the directive if it is revived.)
6. **forbidigo lint guard** — `.golangci.yml` bans raw `exec.Command` / `exec.CommandContext` repo-wide, with `internal/procx/procx.go` as the sole allowed caller. Any future code path that bypasses `procx.Spawn` fails `golangci-lint`. This prevents the security property from regressing under future refactors.

7. **Fail closed** — `procx.Setup()` returns an error if foci-gw holds a security group (`foci-secrets` or `foci-askgw`) but cannot establish the credential drop (the `CAP_SETGID` probe failed, or supplementary groups couldn't be read). The error names the groups actually held. Startup then **aborts** (`log.Fatalf`) rather than running on with every subprocess silently inheriting them. On macOS the drop is never possible (`setgroups(2)` is root-only and there are no capabilities), so a foci-gw user in either group always hits this; subprocess isolation is not available on macOS yet. Set `skip_security_checks = true` to override on a host where this is acceptable (the gateway then logs a loud INSECURE warning and continues). The flag also turns off the `secrets.toml` ownership/permission checks and lets the SSRF guard reach loopback HTTP targets. The legitimate "nothing to drop" cases — running as root, the group not existing, or the process not being a member — are not errors.

This means even if an AI agent constructs a command to read `secrets.toml` using encoding tricks, glob patterns, interpreter string construction, or any other bypass technique, the OS kernel denies access from a spawned subprocess. The two historical escapes from this boundary — ambient `CAP_SETGID` surviving `execve`, and the foci user being a permanent `foci-secrets` member (re-acquirable via `sg`/`newgrp`) — are both closed (see items 3 and 5). Note this protects *subprocesses*; in-process tools are covered separately below.

### Config file protection (`foci.toml`, #479)

The same group keeps agents from rewriting the config. `foci.toml` is `root:foci-secrets` with mode **`0664`**: every process can read it, and only root and foci-gw (the one process holding `foci-secrets`) can write it. An agent changes a setting by asking: `foci config set <path> <value>` (skill `foci-config`) sends the owner an Approve/Deny prompt, and the gateway writes the file only on Approve.

- **Fail closed.** foci-gw refuses a config file in any other shape: at startup it exits naming the fault and the fix; a live apply after an edit applies nothing; a reload of the delegated pretool/stop rules keeps the previous rules. `foci check config`, `foci pretool` and `foci debug` still read any readable file.
- **Writes keep the shape.** Every gateway write edits the file in place (`config.WriteConfigFile`), so the inode, owner, group and mode survive. The gateway is not root, so it could never give a replaced file back to root.
- **The directory stays `foci`-owned.** An agent could rename `foci.toml` and put its own file in its place; the load-time check refuses that file, which is why the check, not the directory, is the boundary.
- **Off Linux** (macOS) there is no such group model, so the check is not applied (see [macOS](#macos)).
- `skip_security_checks = true` turns the check off, with a security warning (the test harness uses it).

`make install` and `make update` do it for you (`make protect-config`, run by `wizard` and by every deploy before the restart); the Docker entrypoint does it at each start. For a manual install, run once — and after any hand edit that replaced the file:

```bash
sudo chown root:foci-secrets /home/foci/config/foci.toml && sudo chmod 0664 /home/foci/config/foci.toml
```

A hand edit of `foci.toml` now needs `sudo` (for example `sudoedit`); editors that write a new file and rename it over the old one leave it unprotected, and the next load refuses it until you run the command above.

### In-process file tools

Some tools run **inside** foci-gw (the main agent's `read`/`write`/`edit`, and the `ExecExport` tools `summary`/`http_request`/`send_to_chat` invoked over the exec bridge), so the OS group-drop does not apply to them — they hold the gateway's credentials. They are also auto-approved: a delegated agent's `foci_*` calls never ask the user. These tools must therefore enforce the boundary themselves: every file-touching path goes through `fileScope.resolveFileArg` (`internal/tools/files.go`). This covers `read`/`write`/`edit`, `summary`, `send_to_chat`'s `file`, and `http_request`'s `body_file` / `files[]` / `save_to`.

`resolveFileArg` does three things, in order:

1. **Canonicalise.** The path is made absolute (relative paths resolve against the agent's workspace) and every symlink is resolved. For a file that does not exist yet, the deepest existing ancestor is resolved. A dangling symlink anywhere on the path is refused: a write would follow it to a target the check never saw.
2. **Confine (#3076).** A sandboxed (isolated) spawn's path must stay inside its own temp directory. Every other path must lie in an **allowed area**: the agent's workspace, `/tmp` (with the process temp dir and foci's temp root), or a directory in `tools.file_allowed_paths` (global `[tools]` or per agent; the lists add up; empty by default; applies live). A refusal names the allowed areas and that key. So an agent cannot use these tools to upload, or overwrite, a file elsewhere that the foci user can reach, such as another agent's workspace or a dotfile. The same read or write from its shell would need the user's approval.
3. **Blocklist.** The canonical path is checked with `Store.IsBlockedPath`, inside the allowed areas too, so none of the tools can read `secrets.toml` or `/proc/self/environ` even though they run at gateway privilege.

The `ask` grader (#3083) is an executable, not a file argument, but it runs at the gateway's privilege (minus the secrets group) with no approval. So it may run only from the agent's skill directories, its workspace, `/tmp` or `tools.file_allowed_paths`, checked on the canonical path when the ask is made and again just before the grader runs.

The tool then opens the canonical path, so it opens what was checked. A symlink the agent swaps in between the check and the open is not caught: the agent's own shell could race it.

### Defence in depth

Several additional layers provide redundancy:

- **`Redact()`** — tool output on the paths listed in [Where output is redacted](#where-output-is-redacted) is scanned for secret values. Any occurrence is replaced with `[REDACTED]`. This catches accidental leaks even if a secret appears in unexpected output. Values shorter than 4 characters are not redacted to avoid false positives. Like `IsBlockedCommand`, this is **advisory/best-effort defence-in-depth, not a boundary**: it matches verbatim byte sequences, so an adversary who can encode, chunk, or otherwise transform a secret before it reaches output can evade it. The real boundaries are the OS group-drop (subprocesses) and `IsBlockedPath` (in-process file tools), which prevent the secret from being read in the first place.

- **`IsBlockedPath()`** — canonical-path check (absolute, symlink-resolved; component-aligned matching, not substring) used by the in-process file tools to refuse blocked paths such as `secrets.toml` and `/proc/self/environ`. This is an enforcement boundary for those tools (see *In-process file tools* above), not merely advisory.

- **`IsBlockedCommand()`** — a substring scan the exec tool applies to a command line referencing blocked paths. Because it inspects an unparsed string (not a resolved filesystem path) it is advisory only — it catches obvious attempts but the OS group-drop is the real boundary for subprocesses.

- **No context injection** — secrets are stored in Go structs, never in the agent's message history. The agent sees `{{secret:NAME}}` templates, not values.

- **MCP server env scrubbing** — MCP servers are third-party subprocesses, so unlike the agent's own shell (which is deliberately given `FOCI_SOCK` etc. so the `foci_*` tools work) they are **not** handed the gateway's full environment. `makeTransport` (`internal/mcp/mcp.go`) sets `cmd.Env` to a minimal allowlist (`PATH`, `HOME`, `LANG`/`LC_*`, …) plus the explicit `env` from `mcp.toml`, so a server can't read `FOCI_GW_SOCK` / `FOCI_SOCK` (the control + exec-bridge sockets) or operator vars from inherited env. Anything a server needs must be declared in its `mcp.toml` `env`.

### Where output is redacted

Redaction runs on specific paths, not on all output.

**Redacted:**

| Path | What is scrubbed |
|---|---|
| API-backend agents | Each tool result and tool error, before it goes to the model (`agent.Redact`, in `internal/agent/turn_api_tools.go`). |
| `shell` tool | The inline output. The tool scrubs it itself. |
| `http_request`, on any backend | The inline body, the header block, the spill file and a text body saved with `save_to`. A binary body that holds a secret value is not saved: the request fails instead. Binary bytes are saved exactly as received otherwise. A body over the inline limit spills. Over the exec bridge, `foci-call` streams this file to stdout. A secret that the `http_max_spill_bytes` or `max_response_bytes` cap cuts in two leaves its first part in the result, inline or in the file: the scrub matches whole values only. |
| `!command` typed in chat | The command text, the output, and the file that holds the full output. |
| App tool-call display | Arguments and results in the `tool.call` frames, on any backend. This also removes generic credential shapes. It changes what the user sees, not what the model sees. |
| Exported traces | Each exported field, with the current secret values and generic credential shapes. |

Each of these scrubs secret values. Only the last two also match generic shapes such as a `ghp_` token.

**Not redacted:**

| Path | What the agent gets |
|---|---|
| A delegated backend's own tools (Claude Code `Bash` and `Read`, for example) | The raw output. No foci code in `internal/delegator` rewrites it, and no foci `PostToolUse` hook changes it. |
| `foci_*` shell functions on a delegated backend | The tool's raw result. The exec bridge has no redaction step, so only `http_request` scrubs anything, and only its own output. Tool errors pass through the bridge unscrubbed. |
| `shell` spill file | The raw output, in the file. |
| Tool-result guard file (`tool-result-*`) | The result as the tool returned it. The guard writes the file before `agent.Redact` runs. |
| Exec bridge oversize file | The tool's raw text. The bridge writes this file when a result is too large for the socket. |

An agent can read each of these files. Do not rely on redaction to keep a value from an agent on these paths. The boundaries are the OS group-drop and `IsBlockedPath`, which stop the agent reading `secrets.toml`, and domain locking, which limits where a secret can go.

### Why encoding attacks don't work

Red team testing showed that an AI agent could bypass string-based defences by:
- Hex/base64 encoding the path or filename
- Using shell glob patterns (`secret?.toml`, `s*ts.toml`)
- Constructing the path via interpreter string operations (`$'s''ecrets.toml'`)
- Reading via `/proc/self/fd/` or other indirect paths

None of these bypass OS-level file permissions. The child process simply does not have the `foci-secrets` group, so `open()` returns `EACCES` regardless of how the path is specified.

### macOS

The OS-level protection above does not exist on macOS. foci runs as the login
user (no `foci` account), a LaunchAgent cannot grant a supplementary group to
one process, and only root may call `setgroups()` to strip one from a child. So
agents run as the same user as foci and **can read `secrets.toml`**. The
in-process file tools still refuse the path ([In-process file tools](#in-process-file-tools)),
and tool output is still redacted on the [paths that redact](#where-output-is-redacted), but neither stops a Bash `cat`. Treat
secrets on a Mac install as readable by the agents.

The startup check adapts: it warns unless the file is owned by the user foci
runs as and is mode `0600`, so at least no other account can read it.

## Startup Security Checks

At startup, foci verifies:
- `secrets.toml` is owned by `root` (uid 0)
- `secrets.toml` group is `foci-secrets`
- `secrets.toml` permissions are `0660`
- The process has `foci-secrets` in its supplementary groups

If any check fails, a WARN message is logged with the specific issue and a suggested fix command. These checks never prevent startup. The `foci.toml` check does: see [Config file protection](#config-file-protection-focitoml-479).

### Suppressing checks

Set `skip_security_checks = true` in `foci.toml` to disable startup checks (e.g. for development environments).

## Setup

### Using setup.sh

`setup.sh` handles all security setup automatically:
- Creates the `foci-secrets` group
- Sets `secrets.toml` ownership to `root:foci-secrets` with mode `0660`
- Configures the systemd unit with `SupplementaryGroups=foci-secrets` and `AmbientCapabilities=CAP_SETGID`

Running `setup.sh` on an existing install upgrades the security model idempotently.

### Manual setup

If not using `setup.sh`:

```bash
# Create group
sudo groupadd foci-secrets

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


