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

### Inbound: `notify`

Reports an out-of-band update about a previously **answered** ask — e.g. `aisudo`'s socket backend sends one after it finishes executing whatever the human approved (`update_completion_status`, "Command completed: exit 0"). There is no reply frame; `notify` is fire-and-forget from the client's perspective.

**Fields:** `id` (required — correlates to the original `ask`'s `id`), `status` (free-form outcome label), `exit_code` (integer; distinguishes success/failure when present), `message` (free-form text). At least one of `status`/`exit_code`/`message` is expected, though none are required by the decoder itself.

**Rendering:** the server looks up the answered-ask's chat message (recorded when the human's answer was sent — see Registry's `recordAnswered`) and renders the notify onto it:
- **Primary: edit in place.** The message the human answered is edited to show the completion status — a checkmark/cross plus `completed, exit N` when `exit_code` is present (`✅ completed, exit 0` / `❌ completed, exit 1`), else a status line built from `status`, plus `message` appended if given.
- **Fallback: standalone message.** If the platform message ID wasn't captured (e.g. the connection only supports the plain-text button fallback) or the edit itself fails, the same text is sent as a new message to the session's chat instead.
- **Unknown/expired `id`:** a notify referencing an ask that was never answered (wrong id, never asked, or the answer aged out — answered-ask info is retained for 15 minutes, mirroring the HTTP transport's own abandoned-answer TTL) is logged and dropped. There's no reply frame to report that back through.

This lights up for **both** transports at once: the Unix-socket path acts on an inbound `notify` frame the same way regardless of which transport originally submitted the `ask` (socket or HTTP — see HTTP transport below for a remote client that can't hold a socket connection open to send one).

### Inbound: `error`

Tolerated from the client — silently accepted, no action taken.

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

## HTTP transport

An opt-in second transport (`[askgw] enabled = true` **and** `[askgw] http_enabled = true`) exposes the same ask machinery over foci's existing HTTP server, for a remote caller (e.g. a Mac running `aisudo`) that can reach foci over the network but not its local Unix socket. It is a different front door onto the *same* `AskRouter`/registry the Unix socket uses — not a parallel implementation — and it reuses the HTTP server's existing `http.api_key` bearer-token auth (no new auth scheme). See [ASKGW.md](ASKGW.md) for the config flag and [WIRING.md](WIRING.md)'s Ask Gateway section for the full design-fork rationale (why poll, not a single held-open call).

**Transport shape:** request/response, not persistent-duplex — a decision can take minutes, so submission and result-collection are two separate calls:

1. **`POST /askgw/ask`** — body is the identical `ask` frame JSON the socket accepts. Returns immediately (202, `{"id","status":"pending"}`) once the question is presented to chat — this is the HTTP equivalent of the socket's `ack`, it does **not** wait for a human answer.
2. **`GET /askgw/ask/{id}?wait=<seconds>`** — polls for the result. The server holds the request open for up to `wait` (default 20s, capped at 25s — comfortably inside the HTTP server's 30s read/write timeout) waiting for a human answer; if none arrives in time it returns `{"status":"pending"}` and the caller re-issues the same GET to keep waiting. This is a bounded, **resumable** long-poll: if the connection drops mid-wait, nothing is lost — just GET the same `id` again. Once resolved, it returns the same `answer` frame shape the socket sends (`status`: `answered`/`timeout`/`dismissed`/`unavailable`), plus an HTTP-only `cancelled` status for an ask withdrawn via (3). A 404 means `id` is not currently trackable — never submitted, already collected by an earlier terminal poll, or aged out (an answered-but-never-polled ask is evicted after 15 minutes).
3. **`POST /askgw/ask/{id}/cancel`** — withdraws a pending ask, same effect as the socket's `cancel` frame (tears down the chat prompt). Unlike the socket transport (which sends no frame back for a cancel), an in-flight poll on that `id` unblocks immediately with `cancelled` rather than waiting out the timeout.
4. **`POST /askgw/notify`** — fire-and-forget HTTP counterpart to the socket's `notify` frame (see "Inbound: `notify`" above). Body is the same `notify` frame JSON (`protocol`/`type`/`id`/`status`/`exit_code`/`message`); unlike `/askgw/ask` this endpoint carries **no id/poll bookkeeping of its own** — it goes straight to rendering against the answered ask `id` refers to (over *either* transport — an ask submitted over the socket can be notified over HTTP and vice versa, since both share the same `Registry`). Returns 202 `{"id","status":"accepted"}` once the frame itself is validated and accepted, or 400 `{"id","code","error"}` if the envelope is malformed/wrong-type — it does **not** report whether a matching answered ask was found, since there's nothing more specific to tell the caller (an unknown/expired `id` is logged server-side and dropped, same as the socket path).

**Example (bash, mirrors the socket example above):**

```bash
API_KEY=...        # http.api_key
BASE=https://foci-host:PORT

id=deploy-002
curl -s -X POST "$BASE/askgw/ask" \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d "{\"protocol\":\"askgw/1\",\"type\":\"ask\",\"id\":\"$id\",\"source\":\"deploy-bot\",\"questions\":[{\"key\":\"go\",\"question\":\"Deploy v1.2.3?\",\"options\":[{\"label\":\"Yes\"},{\"label\":\"No\"}]}]}"

# Poll until it's no longer "pending" (each call waits up to ~20s server-side).
while :; do
  resp=$(curl -s -H "Authorization: Bearer $API_KEY" "$BASE/askgw/ask/$id?wait=20")
  status=$(echo "$resp" | jq -r .status)
  [ "$status" != "pending" ] && break
done
echo "$resp" | jq -r '.status'

# After acting on the answer, report completion — fire-and-forget, no poll.
curl -s -X POST "$BASE/askgw/notify" \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d "{\"protocol\":\"askgw/1\",\"type\":\"notify\",\"id\":\"$id\",\"exit_code\":0}"
```

**What's unchanged from the socket transport:** validation rules, frame/answer shapes, `askgw-<askID>-q<idx>` message-ID namespacing, multi-question flow, and per-ask timeout resolution (`timeout_seconds` or `default_timeout_seconds`) all pass through the identical `AskFrame.Validate()`/registry/present code path — the HTTP layer only adds the id-keyed submit/poll/cancel bookkeeping needed to bridge request/response HTTP onto it.

**What's new/different:** each HTTP-submitted ask gets its own private registry connection (HTTP has no persistent connection to reuse across asks, unlike a socket connection which can host many concurrent asks); an id must be unique among *currently pending* HTTP asks (a second `POST /askgw/ask` reusing a still-pending id gets 409 `duplicate_id`); and an answered ask that nobody polls is retained (not delivered) for 15 minutes before eviction, since there is no persistent listener to push it to.
