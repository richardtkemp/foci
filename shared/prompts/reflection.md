## Reflection Pass

Pause on the work since your last reflection and capture anything future sessions will need. Four outputs live here:

- **Memories** — factual knowledge: decisions, lessons, milestones
- **Todos** — friction that can and should be *fixed* rather than remembered
- **Skills** — ultra-concise hints for specific, likely-repeated pain points
- **Chat follow-up** — one message to the chat about a promise the agent has not kept, or an important point the user dropped

Each is independently optional. If nothing qualifies, skip that section. Better empty than noise.

## Memory Formation

Capture important memories from this session to today's daily file (`memory/YYYY-MM-DD.md`). Do not mention this prompt or the memory formation process to the user unless asked.

### What to write
- Decisions made and their reasoning
- Lessons learned (especially mistakes)
- Project milestones (not every intermediate step)
- Things future sessions need to know

### What NOT to write
- Play-by-play of what happened (this is a memory file, not a transcript)
- Individual commits, tool calls, or command outputs
- Intermediate states ("Claude Code is building...", "waiting for approval...")
- Anything already captured in today's file

### Format
- One section per topic, 2-5 lines each
- Give each section a heading that concisely summarises its contents, so a reader can judge the section from the heading alone. `## Morning` is a bad heading; `## Deploy failed: skipped restart left a stale binary` is a good one. A time may follow the summary, but it never replaces it.
- Aim for 2-5k characters on a typical day, proportional to actual activity. A massive day with 9 deploys will be bigger than a quiet one — that's fine.
- If nothing notable happened since last write, do nothing

### How to write
Append with a quoted heredoc (`cat >> memory/YYYY-MM-DD.md <<'EOF'` … `EOF`), or use the Edit or Write tool, or `echo "text" >> memory/YYYY-MM-DD.md`. Never generate memory text with a script (python, sed, awk): foci keeps track of which notes this session already has from the text of your write call, and a generated write hides it.

### Do NOT edit MEMORY.md
Daily files only. MEMORY.md curation is handled by the consolidation job.

## Todos

Some of what you learned should not be written down at all — it should be **fixed**. When this session hit friction whose cause is fixable at source, file a todo instead of recording a way to live with it.

The test: could a change to the tool, script, config, or code make this friction stop existing? If yes, it is a todo — not a memory, and definitely not a skill.

- **Check for an existing one first** — `foci_todo search <keyword>` (full-text, stemmed, and it searches closed items too, so you also see one already fixed). A duplicate is worse than nothing.
- File it with repro detail: what you ran, what you expected, what you got.
- Worked example — the build prints misleading output. Do **not** write *"beware, the build output is misleading"* into a skill. File a todo to make the build output clearer.

## Skill Formation

**Skills are not memories.** A memory records what happened and why. A skill is an *ultra-concise instruction or hint* for a specific pain point you expect to hit again, written for a reader who does not know the incident happened and does not care — they want the rule, in one breath.

### When to capture a skill

The bar is high, and it is a **conjunction** — capture a skill only when ALL THREE are clearly true:

1. **The path was not obvious.** You had to discover the right sequence, recover from a failure, or follow a correction — not just do the obvious thing that worked first try.
2. **Reading the skill would have saved real time.** A future agent handed this skill reaches the outcome materially faster than re-deriving it from scratch. If any competent agent would figure this out quickly on its own, a skill adds noise, not value.
3. **It will recur, and it is findable from the situation.** The trigger must be something a future agent can recognise *while working* — a tool, a flag, an error shape, a file type. If the only way to know the skill applies is to already know the incident, it is a memory, not a skill.

If all three aren't clearly true, skip this section entirely. Most sessions won't produce a skill. Better none than a bad one.

Concrete signals that (1) and (2) hold: 5+ tool calls chained non-obviously; error recovery you had to adapt to; a mid-task correction that changed the approach; a sequence a future agent would otherwise re-derive painfully.

### Never encode a workaround for something that should be fixed

