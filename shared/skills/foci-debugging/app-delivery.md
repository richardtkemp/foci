<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Debugging app (FAP) delivery gaps — "a message never reached the phone"

The app transport is ack-and-replay, so a missing message is one of a few specific failures. Do not anchor on connection flapping: `close 1006` every ~15s is normal mobile churn that replay absorbs.

## First: are the sockets usable?

Flapping is benign only when the handshake completes. A connect that never sends `hello` delivers nothing, and `device connected` is logged either way:

```
grep -c 'device connected'  ~/logs/foci.log     # sockets opened
grep -c '\[app\] hello:'    ~/logs/foci.log     # sockets that became usable
```

A large gap = the app reaches the server and gets nothing; look for `WARN … consecutive app connects closed without completing the handshake`. `resume=N` on each `hello:` line separates a socket that died before `hello` (no line) from one that resumed nothing (`resume=0`); both give zero `replayTo`. `resume=0` is not a corrupt client DB: the app's `onReady` sends an empty list both under minimal-hello degradation and with an empty local snapshot. Only the client's own `hello=minimal` line separates them.

**Read the close reason from the code that emits it.** Client, foci-client `FapClient.kt`: `release()` sends `"backgrounded"`, the superseded branch sends `"superseded"`, `closeSocket()`'s `pending.cancel()` aborts a handshake with no close frame (server sees a sub-second `1006`). Server, `internal/app/hub.go`: `i/o timeout` is its own `pongWait`. If `replay GET` lines succeed in the same seconds the upgrades die, network and device are exonerated: suspect the upgrade path.

## Approach: read the durable artifact, do not derive from code

1. **Query the frame store.** Every server→app frame is persisted verbatim to `~/data/app-frames.db`, `app_frames(conv_id, seq, wire, sent_ms, visible, preview)`, PK `(conv_id, seq)`:
   ```
   sqlite3 -readonly ~/data/app-frames.db \
     "SELECT conv_id, count(*), datetime(max(sent_ms)/1000,'unixepoch','localtime') \
      FROM app_frames GROUP BY conv_id ORDER BY max(sent_ms) DESC LIMIT 8;"
   ```
   Then pull the window on the active conv, `visible=1`, ordered by `seq`.
2. **Read seq, not the clock.** Consecutive seqs across a long wall-clock jump = the frames were **never persisted** → sink/persistence bug. A frame in the store that the user never saw → **replay/ack bug**.
3. **Cross-check foci.log:** `[app] device connected/disconnected`; `[app] OnReply ... stream_surfaced=true` = delivered; its ABSENCE beside `deferred TurnComplete` = the turn had no live app sink.
4. With both classes in play, test server-side self-heal and client-side over-ack as separate hypotheses.

**"Is the server sending frame type X at all?"** Tally, do not reason about code paths:

```bash
sqlite3 -readonly ~/data/app-frames.db \
  "SELECT json_extract(wire,'\$.t') t, COUNT(*) FROM app_frames \
   WHERE sent_ms>(strftime('%s','now')-7200)*1000 GROUP BY t ORDER BY 2 DESC"
```

Zero rows for an expected type = a definitive server-side break. Transient frames (typing/thinking) are stored with `visible=0`, so their absence is meaningful too.

## Gotchas

- **App convIDs are ULIDs** (`01KWN7XZ...`), not the `c<chatID>` of the session key. `WHERE conv_id='c121886…'` returning zero rows does NOT mean "never delivered". Map them through `chat_metadata` (`conv_id` row) in state.db, or find the active conv by `max(sent_ms)` and read `preview`.
- **Delivered-but-invisible = the conversation is ARCHIVED.** Frames are persisted and sent, but `chat_metadata` `is_archived=true` hides the conv from the app roster. Check: `/usr/bin/sqlite3 -readonly ~/data/state.db "SELECT platform,chat_id,key,value FROM chat_metadata WHERE agent_id='<ag>' AND key='is_archived'"`. Old mechanism (fixed 7f837286): an agent-initiated send to a target with no pinned `is_default` chat resolved through the activity-fallback rung to an archived chat. Default resolution now excludes archived chats, and delivery mints a fresh conversation when only archived ones remain (`route.Resolver.CreateDefault`).
- **A bare `strftime` comparison matches NOTHING, silently.** `strftime` returns TEXT, and SQLite orders every INTEGER below every TEXT, so `sent_ms/1000 > strftime('%s','2026-...')` is always false. Arithmetic forces numeric context (`(strftime('%s','now')-7200)*1000`). It also reads the string as UTC. Prefer `datetime(sent_ms/1000,'unixepoch','localtime') BETWEEN ...`, and run the unfiltered query as a positive control for any zero.
- `convBinding.send` (`hub.go`) persists **unconditionally**, then enqueues to the socket only if `client != nil`. "In store, not delivered" is a real state.
- `convBinding` is stable per convID (`ensureBinding` updates `.client` on reconnect), but `appConn.NewTurnSink` (`conn.go`) captures the binding **once at turn start**: if disconnected then, it returns a Nop sink and the whole turn's output is discarded with no error (#1045).
- Replay self-heals only if the app's resume `ack` stays below the lost frame. The Android dispatcher must advance `conversations.lastSeq` **after** the content write (fixed foci-client e157e23). `rp.Ack` and `fromSeq` are logged: grep `resumeConversations`/`replayTo`/`replay GET`.
- **Timezones:** CC transcripts stamp UTC (`…Z`), foci.log local time (BST `+01:00`), `app_frames.sent_ms` epoch ms. A one-hour-off grep window reads as "no matching line".

## Autonomous ("orphan") CC runs

