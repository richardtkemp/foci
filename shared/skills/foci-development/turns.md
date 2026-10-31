<!-- GOLDEN: ships with foci (shared/skills/foci-development/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Turn lifecycle — steer, ask, app gates

## Steer vs SourceUser

On a CC backend, a text-only message that arrives **while a turn is in flight** goes through `Backend.Inject(SourceSteer)` and **folds into the running turn** (`internal/agent/inbox.go`), unless that turn is a **system turn** (keepalive, reflection, cron, notification): then it waits and runs as its own turn (#2591, `DelegatedManager.SystemTurnInFlight`; WIRING "User input never folds into a system turn"). A message with **attachments** folds only if the backend is a `delegator.FoldAttachmentCarrier` that accepts them (ccstream: images only; CC drops a PDF written mid-turn). Otherwise it queues for a fresh turn (#2099).

- **`SourceSteer` (`now`)**: folds into / aborts-and-redirects the current turn.
- **`SourceUser` (`next`)**: queued; folds in at the next turn boundary.

**Ask-over-steer:** while an unpaused `foci_ask` is pending, plain text is captured as the ask's *answer* (it wins over steer). `/pause` lets text steer instead.

## `foci_ask`

Asynchronous: the tool posts the question(s) and returns at once. The agent should **end its turn**; answers arrive as a new message. Buttons always resolve it; a typed reply goes to the ask only when the session is idle.

- **Persisted** to the session index (`agent_metadata`) on every change and restored at startup (24h TTL), so a pending ask survives a restart. `store == nil` = in-memory only.
- Request IDs are colon-free and auto-namespaced by agentID.
- **Nothing is dropped, nothing lingers (#2080/#1868):** an answer to an ask foci no longer holds (expired, cancelled, lost in a restart) arrives as `[SYSTEM: LATE ANSWER …]` with the question, ask time and choice; the app deletes the dead prompt (`interactive.remove`). WIRING "Late answers and dead prompts".
- **Expiry is not a cancel (#2091):** TTL sweeps send `question.ExpiredData`, never `CancelData`, so the agent hears "your ask expired" (`expiredAskNotice`), not "the user CANCELLED". A new expiry path must do the same.
- `/pause` marks the ask paused (buttons still resolve; text no longer answers). `/resume` un-pauses.

## App vs typed ask-capture

The app answers asks with **interactive-form frames**, so typed-text capture is OFF for the app. **Both** capture paths gate on `platformName != platformApp` (`platformApp = "app"`, `internal/agent/inbox.go`):

- post-turn capture: `typedAnswerEligible` (`internal/agent/run_turn.go`)
- inbound capture: `Agent.Enqueue` (`internal/agent/inbox.go`)

A nil/unknown platform never matches `platformApp`, so it keeps the default (capture).

## Delivery tracking and redelivery (#2050)

On a `delegator.DeliveryTracker` backend (claude-code only), every user-role stdin write (turn start, follow-up, steer, post-tool nudge, pre-answer re-dispatch; not slash commands) carries a uuid and stays pending until CC acks it (`user_message_uuids` on the result records). If the process dies or closes first, inputs missing from its transcript are re-sent as `redelivery` turns with the same uuid. `/reset` drops them; shutdown persists them (`session_metadata` `cc_undelivered`) for the next start. Consumed app messages become `message.consumed` frames (the app's ✓✓); other backends never report consumption, so their app messages stay at ✓. Wiring: `docs/WIRING.md` "Input Delivery Tracking & Redelivery".

**A new place that writes user input to CC** must go through `sendFold` / `beginTrackedTurn` (ccstream `inject.go`), never `writer.SendUser` directly: an untracked write is exactly what #2050 lost.
