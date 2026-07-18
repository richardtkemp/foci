<!-- GOLDEN: ships with foci (shared/skills/foci-development/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Turn lifecycle — steer, ask, app gates

## Steer vs SourceUser

A text-only message arriving **while a turn is in flight** on a CC backend routes via `Backend.Inject(SourceSteer)` — it **folds into the running turn** rather than starting a new one (`internal/agent/inbox.go`). Contrast:

- **`SourceSteer` (`now`)** — folds into / aborts-and-redirects the in-progress turn immediately.
- **`SourceUser` (`next`)** — queued; folds in at the next turn boundary.

**Ask-over-steer:** when an unpaused `foci_ask` is pending, a plain-text reply is captured as the *answer* to the ask (it wins over steer). `/pause` is the escape hatch to let text steer instead.

## `foci_ask`

Asynchronous: the tool posts the question(s) and returns immediately — the agent should **end its turn**; answers arrive later as a new message. Buttons always resolve it; a typed reply routes to the ask only when the session is idle.

- **Persisted** to the session index (`agent_metadata`) on every change and restored on startup (24h TTL) — so a pending ask survives a restart and its message stays addressable for cancel/expiry. `store == nil` disables persistence (in-memory only).
- Request IDs are colon-free and auto-namespaced by agentID.
- `/pause` marks the pending ask paused (buttons still resolve; plain text no longer answers it); `/resume` un-pauses.

## App vs typed ask-capture

The app answers asks via **interactive-form frames**, not typed text — so typed-text ask-capture must be gated OFF for the app platform. **Both** capture paths gate on `platformName != platformApp` (`platformApp = "app"`):

- `internal/agent/run_turn.go:150` (post-turn capture)
- `internal/agent/inbox.go:482` (inbound capture)

Telegram/Discord capture typed answers; the app does not. A nil/unknown platform never matches `platformApp`, so the default (capture) is preserved when the source can't be resolved.
