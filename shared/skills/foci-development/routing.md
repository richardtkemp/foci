<!-- GOLDEN: ships with foci (shared/skills/foci-development/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Routing & delivery

`internal/route` is the **single outbound addressing authority** (2026-07 routing refactor). Don't hand-roll connection resolution — go through it.

## The delivery cascade — `route.ConnFor`

```go
conn, outcome := route.ConnFor(connMgr, agentID, sessionKey, policy)
```

Resolves the delivery connection for a session through ONE cascade:

1. **The session's own live connection** (facet bot, app conversation binding, the chat embedded in the key) — every policy.
2. **Policy-dependent fallback:**
   - `PolicyStrict` — stop (`DeliveryNone`).
   - `PolicyRootFallback` — **suppress branch sessions** (delivering a branch's reply via the primary would leak it into the parent's chat → `DeliverySuppressed`, conn nil), else fall back to the owning platform's primary.
   - `PolicyFallback` — always fall back to primary.

Outcomes: `DeliveredToSession` / `DeliveredViaPrimary` / `DeliverySuppressed` / `DeliveryNone`. **conn is nil exactly for `DeliverySuppressed` and `DeliveryNone`.** Still run the agent turn where applicable (the JSONL records it) and **log the outcome** — a message that fell back or went undelivered must never look delivered.

`route.Broadcast(cm, agentID)` returns every live connection across all platforms — the set for agent-wide notices (mana / rate-limit / max-tokens warnings) and `PolicyBroadcast` targets. The caller picks the send method per connection (`SendNotification` for notices, `SendText` for messages).

## Unsolicited / agent-initiated delivery

An agent-initiated message (cron/keepalive/reflection, or a cross-session reply) to a session the platform can serve → resolve via **`route.ConnFor(cm, agentID, sk, route.PolicyRootFallback)`** then `SendText`. Do NOT use `AsyncNotifier.InjectToAgent` for this — inject makes the agent process it as a *turn* rather than just notifying the user.

`PolicyRootFallback` is the right policy for agent-initiated replies: a facet/branch's reply belongs to the facet, not the main chat, so it's suppressed rather than leaked.

## `send_to_session` (the tool)

`foci_send_to_session` injects a user-role message into a target session; the target agent sees and responds to it. Target accepts a **full key** (`scout/c5970082313`), a **bare agent name** (`scout` → the agent's default session), an **agent-qualified name** (`scout/research`), or a **chat alias**. By default the target's reply routes back to the caller; `reply_to: "session"` sends the reply to the target session's own chat instead.
