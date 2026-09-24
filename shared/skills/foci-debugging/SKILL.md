---
name: foci-debugging
description: Debug and investigate foci platform internals. Service logs and archives, API/cost and payload logs, session files and CC backend transcripts, cache-bust diagnosis, app (FAP) delivery gaps, permission-rule failures, reproducing a make test failure, and triaging a red-main CI run. Read the relevant subfile before investigating.
owner: foci
seeded: true
---

# Foci Debugging — Internals & Investigation

How to investigate a running foci: where the data lives and how to read it. This SKILL.md is a directory; each topic lives in its own file.

> **This `SKILL.md` is yours to customise** (seed-if-missing — override it, add your own sibling files). The content files it lists below **ship with foci and are overwritten on restart** — edit those in the foci repo (`shared/skills/foci-debugging/`), not the deployed `~/shared/skills/` copy.

## Where to look

| Subfile | Read it when you need… |
|---|---|
| **logs.md** | Service logs (`~/logs/foci.log`, the journal), the data-source scope map, per-agent SQLite DBs, and the log-reading gotchas (`.gz` archives, awk-not-grep, panics). |
| **api-cost.md** | Provider auth, the API call log (`api.db`), payload logs, per-agent cost and the `session_index` join — where the data is. |
| **api-cost-accounting.md** | The traps before you state a cost NUMBER: `cost_usd` is cumulative, token columns have different scopes, the `cost divergence` WARN is a sampler. |
| **cache.md** | Anthropic prompt-cache mechanics and cache-bust diagnosis (companion to the `cache-diagnosis` skill). |
| **sessions.md** | Session history files (stable-key JSONL), CC backend transcripts, the `state.db` session/archive/resume tables, and compaction/reset/cron lifecycle. |
| **test-harness.md** | A test fails under `make test` but not under a plain `go test` — the sealed/redirected environment `scripts/seal-test.sh` builds, how to replicate it for one package, and which arm isolates which variable. |
| **ci-triage.md** | A `[foci-test-runner]` "tests FAILED" notification — who broke main: same-commit flips, reading parallel test logs, bounding the culprit range, recurring causes, and a deploy changing the machine under the tests. |
| **permissions.md** | A task fails with `Permission allow rule Write(...) is not matched by file permission checks` — why it's order-dependent, how to audit exposure across agents, and the seed constant that reintroduces it. |
| **app-delivery.md** | A message never reached the native app — the durable frame store (`~/data/app-frames.db`), reading `seq` to tell a sink bug from a replay/ack bug, and the ULID-vs-chat-id trap. |

For **normal operation** (not investigation) — tools, prompts, turn lifecycle, config — see the companion **`foci-usage`** skill.
