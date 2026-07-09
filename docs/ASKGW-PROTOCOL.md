# askgw Protocol — Server Reference

This documents the `askgw/1` protocol from the server's (foci's) perspective: what foci receives on the socket, how it processes frames, and what it sends back. For client-side usage, see the [client protocol reference](https://github.com/richardtkemp/ai-sudo/blob/main/docs/askgw-protocol.md). For setup and configuration, see [ASKGW.md](ASKGW.md).

## Transport

Newline-delimited JSON over a Unix socket. The server listens on a single socket (default `~/data/askgw.sock`), accepting multiple concurrent connections. Each connection is independent — a per-connection registry tracks in-flight asks, keyed by composite `(connID, askID)` so two connections can reuse the same ask ID without collision.

## Frame handling

### Inbound: `ask`

The server receives an `ask` frame, validates it, resolves the target agent/session, and presents the first question to the human via `SendInteractiveMessageWithID` — the same interactive-button surface CC permission prompts use.

**Validation** (rejects with an `error` frame on failure):
- `protocol` must be `"askgw/1"`; `type` must be `"ask"`
- `id` required, must not contain `:` (platform splits button data on first `:`)
- `questions` non-empty; each has a unique `key`, non-empty `question`, at least one option
- Option labels non-empty and unique within each question

**Resolution:**
- `agent` field → agent ID → `defaultSessionKeyFor` (most recently active session for that agent). Falls back to `default_agent` from config.
- If no session resolves, the server sends an `answer` with `status: "unavailable"` and tears down the entry.

**Presentation:**
- Message IDs namespaced as `askgw-<askID>-q<idx>` so they never collide with CC permission prompts.
- For multi-question asks, questions are presented one at a time. Each answer advances to the next question. The final `answer` frame includes all responses.

**On acceptance:** the server sends an `ack` frame back to the client.

**Timeout:** per-ask `timeout_seconds` or the server's `default_timeout_seconds`. On expiry, the server cancels the prompt UI and sends an `answer` with `status: "timeout"`.

### Inbound: `cancel`

The client withdraws a pending ask. The server cancels the prompt UI (`CancelInteractiveMessage`), tears down the registry entry, and sends nothing back (no `answer` frame for cancelled asks).

### Inbound: `notify`, `error`

Tolerated from the client — silently accepted, no action taken. Only `ask` and `cancel` trigger server-side behaviour.

### Outbound: `answer`

Sent when the human responds, the timeout expires, or the human dismisses the prompt. The `status` field distinguishes the outcome:

| Status | Trigger |
|--------|---------|
| `answered` | Human selected an option (or options, for multi-select) |
| `timeout` | Per-entry timer expired before the human responded |
| `dismissed` | Human dismissed the prompt via the UI |
| `unavailable` | No active session for the target agent at presentation time |

Answers are keyed by question `key`. Single-select answers are a JSON string (the option label); multi-select answers are a JSON array.

### Outbound: `ack`

Sent immediately after the first question is successfully presented, confirming the ask was accepted and is now visible to the human.

### Outbound: `error`

Sent on validation failure. Fatal errors (`bad_protocol`, envelope `malformed`) close the connection. Non-fatal errors (`unknown_type`, `rejected`, `too_large`) return the error and continue processing.

## Connection lifecycle

On connect:
1. Peer UID checked via `SO_PEERCRED` against `allowed_uids`.
2. A `connID` is assigned; a per-connection registry entry is created.
3. Frames are processed sequentially (one `bufio.Scanner` per connection, line-delimited).

On disconnect:
- All in-flight asks for the connection are cancelled (their prompt UI torn down, timers stopped).
- No answers are delivered — the socket is gone.

## Security model

| Layer | Mechanism |
|-------|-----------|
| Socket access | Group `foci-askgw`, mode `0660`. Parent dir must not be group/world-writable. |
| UID allow-list | `SO_PEERCRED` checked against `allowed_uids` (usernames or numeric UIDs) |
| Agent isolation | `procx` strips `foci-askgw` group from child agent subprocesses — they cannot reach the socket |

Both layers must pass: the connecting process must be in the group AND have its UID in the allow-list.
