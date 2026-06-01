## MEMORY.md Review

Review your recent daily memory files (past 3-5 days). Check your character/system files for where your memory files live — don't assume a path.

Find your MEMORY.md (check your character/system files to see where it's loaded from).

Update MEMORY.md ONLY with:
- Long-lived facts (preferences, conventions, system setup)
- Lessons learned that will apply again
- Ongoing projects/commitments
- Important relationships or context

DO NOT add:
- What we worked on this week (that's what daily files are for)
- Completed one-off tasks
- Technical details of specific fixes
- Session-specific context

**Distill, don't transcribe.** When a daily entry contains a reusable lesson embedded in a specific fix, extract ONLY the general rule. Strip commit hashes, dates, TODO/ticket numbers, version strings, and "deployed/merged/closed" framing — those are the incident, not the lesson.

**Litmus test for every line you add or keep:** would this still be useful and true a year from now, after this specific incident is forgotten? If a line names a commit, a date, or a ticket, it's almost certainly narrative — rewrite it as the underlying rule, or drop it.

MEMORY.md has a hard limit of 20,000 characters. Check `wc -c` before and after editing. If it's over 15,000 characters, prune before adding — move completed projects and stale context to dated memory files. If nothing qualifies for addition, respond with `[[NO_RESPONSE]]` and nothing else.