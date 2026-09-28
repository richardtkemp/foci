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
- **Nothing is dropped, nothing lingers (#2080/#1868):** an answer to an ask foci no longer holds (expired, cancelled, dropped across a restart) is delivered as a `[SYSTEM: LATE ANSWER …]` message naming the question text, when it was asked and the choice; the dead prompt is deleted from the app (`interactive.remove`), never shown as "expired". The question comes from the app's stored `interactive` frame — see WIRING "Late answers and dead prompts".
- **Expiry is not a cancel (#2091):** the TTL sweeps feed the ask layer `question.ExpiredData`, never `CancelData`, so the agent hears "your ask expired" (`expiredAskNotice`), not "the user CANCELLED". A new expiry path must do the same.
- `/pause` marks the pending ask paused (buttons still resolve; plain text no longer answers it); `/resume` un-pauses.

## App vs typed ask-capture

The app answers asks via **interactive-form frames**, not typed text — so typed-text ask-capture must be gated OFF for the app platform. **Both** capture paths gate on `platformName != platformApp` (`platformApp = "app"`):

- `internal/agent/run_turn.go:150` (post-turn capture)
- `internal/agent/inbox.go:482` (inbound capture)

Telegram/Discord capture typed answers; the app does not. A nil/unknown platform never matches `platformApp`, so the default (capture) is preserved when the source can't be resolved.

## Delivery tracking and redelivery (#2050)

On a backend implementing `delegator.DeliveryTracker` (claude-code only), every user-role stdin write — turn-starting message, follow-up, steer, post-tool nudge, pre-answer re-dispatch; not slash commands — carries a uuid and stays pending until CC acks it (`user_message_uuids` on the API response's records). If the process dies or is closed first, inputs its transcript does not hold are re-sent as fresh `redelivery` turns under the same uuid; `/reset` drops them, a shutdown persists them (`session_metadata` `cc_undelivered`) for the next start. Consumed app messages become `message.consumed` frames (the app's ✓✓); other backends never report consumption, so their app messages stay at ✓. Full wiring: `docs/WIRING.md` "Input Delivery Tracking & Redelivery".

**Adding a new place that writes user input to CC?** Route it through `sendFold` / `beginTrackedTurn` (ccstream `inject.go`), never `writer.SendUser` directly — an untracked write is exactly the message #2050 lost.
