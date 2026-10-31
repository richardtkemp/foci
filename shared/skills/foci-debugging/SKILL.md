---
name: foci-debugging
description: Debug and investigate foci platform internals. Service logs and archives, API/cost and payload logs, session files and CC backend transcripts, cache-bust diagnosis, app (FAP) delivery gaps, permission-rule failures, reproducing a make test failure, and triaging a red-main CI run. Read the relevant subfile before investigating.
owner: foci
seeded: true
---

# Foci Debugging — Internals & Investigation

How to investigate a running foci: where the data lives and how to read it. Each topic has its own file.

> **This `SKILL.md` is yours to customise** (seed-if-missing). The subfiles below **ship with foci and are overwritten on restart**: edit them in the foci repo (`shared/skills/foci-debugging/`), not in `~/shared/skills/`.

## Where to look

| Subfile | Read it when you need… |
|---|---|
| **logs.md** | Service logs (`~/logs/foci.log`, journal, `.gz` archives), the data-source map, per-agent SQLite DBs, crash-vs-clean restart. |
| **api-cost.md** | Provider auth, the cost ledger (`api.db`), payload logs, per-agent cost and the `session_index` join. |
| **api-cost-accounting.md** | The traps before you state a cost NUMBER: CC's `cost_usd` is cumulative per process, legacy calls hold turn totals, checking CC's price per process. |
| **cache.md** | Prompt-cache bust diagnosis (companion to the `cache-diagnosis` skill). |
| **sessions.md** | Session JSONL files, CC backend transcripts, `state.db` session/archive/resume tables, compaction/reset/cron, a stuck session after `/stop`. |
| **test-harness.md** | A test fails under `make test` but not a plain `go test`: the sealed env `scripts/seal-test.sh` builds, and which arm isolates which variable. |
| **ci-triage.md** | A `[foci-test-runner]` "tests FAILED" notification: who broke main. |
| **permissions.md** | `Permission allow rule Write(...) is not matched by file permission checks`, and why a command is or is not auto-approved (execguard). |
| **app-delivery.md** | A message never reached the native app: the frame store (`~/data/app-frames.db`), sink vs replay bugs, the ULID-vs-chat-id trap. |

For **normal operation** (tools, prompts, turn lifecycle, config) see **`foci-usage`**.