If the lesson is "tolerate this broken thing", it does not belong in a skill — **file a todo** (above) and write nothing. A skill that compensates for a queued todo is worse than useless: it outlives the bug and teaches the workaround to agents who would have met the fixed version.

Before adding anything, ask: am I documenting how the world *works*, or how to survive something that shouldn't be that way?

### Write the rule, not the incident

The evidence convinced *you*. The reader already believes you — they need the instruction, not the proof.

- **Cut:** dates, ticket numbers, repo paths, function and test names, verbatim command output, counts, the story of how you found it, what you believed at the time.
- **Keep:** the trigger, the rule, and the check. A short parenthetical is the most provenance any entry earns.

The same lesson, before and after:

> ✗ Verified 2026-08-03 in `/home/rich/git/foci`: `ack -rn foo internal/` → 0, `ack -n foo internal/command/` → 8, `grep -rn` → 13 (it was in `internal/command/observability.go`); I stated out loud that the function didn't exist…
>
> ✓ `ack -n` means `--no-recurse`, not line numbers (in `-rn` the `-n` wins). A zero result from ack over a parent dir is not evidence of absence — re-check with `grep -rn`.

### Size — count characters, not lines

A skill is loaded into context every time it is read; every byte competes with the work.

- **One entry: ≤400 characters.** If it needs more, it isn't distilled yet.
- **One file: ≤6,000 characters** (a router SKILL.md that mostly points at sub-files: ≤10,000). Over budget means condense or split — never append.
- Measure in **characters**. A single 2,000-character bullet satisfies any line count, so lines cannot bound this.

### Merge into an existing skill — don't create a new one by default

Creating a new skill is the **exception, not the default.** Scan the Available Skills block in the system prompt first. If any existing skill is even adjacent to this workflow, **edit its SKILL.md to fold in what you learned** rather than adding a sibling. A new top-level skill is justified only when nothing existing is a plausible home. Skill-sprawl and duplication are worse than an imperfect edit to the right existing skill.

### Where and how to write

Location: `workspace/skills/{kebab-case-slug}/SKILL.md`

Frontmatter:

```
---
name: concise-skill-name
description: One-line trigger — when a future agent should reach for this skill
autogenerated: true
script: script.sh   # optional; omit if markdown-only
---
```

`autogenerated: true` flags the skill for human review. Leave it — a reviewer removes the line once the skill has been verified.

### Pair with a script when there's a deterministic core

If the workflow has a replayable sequence (commands, a pipeline, an API chain), write it as `script.sh` or `script.py` next to SKILL.md and reference it via the `script:` frontmatter field.

- **SKILL.md** explains *when* to reach for it and *why* the approach works
- **script** does *what* — the mechanical steps, runnable

A skill without a script is still useful. A script without a skill is not discoverable.

### Skill body structure

Keep it tight. A good skill is:

1. **Trigger** — one paragraph on the situation where this skill applies
2. **Approach** — the steps or the script invocation, in order
3. **Gotchas** — what to check before trusting the output

Hold it inside the character budget above. If it won't fit, it's probably two skills.

## Chat Follow-up

Read the conversation for two things that are still open now:

- **An unkept promise.** The agent said it WOULD do a specific thing ("I'll run the tests", "I will file that"), and the conversation shows that it did not do it.
- **A dropped point.** The agent told the user that something was important (a risk, a decision the user must make, an action the user must take), and the user did not respond to it.

These are NOT promises, and they never get a follow-up: an offer ("want me to…?"), a suggestion, or a step that waits for the user's answer. Also skip an item the conversation shows was done, cancelled or replaced.

If you find one or more items, send ONE message to your parent chat with `send_to_session` (the parent key is in your orientation), `reply_to` = `session`. Name each item and quote the line it comes from. For an unkept promise, tell the chat to do the thing now, or to tell the user why not. For a dropped point, tell the chat to remind the user. Put every item in the one message: you can send only one. If you find nothing, send nothing.

This message is the only one you may send to another session. Never use it for a status update or a summary of this pass. If your orientation names no parent chat, skip this section.

---

When done with all four sections, respond with `[[NO_RESPONSE]]` and nothing else. No announcements needed.
