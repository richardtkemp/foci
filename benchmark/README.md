# Compaction Benchmark

Measures how well a foci agent retains working knowledge across compaction.

## Quick Start

```bash
# 1. Add bench agent to foci.toml (see bench-agent.toml.example)
# 2. Restart foci
# 3. Run the benchmark
python3 benchmark/run.py --agent bench --phases all
```

## Architecture

1. **Load phase** — sends scripted messages to build up context (~200-300 messages)
2. **Compact phase** — triggers `/compact` via CLI
3. **Quiz phase** — asks deterministic questions, scores answers

## Triggering Compaction

```bash
foci send -a bench "/compact"
```

## Quiz Modes

- **Mode 1: Pure context** — tools+memory disabled (tests what compaction retained)
- **Mode 2: Context+memory** — memory enabled, tools disabled
- **Mode 3: Full agent** — all tools enabled (tests graceful recovery)

Mode switching requires agent config changes between quiz runs (tool restrictions TBD).

## Scoring

- **Correct:** exact match
- **Partially correct:** right direction, wrong specifics
- **Confabulated:** confidently wrong (penalised heavily)
- **Acknowledged unknown:** "I don't know" (better than confabulation)

```
Score = (accuracy × 2) - (confabulation × 5) + (honest_fail × 1)
```

## Files

- `bench-agent.toml.example` — agent config snippet
- `fixtures/` — pre-populated workspace files
- `quizzes/` — question banks with expected answers
- `loading/` — scripted message sequences
- `run.py` — harness script

## Coverage

**Loading sequences (197 messages across 19 sequences):**
- File operations, investigation, distractors, corrections, debugging
- Docker tasks, context overload, config changes, user preferences
- Multi-step task planning, error investigation, people/context
- Tangents and returns, ambiguous instructions, numbers/specifics
- Contradictions, rapid-fire recall, dependency chains, end-of-day wrap

**Quiz categories (48 questions across 8 banks):**
- Factual recall, correction retention, cross-reference, negative knowledge
- People/context, numbers/specifics, architecture decisions, preferences

Key design principle: many loading messages establish a fact then *correct* it later.
The quiz tests whether the agent remembers the *corrected* version, not the original.
This directly measures compaction quality — poor compaction preserves the wrong version.
