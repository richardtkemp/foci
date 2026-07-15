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
# Last few messages
tail -5 /path/to/root.jsonl | jq -r '.role + ": " + (.content[]? | select(.type=="text") | .text)'

# All content (not just text)
tail -5 /path/to/root.jsonl | jq -r '.role + ": " + (.content[]? | tostring)'
```

## CC Backend Transcripts (JSONL)

When foci runs on the Claude Code backend, the raw CC transcript is richer than foci's own session store above: per-block `thinking`/`text`/`tool_use`/`tool_result`, RFC3339 `timestamp`, and a thinking `signature`. Use this (not the foci store) when you need turn-level *structure* — e.g. distinguishing thinking from output, or diagnosing why a turn's text arrived oddly.

**Path:** `<foci-os-user-home>/.claude/projects/<workspace-cwd-slug>/*.jsonl`. The `.claude/` dir lives under the **foci OS user's home** (e.g. `/home/foci`), shared across all agents — NOT inside the agent's own workspace. The project *subdir* is the agent's workspace path slugified (`/` → `-`): workspace `/home/foci/clutch` → `/home/foci/.claude/projects/-home-foci-clutch/`. Most recent session = newest mtime.

```bash
# Map a turn's block structure (the key move)
tail -30 SESS.jsonl | jq -rc 'select(.type=="assistant" or .type=="user") | {ts:.timestamp, type, blocks:((.message.content // []) | if type=="array" then map(if .type=="thinking" then "think("+((.thinking|length)|tostring)+")" elif .type=="text" then "text:"+(.text[0:50]) elif .type=="tool_use" then "tool_use:"+.name else .type end) else ["str"] end)}'
```

**Gotcha:** a redacted/summarised thinking block has `thinking` length 0 but a non-empty `signature` — it's still a thinking block, just with content stripped. Don't mistake an empty thinking block for "no thinking happened." Conversely, conversational preamble before a tool call is a real `text` (output) block, not thinking — foci joins all of a turn's text blocks into one delivered message with **no separator**.

**Recovering an uncommitted edit's exact content** ("what did that Edit/Write change, but it was never committed and `tool_details.db` is empty/not applicable"): grep the CC transcripts for the tool call itself — every `Edit`/`Write` shows up as a `tool_use` block with the real `old_string`/`new_string` (or `content`) in `.input`, independent of git history entirely.

```bash
# Find the right transcript file(s) by mtime when you don't know the session id
find ~/.claude/projects/<workspace-slug>/ -maxdepth 1 -name "*.jsonl" -newermt "TIME1" ! -newermt "TIME2" -printf "%T@ %p\n" | sort -n

# Pull every Edit/Write on a specific file, with its old/new content
jq -c 'select(.message.content[]?.type=="tool_use" and (.message.content[]?.input.file_path? // "" | test("TARGET_FILE"))) | .message.content[] | select(.type=="tool_use") | {name, input}' SESS.jsonl
```

Works even when the edit was made by a **branch session** — a branch commonly shares/continues the parent's underlying transcript file rather than starting a fresh one, so a branch's tool calls show up interleaved with the parent's history in the same `.jsonl`. This is the artifact of last resort for "what was in this file before someone changed it" when the change never hit git (config repos, scratch files, anything outside the tracked tree) — don't give up at an empty `tool_details.db`.

## Session state (state.db)

`~/data/state.db` holds the unified session lifecycle + provenance timelines.

| Table | Contents |
|---|---|
| `session_index` | session lifecycle: `session_key`, `status`, `session_type`, `agent_id`, `chat_id`, `is_root`, `last_activity_at` |
| `session_archives` | archive rotations: when a session was compacted/reset, reason, and the resulting `file_path` |
| `cc_resume_history` | which CC resume-ID was live for a session at a given time |
| `agent_metadata`, `chat_metadata`, `session_metadata` | agent/chat/session metadata (chat_metadata registers platform *ownership*; keys are deterministic so it no longer persists keys) |

```bash
# All sessions with status, type, last activity
sqlite3 ~/data/state.db "SELECT session_key, status, session_type, last_activity_at FROM session_index ORDER BY last_activity_at DESC LIMIT 10"

# Active only
sqlite3 ~/data/state.db "SELECT session_key, session_type, last_activity_at FROM session_index WHERE status='active' ORDER BY last_activity_at DESC"

# Archive rotations for a session (when compacted/reset, to which file)
sqlite3 ~/data/state.db "SELECT archived_at, reason, file_path FROM session_archives WHERE session_key='clutch/c5970082313' ORDER BY archived_at"

# CC resume-ID timeline
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