When background subagents (CC Agent tool) finish, CC **resumes the session on its own** to process the results. Foci opens no turn for it unless it adopts it (`OpenAutonomousTurn`): text goes out via the always-live SessionEvents. So:

- **No `turn_lifecycle` events in foci.log does NOT mean idle.** The authoritative artifact is the CC transcript (`~/.claude/projects/-home-foci-<agent>/<uuid>.jsonl`, UTC):
  `jq -rc 'select(.timestamp>="…" and .timestamp<="…")|{ts:.timestamp[11:19],t:.type,tool:([.message.content[]?|select(.type=="tool_use")|.name]|join(",")),txt:([.message.content[]?|select(.type=="text")|.text]|join(" ")|.[0:60])}' <transcript>`
- **`in_flight=false` in the Inject log is a half-truth.** `IsTurnInFlight()` (`inject.go`) reports only `b.turnActive`, not `b.autonomousActive`. The system-inject gate `tryBeginTurn` checks both, so a reflection that logs `in_flight=false` and retries every 5s is correctly blocked, not the bug.
- **Foci has THREE "turn active" flags that disagree during an autonomous run:** `inb.turnActive` (inbox steer gate), `b.turnActive` (ccstream, ground truth "CC mid-run"), `a.inFlight` (agent counter, `markInFlight`). An adopted run sets the last two, not the inbox flag. When you change a steer/in-flight gate, **OR the BACKEND flag (`DelegatedManager.BackendTurnInFlight`), NOT the agent counter**: the counter leads at primary-written and reopens the #777 header-stripping reorder race.
- **Past failure shapes** (both fixed; the fingerprints still identify a regression):
  - *Drop* (#1068, fixed #1070): `AttachSessionEvents` rebinds the session-scoped, shared `se.OnText`; a reflection turn with no sink on ctx rebound it to the NopSink singleton, so a concurrent autonomous run's text vanished. Fingerprint: block 1 delivered, blocks 2..N missing, drop window = the reflection's wait window.
  - *Duplicate* (#1274, fixed 4ff79251): a follow-up during an adopted run started a fresh `RunTurn` whose `router.Register`/`Clear` clobbered the run's sink, so the rest went through late delivery. Fingerprint: intermediate blocks as `message` frames on distinct disposable `SessionSink`s, plus a final aggregate ≈ the sum of the pieces.
- **Prove the drop is after `se.OnText`:** `handlers.go` logs `[ccstream] OnAssistant: text_blocks=N text_bytes=B` for every assistant message (global tag). Match the transcript's text blocks (timestamp, length) 1:1 to these lines. A matched block missing from `app-frames.db` = loss downstream of `se.OnText` (router/sink dispatch), not the stdout reader.
- **Settle drop-vs-duplicate by instrumenting and reproducing**, not from archived logs. Put a DEBUG line at the `RunTurn` / `OpenAutonomousTurn` / orchestrator router Register/Clear sites, deploy, then repro: spawn a background Agent-tool subagent and fold user messages into the autonomous run. Logs from before the instrumentation cannot refute a hypothesis. A lone quiet trigger adopts cleanly; the bug needs a follow-up folding into an ACTIVE run.

## The message was never emitted as text

On Opus 5.5 / Fable 5.1, text written before a tool call can come back as a progress-update THINKING block (documented API behaviour; ~9% of opus-5-5 thinking responses, foci #1303). Foci renders only text blocks, so it is dropped, and the agent cannot tell. Fingerprint in the CC transcript (group lines by `message.id`): `thinking`(empty) + `thinking`(readable, a paraphrase of the lost reply) + `tool_use`, no `text`. Cross-check: `OnAssistant` shows `text_bytes=0`; `app-frames.db` has no `text.end` for it between the neighbouring seqs (match the exact distinctive string). Rule (backend-claude-code.md): text the user must read goes in the FINAL message after the last tool call, or via `foci_send_to_chat`.

## Subagent output in the MAIN chat instead of a collapsible chit

A subagent's text (or a SendMessage follow-up prompt) shows as a plain `role="agent"`/`role="user"` bubble. Do NOT chase the server `SessionSubagentDeliverer` fallback first. Find the frame and tally the group:

```
# (a) which frame kind carried the leaked text?
sqlite3 -readonly ~/data/app-frames.db "SELECT seq,datetime(sent_ms/1000,'unixepoch'),visible,
  json_extract(wire,'\$.t'), json_extract(wire,'\$.d.groupKey'), json_extract(wire,'\$.d.runIndex')
  FROM app_frames WHERE wire LIKE '%<distinctive substring>%' ORDER BY sent_ms"
# (b) start/text/end tally per group: a MISSING subagent.start is the smoking gun
sqlite3 -readonly ~/data/app-frames.db "SELECT substr(gk,1,26), SUM(t='subagent.start') starts,
  SUM(t='subagent.text') texts, SUM(t='subagent.end') ends FROM (SELECT json_extract(wire,'\$.d.groupKey') gk,
  json_extract(wire,'\$.t') t FROM app_frames WHERE conv_id='<convID>' AND json_extract(wire,'\$.t') LIKE 'subagent%')
  GROUP BY gk HAVING texts>0 ORDER BY starts"
```

`t="subagent.text"` with a valid groupKey/runIndex = the server framed it correctly; the leak is client-side. `InboundFrameDispatcher.kt` orphans `subagent.text`/`subagent.prompt` into a main-chat bubble when no base chit row (`db.messages().get(subagentId(groupKey))`) exists, i.e. no `subagent.start` arrived (run-1 SubagentStart is hook-driven and drops ~7% under bursty dispatch, #1423/#1425). Block size and start/end order are red herrings. The client now self-heals (#1422/#1424): an orphan provisions a chit and adopts a late start.
