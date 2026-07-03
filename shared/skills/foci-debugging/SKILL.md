---
name: foci-debugging
description: Debug and investigate foci platform internals. API logs, payload logs, session files, CC backend transcripts, cache diagnosis, service logs, and common investigation patterns. Read the relevant subfile before investigating.
---

# Foci Debugging — Internals & Investigation

How to investigate a running foci: where the data lives and how to read it. This SKILL.md is a directory; each topic lives in its own file.

> **This `SKILL.md` is yours to customise** (seed-if-missing — override it, add your own sibling files). The content files it lists below **ship with foci and are overwritten on restart** — edit those in the foci repo (`shared/skills/foci-debugging/`), not the deployed `~/shared/skills/` copy.

## Where to look

| Subfile | Read it when you need… |
|---|---|
| **logs.md** | Service logs (`~/logs/foci.log`, the journal), the data-source scope map, per-agent SQLite DBs, and the log-reading gotchas (`.gz` archives, awk-not-grep, panics). |
| **api-cost.md** | Provider auth, the API call log (`api.db`), payload logs, and "where did the cost go?" — cost/token/cache-stat queries. |
| **cache.md** | Anthropic prompt-cache mechanics and cache-bust diagnosis (companion to the `cache-diagnosis` skill). |
| **sessions.md** | Session history files (stable-key JSONL), CC backend transcripts, the `state.db` session/archive/resume tables, and compaction/reset/cron lifecycle. |

For **normal operation** (not investigation) — tools, prompts, turn lifecycle, config — see the companion **`foci-usage`** skill.
