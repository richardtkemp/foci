# Webhooks

Webhooks let external systems trigger agent turns via HTTP. Each webhook has a **hook ID** that maps to a **prompt file** in the config.

## Configuration

Declare webhooks in `[system]` (global) or `[[agents]].system` (per-agent). Per-agent entries merge with global — agent keys override matching global keys, unmatched global keys are preserved.

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
| `if_active=DURATION` | Only proceed if user was active within duration (e.g. `1h`) |
| `if_inactive=DURATION` | Only proceed if user was **not** active within duration |

## Response Codes

| Code | Meaning |
|------|---------|
| 202 | Queued (async, default) |
| 200 | Sync response (with `?sync=true`) |
| 400 | Bad request (malformed path, unknown agent) |
| 404 | Unknown hook ID or prompt file not found |
| 412 | No active session — send a message to the bot first |

## Authentication

All HTTP endpoints (including webhooks) require authentication via `Authorization: Bearer <key>` header or `api_key` query param. The key is configured in `secrets.toml` as `http.api_key`.

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
