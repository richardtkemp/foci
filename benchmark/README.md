# Compaction Benchmark

Measures how well a foci agent retains working knowledge across compaction.

## Quick Start

```bash
# 1. Add bench agent to foci.toml (see bench-agent.toml.example)
# 2. Restart foci
# 3. Run the benchmark
python3 benchmark/run.py --agent bench --phases all

# Or run phases separately:
python3 benchmark/run.py --agent bench --phases load
python3 benchmark/run.py --agent bench --phases compact
python3 benchmark/run.py --agent bench --phases quiz --mode context-only
python3 benchmark/run.py --agent bench --phases compare  # all 3 modes
```

## Architecture

1. **Load phase** — sends 201 scripted messages to build up context across 20 sequences
2. **Compact phase** — triggers `/compact` via CLI
3. **Quiz phase** — asks 50 deterministic questions, auto-scores answers

## Quiz Modes

Three modes test different aspects of compaction quality:

| Mode | What it tests | How |
|------|--------------|-----|
| `context-only` | Pure compaction retention | Agent told "answer from memory only, no tools" |
| `context+memory` | Compaction + memory files | Agent may read memory/*.md but no other tools |
| `full` | Graceful recovery | Agent can use all tools to find answers |

The gap between `context-only` and `full` scores reveals how much the agent can recover via tools.

Run all three at once:
```bash
python3 benchmark/run.py --agent bench --phases compare
```

## Scoring

| Score | Meaning | Points |
|-------|---------|--------|
| correct | Expected answer found in response | +2 |
| partial | Key terms present but not exact | +1 |
| confabulated | Confident but wrong (worst outcome) | -5 |
| acknowledged_unknown | "I don't know" (honest) | +1 |
| wrong | Incorrect | 0 |

```
Composite = (correct × 2) + (partial × 1) - (confabulated × 5) + (acknowledged_unknown × 1)
Max score = total_questions × 2
```

Confabulation is penalised 5× because a confidently wrong agent is worse than one that says "I don't know."

## Loading Sequences (201 messages across 20 sequences)

- File operations, investigation, distractors, corrections, debugging
- Docker tasks, context overload, config changes, user preferences
- Multi-step task planning, error investigation, people/context
- Tangents and returns, ambiguous instructions, numbers/specifics
- Contradictions, rapid-fire recall, dependency chains, end-of-day wrap

## Quiz Categories (50 questions across 8 banks)

- Factual recall, correction retention, cross-reference, negative knowledge
- People/context, numbers/specifics, architecture decisions, preferences

## Design Principle

Many loading messages establish a fact then *correct* it later. The quiz tests whether the agent remembers the *corrected* version, not the original. This directly measures compaction quality — poor compaction preserves the wrong version.

## Files

- `bench-agent.toml.example` — agent config snippet
- `fixtures/` — pre-populated workspace files
- `quizzes/` — question banks with expected answers
- `loading/` — scripted message sequences (JSONL)
- `run.py` — harness script
- `results-*.json` — output files (gitignored)
