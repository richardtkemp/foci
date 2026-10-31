<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Sessions — history files, CC transcripts, lifecycle

## Session Files (JSONL)

Per-session conversation history. No timestamps — just role + content.

**Path:** `~/data/sessions/<AGENT_ID>/<TYPE_ID>/root.jsonl` — `<TYPE_ID>` is `c<chat-id>` for a chat session (e.g. `c5970082313`) or `i<name-or-epoch>` for an independent session. Session keys are **stable identities** (`clutch/c5970082313`, `clutch/iresearch`); compaction and /reset never change the key or the directory.

Branch files sit beside the root as `b<epoch>.jsonl` with a `branch_meta` first line. Compaction/reset archive the live file **in place** with an "archived at" stamp: `root.<STAMP>.jsonl` (e.g. `root.2026-03-04T02-30-00+0000.jsonl`, `.<N>` counter on collision) — that file holds the session's history **up to** the stamp.

**Point-in-time lookup — which file / CC session covers moment T:**

```bash
foci debug at clutch/c5970082313 2026-07-01T12:00:00Z   # RFC3339
foci debug at clutch 3h                                  # duration ago; bare agent = default session
```

Prints the JSONL path covering that moment (live file or archive, with source) and the CC resume ID observed live then. Backed by `session_archives` + `cc_resume_history` in state.db, with archive filename stamps as a state.db-independent fallback.

```bash
tail -5 /path/to/root.jsonl | jq -r '.role + ": " + (.content[]? | select(.type=="text") | .text)'
```

## CC Backend Transcripts (JSONL)

On the CC backend the raw CC transcript is richer than foci's store: per-block `thinking`/`text`/`tool_use`/`tool_result`, RFC3339 UTC `timestamp`, thinking `signature`. Use it for turn-level *structure*.

**Path:** `/home/foci/.claude/projects/<workspace-slug>/*.jsonl`: under the foci OS user's home, shared by all agents, NOT in the agent's workspace. Workspace `/home/foci/clutch` → `-home-foci-clutch`. Newest mtime = most recent session.

```bash
# Map a turn's block structure (the key move)
tail -30 SESS.jsonl | jq -rc 'select(.type=="assistant" or .type=="user") | {ts:.timestamp, type, blocks:((.message.content // []) | if type=="array" then map(if .type=="thinking" then "think("+((.thinking|length)|tostring)+")" elif .type=="text" then "text:"+(.text[0:50]) elif .type=="tool_use" then "tool_use:"+.name else .type end) else ["str"] end)}'
```

**Gotchas:** a thinking block with `thinking` length 0 and a non-empty `signature` is redacted, not absent. Preamble before a tool call is a real `text` block; foci joins all of a turn's text blocks into one delivered message with **no separator**.

**Recovering an uncommitted edit's exact content** (never committed, `tool_details.db` empty): every `Edit`/`Write` is a `tool_use` block in the CC transcript with the real `old_string`/`new_string` (or `content`) in `.input`.

```bash
jq -c 'select(.message.content[]?.type=="tool_use" and (.message.content[]?.input.file_path? // "" | test("TARGET_FILE"))) | .message.content[] | select(.type=="tool_use") | {name, input}' SESS.jsonl
```

A **branch session** often continues the parent's transcript file, so its tool calls sit interleaved in the parent's `.jsonl`. This is the last-resort record for files outside git.

## Session state (state.db)

`~/data/state.db` holds the unified session lifecycle + provenance timelines.

| Table | Contents |
|---|---|
| `session_index` | session lifecycle: `session_key`, `status`, `session_type`, `agent_id`, `chat_id`, `is_root`, `last_activity_at` |
| `session_archives` | archive rotations: when a session was compacted/reset, reason, and the resulting `file_path` |
| `cc_resume_history` | which CC resume-ID was live for a session at a given time |
| `agent_metadata`, `chat_metadata`, `session_metadata` | agent/chat/session metadata (chat_metadata registers platform *ownership*; keys are deterministic so it no longer persists keys) |

```bash
sqlite3 ~/data/state.db "SELECT session_key, status, session_type, last_activity_at FROM session_index ORDER BY last_activity_at DESC LIMIT 10"
sqlite3 ~/data/state.db "SELECT archived_at, reason, file_path FROM session_archives WHERE session_key='clutch/c5970082313' ORDER BY archived_at"
sqlite3 ~/data/state.db "SELECT observed_at, resume_id FROM cc_resume_history WHERE session_key='clutch/c5970082313' ORDER BY observed_at"
```

## Lifecycle investigations

```bash
# When did compaction happen? (in-place archive; the session key is unchanged)
grep 'compacted from' ~/logs/foci.log | tail -10

# Resets
grep 'session reset key=' ~/logs/foci.log | tail -10

# Background cron sessions — list recent, then follow one
grep 'branch created.*cron' ~/logs/foci.log | tail -10
grep '<SESSION_KEY>' ~/logs/foci.log | head -20
```

## "Session stuck after /stop": did the interrupt reach CC, and did it orphan a subagent?

In delegated mode `/stop` (`command.StopCommand`) FIRST calls `DelegatedManager.StopSession` → `Backend.Interrupt` → `writer.SendInterrupt()`, straight to CC, apart from foci's turn-cancel (`CancelSession sk=... firing turn cancel`). Check both paths before you conclude "/stop did nothing".

**Proof the interrupt landed:** CC's transcript (`~/.claude/projects/<workspace-slug>/<resume-id>.jsonl`; resume-id from `foci debug at <session-key> <time>`) has a synthetic user message `[Request interrupted by user]` at that moment. The transcript holds only conversational turns, not raw stream-json events (`task_notification`, `session_state_changed`).

**Orphaned background subagent:** find the `Agent` `tool_use_id`. A background agent's `tool_result` returns at LAUNCH ("Async agent launched successfully"). If that id never appears again, the interrupt orphaned it. The `task_notification` handler (`internal/delegator/ccstream/handlers.go`) clears `SubagentTracker` on any terminal status and logs `subagent_end signal=task_notification status=<s>`; no such line = no notification, which foci.log alone cannot tell from "still running". The backstop is the `defaultAgentMaxAge` prune (2h; `[cc_backend].background_task_max_age`, `internal/delegator/agent_tracker.go`). It blocks the WHOLE session's turn dispatch until then (`agents.Pending()>0`, spec §4). `subagent tracker: pruned` in foci.log, timed exactly `defaultAgentMaxAge` after the orphaning, confirms it.

`OnResult` now calls `agents.ClearAll()` on any non-`"success"` result (#1350), which fixes the common interrupt case. Open gap (check whether it is still red): `inbox.go`'s sink-delivery gate (#767) can block a real user turn whenever ANY in-flight turn is non-delivering (`markInFlight(sk, false)`), not only system injects. Repro: `TestInbox_PlatformTurn_NotBlockedByOrphanedAutonomousAdoption` (`internal/agent/inbox_autonomous_gate_test.go`).
