<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Debugging app (FAP) delivery gaps — "a message never reached the phone"

**Trigger:** the native app user reports missing messages — replies that were
"sent" but never appeared, or a stretch where nothing arrived. The app transport
is ack-and-replay, so a missing message is one of a few specific failures, not
random loss. Don't anchor on connection flapping (`close 1006` every ~15s is
normal mobile churn the replay design is *built* to absorb) — that's the visible
symptom, not the cause.

**First, though, confirm the sockets are usable.** Flapping is only benign when
the handshake completes; a connect that never sends `hello` delivers nothing,
and `device connected` is logged for it either way. Count them:

```
grep -c 'device connected'  ~/logs/foci.log     # sockets opened
grep -c '\[app\] hello:'    ~/logs/foci.log     # sockets that became usable
```

A large gap means the app is reaching the server and receiving nothing — look
for `WARN … consecutive app connects closed without completing the handshake`.
The `resume=N` on each `hello:` line is the discriminator between a socket that
died before `hello` (no line) and one that asked to resume nothing (`resume=0`);
both produce zero `replayTo`, so replay counts alone cannot tell them apart.
`resume=0` does NOT tell you WHY, and it is not a corrupt client DB: the app's
`onReady` sends an empty resume list both when the minimal-hello degradation is
active and when its local conversations snapshot is empty — opposite causes,
identical on the wire. Only the client's own `hello=minimal` line separates them.

**Then read the close reason**, and read it out of the code that emits it:
client-side in foci-client's `FapClient.kt` (`release()` sends `"backgrounded"`;
the superseded branch sends `"superseded"`; `closeSocket()`'s `pending.cancel()`
aborts an in-flight handshake with no close frame at all, which the server sees
as a sub-second `1006`), server-side in `internal/app/hub.go` (`i/o timeout` is
its own `pongWait`).

The part no code states: if `replay GET` lines succeed in the same seconds the
upgrades die, network and device are both exonerated — suspect the upgrade path.

## Approach — look at the durable artifact, don't derive from code

1. **Query the durable frame store** (the decisive artifact). Every server→app
   frame is persisted verbatim to `~/data/app-frames.db`:
   ```
   sqlite3 -readonly ~/data/app-frames.db \
     "SELECT conv_id, count(*), datetime(max(sent_ms)/1000,'unixepoch','localtime') \
      FROM app_frames GROUP BY conv_id ORDER BY max(sent_ms) DESC LIMIT 8;"
   ```
   Schema: `app_frames(conv_id, seq, wire, sent_ms, visible, preview)`, PK `(conv_id, seq)`.
   Then pull the window on the active conv, `visible=1`, ordered by `seq`.
2. **Read the seq column, not the clock.** A *consecutive* seq gap (e.g. 11893 →
   11894) across a several-minute wall-clock jump means **those frames were never
   persisted at all** → a sink/persistence bug (frame generated but never handed
   to the binding). A frame that *is* present in the store but the user never saw
   → a **replay/ack bug** (delivered-then-lost, or app over-acked).
3. **Cross-check foci.log** for `[app] device connected/disconnected`, and for
   the turn: `[app] OnReply ... stream_surfaced=true` means delivered; its
   ABSENCE beside a `deferred TurnComplete` means the turn had no live app sink.
4. **When the two failure classes are in play, split them with two agents** (one
   per hypothesis) — server-side self-heal vs client-side over-ack. They converge.

## "Is the server sending frame type X at all?"

Tally the frame types actually sent, rather than reasoning about whether the
producing code path runs:

```bash
sqlite3 -readonly ~/data/app-frames.db \
  "SELECT json_extract(wire,'\$.t') t, COUNT(*) FROM app_frames \
   WHERE sent_ms>(strftime('%s','now')-7200)*1000 GROUP BY t ORDER BY 2 DESC"
```

Zero rows for an expected type is a **definitive server-side break** — no deploy
and no extra instrumentation needed. This is the single query that collapses the
"server isn't sending it" vs "client isn't rendering it" ambiguity, which is
otherwise expensive to settle. Note transient frames (typing/thinking) are stored
with `visible=0` but ARE present here, so their absence is meaningful too.

## Gotchas (what surprised me)

- **App convIDs are ULIDs** (`01KWN7XZ...`), NOT the session's `cXXXXXXXX` chat
  component. Find the active one by `max(sent_ms)`. **Corollary trap:** these are
  TWO id namespaces for the SAME chat — the foci session key is `agent/c<chatID>`,
  the app frame store keys by ULID `conv_id`. A `SELECT ... WHERE conv_id='c121886…'`
  returning **zero rows does NOT mean "never delivered"** — the frames are under the
  ULID. Map between them via `chat_metadata` (`conv_id` row for the chat) in state.db,
  or just group by `agent_id` and read `preview`.
