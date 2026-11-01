# Webhooks

Webhooks let external systems trigger agent turns via HTTP. Each webhook has a **hook ID** that maps to a **prompt file** in the config.

## Configuration

Declare webhooks in `[system]` (global) or `[[agents]].system` (per-agent). Per-agent entries merge with global — an agent's entry for a hook ID replaces the global entry for that ID whole (no field-level merge), unmatched global keys are preserved.

Each entry is either a prompt path string, or a table with a `prompt` and an optional `session` setting:

| `session` value | Each call runs in |
|-----------------|-------------------|
| *(absent)* or `persistent` | The per-hook independent session `agent/i<hookID>` (default; same as the string form) |
| `new-per-call` | A fresh independent session, never shared with another call |
| any other value | That fixed session, resolved exactly like a `?session=` value |

```toml
[system.webhooks]
deploy = "deploy.md"                                         # persistent
build  = { prompt = "build.md", session = "new-per-call" }   # fresh session per call
pager  = { prompt = "pager.md", session = "oncall" }         # always agent/ioncall
```

A table without `prompt`, an empty `prompt`, an empty `session`, or an unknown sub-key fails config load with an error that names the hook. Changes apply live, like the rest of the webhooks map.

```toml
[system]
webhooks = { new_commit = "new_commit.md", deploy = "deploy.md" }

[[agents]]
id = "scout"
[agents.system]
webhooks = { alert = "alert-handler.md" }
# Result: scout has { new_commit = "new_commit.md", deploy = "deploy.md", alert = "alert-handler.md" }
```

Hook IDs must not contain path separators (`/` or `\`).

## Prompt Resolution

The mapped prompt path is resolved using `prompts.ResolvePrompt`:

1. **Bare filename** (e.g. `"deploy.md"`) — searched in `{workspace}/prompts/` then `{shared}/prompts/`
2. **Absolute path** (e.g. `"/home/foci/prompts/deploy.md"`) — read directly

## URL Format

```
POST /webhook/{agent}/{hookid}
```

- `{agent}` — agent ID (e.g. `scout`)
- `{hookid}` — must match a key in the agent's webhooks config

## Request Body

The request body is the webhook payload (max 1 MB). It's appended to the prompt under a `## Webhook Payload` heading. An empty body sends just the prompt text.

## Query Parameters

| Param | Description |
|-------|-------------|
| `sync=true` | Wait for agent response (default: async 202) |
| `session=<key>` | Route the webhook to a specific session. Overrides the hook's `session` setting. The value always names a session, even when it reads like a keyword (`?session=new-per-call` targets `agent/inew-per-call`) |
| `new_session=true` | Run this call in a fresh independent session, on any hook, whatever its `session` setting. A separate parameter, so it never collides with a session name. 400 if combined with `session=` |
| `if_active=DURATION` | **Session-level**: skip unless the agent's session ran a turn within duration (e.g. `1h`). A turn currently in flight always counts as active. |
| `if_inactive=DURATION` | **Session-level**: skip if the session ran a turn within duration. In-flight always counts. Keepalive shape. |
| `if_user_active=DURATION` | **User-attention**: skip unless the user touched this agent within duration. CLI/cron/agent-to-agent does not count. |
| `if_user_inactive=DURATION` | **User-attention**: skip if the user touched this agent within duration. |

See [SPEC.md](SPEC.md) Activity gating for the distinction between the two domains and the in-flight short-circuit semantics.

## Session Routing

The session is chosen in this order:

1. A non-empty `?session=<key>` — resolved like `/send`'s session selector.
2. `?new_session=true` — a fresh independent session for this call.
3. The hook's `session` setting — a fixed session, or `new-per-call`.
4. Default (`persistent`): the hook's own independent session keyed `agent/i<hookID>` (e.g., `scout/ideploy` for the `deploy` hook), created lazily and revived by every later call.

Every default is separate from the user's interactive chat — webhook turns do not pollute the user's conversation. A fresh session is an independent root keyed `agent/i<hookID>-<unix-nanos>-<random hex>` (session type `independent`); the response receipt's `session` field names it.

**Activity gates on a fresh session.** The gates (`if_active`, `if_inactive`, `if_user_*`) evaluate against the resolved session. A fresh session (`new-per-call` or `?new_session=true`) has no history, so `if_active` always skips and `if_inactive` always runs. To gate a per-call hook on recent activity, gate on a session that has history instead (for example call with `?session=<that session>`), or use the `if_user_*` gates.

**Retention.** Fresh sessions are not archived or deleted automatically; a busy `new-per-call` hook creates one session per call.

## Response Codes

| Code | Meaning |
|------|---------|
| 202 | Queued (async, default) |
| 200 | Sync response (with `?sync=true`) |
| 400 | Bad request (malformed path, unknown agent, bad `new_session` value, or `new_session=true` with `session=`) |
| 404 | Unknown hook ID or prompt file not found |
| 412 | No active session — send a message to the bot first |

## Authentication

Webhooks authenticate via the `Authorization: Bearer <key>` header only. The `api_key` query parameter is honored ONLY on `/voice` (for WebSocket compatibility) and is explicitly ignored everywhere else, including all webhook endpoints. The key is configured in `secrets.toml` as `http.api_key`.

## Examples

### Async webhook (fire-and-forget)

```bash
curl -X POST http://localhost:18791/webhook/scout/deploy \
	-H "Authorization: Bearer $FOCI_API_KEY" \
	-d '{"repo": "foci", "branch": "main", "sha": "abc123"}'
```

### Sync webhook (wait for response)

```bash
curl -X POST "http://localhost:18791/webhook/scout/deploy?sync=true" \
	-H "Authorization: Bearer $FOCI_API_KEY" \
	-d '{"repo": "foci", "branch": "main", "sha": "abc123"}'
```

### Activity-gated (only when idle)

```bash
curl -X POST "http://localhost:18791/webhook/scout/alert?if_inactive=30m" \
	-H "Authorization: Bearer $FOCI_API_KEY" \
	-d 'Disk usage at 90%'
```
