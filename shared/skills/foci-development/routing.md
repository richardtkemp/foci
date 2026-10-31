<!-- GOLDEN: ships with foci (shared/skills/foci-development/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Routing & delivery

`internal/route` is the **single outbound addressing authority**. Do not hand-roll connection resolution.

## The delivery cascade: `route.ConnFor`

```go
conn, outcome := route.ConnFor(connMgr, agentID, sessionKey, policy)
```

1. **The session's own live connection** (facet bot, app conversation binding, the chat in the key), for every policy.
2. Only if there is none, by policy:
   - `PolicyStrict`: stop (`DeliveryNone`, conn nil). Never lands a message somewhere the sender did not name.
   - `PolicyFallback` (default): the owning platform's **primary** connection (`DeliveredViaPrimary`).

Outcomes: `DeliveredToSession` / `DeliveredViaPrimary` / `DeliveryNone`. **conn is nil exactly for `DeliveryNone`.** Still run the agent turn where it applies (the JSONL records it) and **log the outcome**: a fallen-back or undelivered message must never look delivered.

There is **no branch-suppression policy** (`PolicyRootFallback`/`DeliverySuppressed` were removed). A branch/facet key without its own connection falls back to primary under `PolicyFallback`. A caller that must not leak into the primary chat passes `PolicyStrict`. Policy is set on `route.Target` (`?policy=strict|fallback|broadcast`, default `fallback`) and carried in `Resolution.Policy` to delivery. Session *resolution* does not depend on policy.

`route.Broadcast(cm, agentID)` returns every live connection on all platforms: the set for agent-wide notices (mana / rate-limit / max-tokens) and `PolicyBroadcast`. The caller picks the method per connection (`SendNotification` for notices, `SendText` for messages).

## Unsolicited / agent-initiated delivery

Restart changelogs, scheduled wakes, proactive warnings, backgrounded async-tool results and cross-agent `send_to_session` all use ONE primitive: **`deliverToSessionChat()`** (`cmd/foci-gw/agents_notify.go`). It runs a system-injected turn on the target session, serialised on that session's inbox worker (so it waits behind a pending `foci_ask` instead of racing platform turns), and renders to the session's chat via `route.ConnFor(..., route.PolicyFallback)`. No own connection → primary chat (`DeliveredViaPrimary`). Nothing live → the turn still runs into the JSONL, unrendered.

The exception is **`relayResponseToCaller()`** (only `send_to_session` with `reply_to=caller`): it runs the turn on the target but CAPTURES the output in a `BufferSink` and relays it to the *calling* session. It cannot merge into `deliverToSessionChat`.

## `send_to_session` (the tool)

`foci_send_to_session` injects a user-role message into a target session. Target forms: full key (`scout/c5970082313`), bare agent (`scout` → default session), agent-qualified name (`scout/research`), or chat alias. The reply goes back to the caller by default; `reply_to: "session"` sends it to the target's own chat.