- **Delivered-but-invisible = the conversation is ARCHIVED, not a delivery bug.**
  A separate failure class from the persist/replay ones above: frames ARE persisted
  AND handed to a live socket, but the conversation is `is_archived=true` in
  `chat_metadata` → hidden from the app roster, so the human never opens it. Tell:
  `app_frames.db` shows a full, recent frame window (incl. the turn's `finalText`)
  under a ULID conv_id, yet the human says they saw nothing. Check
  `/usr/bin/sqlite3 -readonly ~/data/state.db "SELECT platform,chat_id,key,value FROM
  chat_metadata WHERE agent_id='<ag>' AND key='is_archived'"`. Root cause when it's a
  cross-agent `send_to_session`/agent-initiated send: the target had **no pinned
  `is_default` chat**, so `DefaultSessionKeyForAgentOn` resolved "via default" through
  the **activity-fallback rung** to whatever root a human touched most recently — which
  can be an archived (hidden) chat. Fixed foci **542dab21**: all default-resolution
  rungs + `DefaultChatForAgent` now exclude `is_archived`, and delivery paths mint a
  fresh visible conversation when only archived ones remain (`route.Resolver.CreateDefault`).
  If you see this pre-542dab21 behaviour, that's the mechanism.
- **A bare `strftime` comparison matches NOTHING, silently.** `strftime` returns
  TEXT and SQLite orders every INTEGER below every TEXT, so
  `sent_ms/1000 > strftime('%s','2026-...')` is unsatisfiable and reports a
  confident empty result. Arithmetic rescues it (`(strftime('%s','now')-7200)*1000`
  forces numeric context, which is why the query above works) — a bare `>`/`BETWEEN`
  does not. It also reads the string as **UTC**; BST is +1h. Filter with
  `datetime(sent_ms/1000,'unixepoch','localtime') BETWEEN ...` instead, and treat
  any zero-row filtered query as needing the UNfiltered one as its positive control.
- `convBinding.send` (hub.go:~1385) persists to the store **unconditionally**,
  then enqueues to the live socket only if `client != nil`. So "in store, not
  delivered" is a real, distinct state from "never generated".
- `convBinding` is **stable per convID** (`ensureBinding` reuses it, updates
  `.client` on reconnect). But `appConn.NewTurnSink` (conn.go:~476) captures the
  binding **once at turn-start** — if disconnected at that instant it returns a
  nil/Nop sink and the whole turn's output is silently discarded (no error).
- Replay self-heals *only* if the app's resume `ack` stays below a lost frame.
  The android dispatcher must advance `conversations.lastSeq` **after** the
  content write (content-then-watermark) — advancing before lets a crash strand
  an acked-but-unpersisted frame. (Fixed foci-client e157e23; see #1045 for the
  open server-side discard-sink half.)
- `rp.Ack` (resume) and `fromSeq` (GET /app/replay) are logged as of foci
  38b246b5 — grep `resumeConversations`/`replayTo`/`replay GET` after a deploy.
- **Absence of `turn_lifecycle` events in foci.log does NOT mean the session was
  idle.** When background sub-agents (CC Agent tool) finish, CC *autonomously*
  resumes the session to process their results, but foci opens no foci-turn for
  it — an **"orphan run"** (`handlers.go:293`, `turnActive=false`, text delivered
  via always-live SessionEvents). So foci's ccstream log shows zero turn events
  while CC is actively producing. **The authoritative artifact is the CC session
  transcript** (`~/.claude/projects/-home-foci-<agent>/<uuid>.jsonl`), not the
  ccstream turn log. `jq` it for the window (timestamps are UTC):
  `jq -rc 'select(.timestamp>="…" and .timestamp<="…")|{ts:.timestamp[11:19],t:.type,tool:([.message.content[]?|select(.type=="tool_use")|.name]|join(",")),txt:([.message.content[]?|select(.type=="text")|.text]|join(" ")|.[0:60])}' <transcript>`.
  If the transcript shows continuous assistant work but foci logged `in_flight=false`,
  you've found an orphan-run window — reflection/keepalive can inject into it and
  its silent sink swallows any user-facing reply produced there (#1047).
- **Prove the drop is POST-`se.OnText` (not a reader stall) via the global `OnAssistant` log.**
  `handlers.go:46` logs `[ccstream] OnAssistant: text_blocks=N text_bytes=B` for every
  assistant message (global tag, not session-keyed). Pull the CC transcript's text-block
  `(timestamp, length)` for the window and match them 1:1 against these log lines — an exact
  timestamp+bytes match proves foci *ingested* the block and called `se.OnText` (`handlers.go:130`,
  `se` is stable, "never nil after AttachSessionEvents"). So if the block matches a log line
  yet is absent from `app-frames.db`, the loss is entirely **downstream of `se.OnText`** — in
  the session-router/sink dispatch, NOT the stdout reader. (This killed the "reader stalled"
  hypothesis for #1068: reader was fine, tools executed, OnAssistant fired for all 11 blocks.)
- **The "silent sink" identity: `AttachSessionEvents` rebinds the SHARED session-scoped
  `se.OnText` to NopSink (#1068).** NOT the router `current`-sink — that was a wrong earlier
  framing. The real mechanism: `se` (`b.sessionEvents`) is **session-scoped, shared across all
  turns** for a session, not turn-scoped. `RunInference` calls `AttachSessionEvents` on EVERY
  turn (`turn_delegated.go:301`), binding `se.OnText`'s target to
  `sessionSink := turnevent.SinkFromContext(ts.Ctx)`. A reflection/system turn has "no sink on
  ctx" (`inbox.go:698-699`) → `SinkFromContext` returns the **NopSink singleton**
  (`turnevent/context.go`). Crucially, the reflection reaches `:301` (AttachSessionEvents)
  BEFORE its backend dispatch blocks on `autonomousActive` (`inject.go:46`). So it rebinds the
  SHARED `se.OnText` → NopSink; a **concurrent autonomous CC run shares that same `se`**, so its
  `se.OnText` now emits into NopSink → dropped silently (block 1 delivered before the reflection
  reached :301; blocks 2..N swallowed after). The reflection then blocks on `autonomousActive`
  and retries `ImmediateInject` every 5s for the whole autonomous window → drop window == the
  reflection's wait window. The fix (#1070) adopts the autonomous run as a real in-flight turn so
  the reflection is held BEFORE it reaches :301.
- **`in_flight=false` in the Inject log is a HALF-truth — don't trust it as "session idle."**
  It comes from `IsTurnInFlight()` (`inject.go:212`) which reports only `b.turnActive` (foci-opened
  turns), NOT `b.autonomousActive`. The system-inject gate `tryBeginTurn` (`inject.go:46`) checks
  `turnActive || autonomousActive` — so a reflection logging `in_flight=false` can still be correctly
  BLOCKED (and looping every 5s) by an orphan run. The retry loop is by-design gating, not the bug.
- **A follow-up message during an adopted autonomous run DUPLICATES the reply (the mirror of the
  drop above) — #1274, fixed foci `1b4e2ead`.** foci has THREE "turn active" flags that disagree
  during an autonomous run: `inb.turnActive` (inbox steer-gate, set only by the worker's dispatch
  loop `inbox.go:808`), `b.turnActive` (ccstream, ground truth of "CC mid-run"), and `a.inFlight`
  (agent counter, `markInFlight`). An adopted autonomous run (`OpenAutonomousTurn`) enters via the
  ccstream reader goroutine, so it sets the backend flag + agent counter but NOT `inb.turnActive` →
  the inbox thinks the session is IDLE. A user follow-up is then dispatched as a FRESH `RunTurn`,
  which `router.Register(its own sink)` (clobbering the autonomous run's registered sink) then
  `defer router.Clear()` on return → router empty → the rest of the run incl. the final aggregate
  `TurnComplete` falls to the late-delivery fallback and is re-delivered. Tell: intermediate blocks
  arrive as `message` frames on fresh disposable late-delivery `SessionSink`s (distinct sink
  pointers per Emit) AND a final aggregate whose length ≈ the SUM of the pieces. Fix = make the
  steer-gate consult the backend flag (`DelegatedManager.BackendTurnInFlight`, so the follow-up
  STEERS via `ImmediateInject` and folds through the already-registered sink) + `run_turn.go` skips
  Register/Clear when a turn's already in-flight for sk. **#777-safe rule: OR the BACKEND flag, NOT
  the agent counter** — `markInFlight` leads primary-written, so the counter would reopen the
  header-stripping reorder race; `b.turnActive` flips at primary-written, same instant as
  `inb.turnActive`, so it only adds the autonomous case.
- **To settle drop-vs-duplicate root cause, instrument the router Register/Clear sites and REPRODUCE,
  don't reason from archived logs.** The `RunTurn`/`OpenAutonomousTurn`/orchestrator router calls
  are the decisive seam; a DEBUG line at each, deployed, then a live repro (spawn a background
  Agent-tool subagent → fold user messages into the resulting autonomous run) shows exactly which
  call clobbers. Archived logs that predate the instrumentation structurally CAN'T show it — don't
  "refute" a hypothesis from them (I did, and was wrong). The failure needs the specific ingredient
  (a follow-up folding into an ACTIVE autonomous run); a lone quiet trigger just adopts cleanly.
- **A third failure class: the message was never EMITTED at all (model-side, upstream
  anthropics/claude-code #50597) — foci and the app are innocent.** 2026-07-16, twice in
  one evening: a long report-style reply composed mid-turn between tool calls simply never
  became a text block — it "landed in thinking" (the agent cannot tell from inside; its
  closer even referenced the unsent message). Diagnosis chain, most-decisive first:
  1. `[ccstream:<agent>-…] OnAssistant: text_blocks=N text_bytes=B` summed over the turn —
     if the whole turn shows one tiny text block, nothing longer ever existed.
  2. Frame anatomy of a delivered block in `app_frames.db`: `turn.start` → `text.delta`(s)
     → `text.end` (its `d.finalText` is the per-BLOCK payload) → `meta`. Read the MATCHED
     frame's `wire` payload — an OR-query hit at the right timestamp proved to be the
     *other* (short) message once actually read. Match the exact distinctive string of the
     specific message.
  3. The CC transcript can't help for thinking content: ALL persisted `thinking` blocks are
     stripped to `{"thinking":"","signature":…}` (len 0). `text_blocks`/`text_bytes` in the
     ccstream log is the only per-turn ground truth for "did text go out".
  Pattern/mitigation: long blocks vanish, short ones survive; heavy per-turn system-reminder
  injection correlates (unproven). For critical content have the agent use the send_to_chat
  tool path (a tool call provably executes) and verify the frame payload after. foci #1303.
- **A fourth class: subagent output rendered in the MAIN chat instead of a collapsible chit.**
  (foci #1420→#1422/#1424/#1423, 2026-07-20.) Symptom: a background/reactivated Agent-tool
  subagent's text (or a SendMessage-follow-up prompt) appears as a plain `role="agent"`/`role="user"`
  bubble in the main conversation. **Do NOT chase the server sink.go `SessionSubagentDeliverer`
  fallback first** — that was the wrong side. Go to `app_frames.db` and (a) find the exact frame,
  (b) tally the group's lifecycle:
  ```
  # (a) what KIND of frame carried the leaked text? — decode t + payload
  sqlite3 -readonly ~/data/app-frames.db "SELECT seq,datetime(sent_ms/1000,'unixepoch'),visible,
    json_extract(wire,'\$.t'), json_extract(wire,'\$.d.groupKey'), json_extract(wire,'\$.d.runIndex')
    FROM app_frames WHERE wire LIKE '%<distinctive substring>%' ORDER BY sent_ms"
  # (b) the group's full start/text/end tally — a MISSING subagent.start is the smoking gun
  sqlite3 -readonly ~/data/app-frames.db "SELECT substr(gk,1,26), SUM(t='subagent.start') starts,
    SUM(t='subagent.text') texts, SUM(t='subagent.end') ends FROM (SELECT json_extract(wire,'\$.d.groupKey') gk,
    json_extract(wire,'\$.t') t FROM app_frames WHERE conv_id='<convID>' AND json_extract(wire,'\$.t') LIKE 'subagent%')
    GROUP BY gk HAVING texts>0 ORDER BY starts"
  ```
  If `t="subagent.text"` with a valid groupKey/runIndex, the SERVER framed it correctly — the leak is
  CLIENT-side: `InboundFrameDispatcher.kt` orphans a `subagent.text`/`subagent.prompt` into an inline
  main-chat bubble when no base chit row (`db.messages().get(subagentId(groupKey))`) exists, i.e. **no
  `subagent.start` ever arrived for that group** (run-1 SubagentStart is hook-driven and drops ~7% under
  bursty dispatch — foci #1423/#1425). Block SIZE and start/end ordering are RED HERRINGS — all blocks
  for a startless group orphan; size just decides which one the human notices. Client self-heal landed
  (#1422/#1424): orphan now provisions a chit + adopts a late start.
- **Timezone when correlating transcript ↔ foci.log ↔ app_frames.** CC subagent transcripts stamp UTC
  (`…Z`); foci.log stamps BST (`+01:00`); `app_frames.sent_ms` is epoch ms. A block at `13:34:05Z` is
  `14:34:05 BST` — grep foci.log at the BST wall-clock, not the Z value. A one-hour-off grep window
  reads as "no matching log line" and sends you down a wrong path.
