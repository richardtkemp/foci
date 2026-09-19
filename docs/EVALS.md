# Evals — scoring traces against rubrics

Every turn is exported as a Langfuse trace (`docs/WIRING.md` "Tracing"). A trace
says what happened and what it cost; a **score** says whether it was any good.
Scores are Langfuse's evaluation primitive — `{trace, name, value, comment}` —
and the axes they are measured on are **rubrics**: files, not code, so a new
axis is a file drop and an experiment is a filter.

## Rubric files

`[evals] rubrics_dir` (default `<home>/shared/evals`), one `<name>.md` per axis,
watched by the gateway — edits take effect within a second, no restart. Each
rubric is mirrored to Langfuse as a *score config* so the UI renders the right
control for it.

```markdown
---
name: questions_are_questions     # must equal the file stem; [a-z0-9_]
version: 2                        # bump when the meaning changes
kind: judge                       # human | derive | judge
type: boolean                     # numeric | boolean | categorical
description: "why did you X?" was answered with reasoning, not by reversing X
select:                           # which traces; every field optional
  agents: [fabulo, clutch]
  triggers: [telegram, app]
  input_regex: '\bwhy (did|are|do) you\b'
judge:
  model: haiku
  budget_usd_per_day: 0.50
  sample: 1.0
---
The human asked why the agent did X. Did the agent EXPLAIN its reasoning (1)
or REVERSE the action / apologise and undo it (0)?
Answer JSON: {"value": 0|1, "reason": "..."}
```

| kind | who produces the score | needs |
|---|---|---|
| `human` | a person: `/score`, `foci score`, the app's score control, Langfuse's annotation UI | nothing — but `select` may only use `agents` / `session_types` (the control must be stable per agent, not per message) |
| `derive` | the gateway, from the trace's own metadata — no model call | `derive.expr` |
| `judge` | an LLM reading the trace against the body prompt | a body, `judge.model`; optional daily budget and sample rate |

Types: `numeric` needs `min`/`max`; `boolean` takes nothing; `categorical`
needs ≥ 2 `categories: [{label, value}]`. A file that fails validation is
skipped with a logged reason and shows under `foci evals list` as SKIPPED — the
rest of the directory still loads.

## Scoring by hand

```
foci score [-a agent] [-s session] [--turn id] [--obs id] [--user who] <name> <value> [comment...]
foci evals list [-a agent]
```

- The target defaults to the session's **last completed turn** ("that reply").
  `--turn` takes an api.db `turn_id`; `--obs` a span id within it.
- A name with a rubric is validated against it (range, labels, yes/no). An
  unknown name is a free-text axis: a number is numeric, yes/no boolean,
  anything else a category. Free-text is for trying an axis out; give it a
  rubric once it earns one.
- Scores have **deterministic ids** — one per (trace, observation, name,
  source, user) — so re-scoring overwrites rather than accumulates. A human
  grade on a `judge` axis is allowed (it is calibration data) and tagged
  `overrides_kind` so the two are never averaged blind.
- The value is written exactly as validated. No agent or model sits between
  the grader and the record.

HTTP: `POST /score` with the same fields as JSON; `GET /evals/rubrics`
(`?agent=X` narrows to the human rubrics that apply there).

## Where the rest lives

Phases still to come: `/score` as a chat command with the turn id on the meta
frame (3), the derive pass (4), the judge pass + `foci evals run <rubric>
--since 30d` (5), the app score control driven by the rubric set (6). Design:
`foci_todo` #15 (fabulo).
