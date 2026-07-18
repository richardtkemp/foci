<!-- GOLDEN: ships with foci (shared/skills/foci-development/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Routing & delivery

`internal/route` is the **single outbound addressing authority** (2026-07 routing refactor). Don't hand-roll connection resolution — go through it.

## The delivery cascade — `route.ConnFor`

```go
conn, outcome := route.ConnFor(connMgr, agentID, sessionKey, policy)
```

Resolves the delivery connection for a session through ONE cascade:

1. **The session's own live connection** (facet bot, app conversation binding, the chat embedded in the key) — every policy.
2. **Policy-dependent fallback** (only reached when the session has no live connection of its own):
   - `PolicyStrict` — stop (`DeliveryNone`, conn nil): deliver to the named session or nowhere. Never lands a message somewhere the sender didn't name.
   - `PolicyFallback` (the default) — fall back to the owning platform's **primary** connection (`DeliveredViaPrimary`).

Outcomes: `DeliveredToSession` / `DeliveredViaPrimary` / `DeliveryNone`. **conn is nil exactly for `DeliveryNone`.** Still run the agent turn where applicable (the JSONL records it) and **log the outcome** — a message that fell back or went undelivered must never look delivered.

There is **no branch-suppression policy** anymore (the old `PolicyRootFallback`/`DeliverySuppressed` were removed 2026-07): a branch/facet key with no live connection of its own falls back to primary under `PolicyFallback` just like any other session. If a caller must *not* leak into the primary chat, it passes `PolicyStrict`. Policy is set on the `route.Target` (`?policy=strict|fallback|broadcast`, default `fallback`) and carried through `Resolution.Policy` to the delivery layer; session *resolution* is policy-independent.

`route.Broadcast(cm, agentID)` returns every live connection across all platforms — the set for agent-wide notices (mana / rate-limit / max-tokens warnings) and `PolicyBroadcast` targets. The caller picks the send method per connection (`SendNotification` for notices, `SendText` for messages).

## Unsolicited / agent-initiated delivery

Agent-initiated turns (restart changelogs, scheduled wakes, proactive warnings, backgrounded async-tool results, and cross-agent `send_to_session`) share ONE primitive — **`deliverToSessionChat()`** in `cmd/foci-gw/agents_notify.go`. It runs a system-injected turn on the target session (serialised on that session's inbox worker, so it defers behind a pending `foci_ask` instead of racing platform turns) and renders the output to that session's own chat, resolving delivery via **`route.ConnFor(cm, agentID, sk, route.PolicyFallback)`**. No live connection of its own → falls back to the agent's primary chat (logged as `DeliveredViaPrimary`); nothing live anywhere → the turn still runs and lands in the JSONL, it just isn't rendered.

The one shape that is **not** a plain delivery is **`relayResponseToCaller()`** (used only by `send_to_session` with `reply_to=caller`): it runs the turn on the target session but CAPTURES its output with a `BufferSink` instead of delivering it, then relays that text to the *calling* session. Because it reads the turn's output rather than showing it, it can't collapse into `deliverToSessionChat`.

## `send_to_session` (the tool)

`foci_send_to_session` injects a user-role message into a target session; the target agent sees and responds to it. Target accepts a **full key** (`scout/c5970082313`), a **bare agent name** (`scout` → the agent's default session), an **agent-qualified name** (`scout/research`), or a **chat alias**. By default the target's reply routes back to the caller; `reply_to: "session"` sends the reply to the target session's own chat instead.
