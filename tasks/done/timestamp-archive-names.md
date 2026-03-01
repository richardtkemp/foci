# Timestamp-Based Archive Names (#252)

## Problem

Session archives use progressive numbers (`.1.jsonl`, `.2.jsonl`). These are unstable — if an old archive is deleted, the numbers don't convey when the archive was created. They're also meaningless as references.

## Proposed Fix

Change archive naming from `5970082313.1.jsonl` to `5970082313.2026-03-01T09-00-00Z.jsonl`.

### Files to modify

**session/store.go:**
- `nextArchivePath()` — use `time.Now().UTC().Format("2006-01-02T15-04-05Z")` instead of incrementing counter. Still need to handle collisions (add `-1`, `-2` suffix if same-second archive exists, though unlikely).
- `isArchiveFile()` — update to recognize timestamp format. Current check: `strings.Contains(base, ".")` after trimming `.jsonl`. New check: match pattern like `{base}.{timestamp}.jsonl` where timestamp matches `\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}Z`.
- Both old (numbered) and new (timestamp) formats should be recognized as archives for backward compatibility.

**session/store_test.go:**
- Update test archive filenames to use timestamp format
- Add test for backward compat with numbered archives
- Test collision handling

**command/sessions.go:**
- If archive listing/display exists, update to handle new format

**session/index.go:**
- If index references archives, update

### Key decisions
- Use UTC always (the `Z` suffix)
- Use hyphens instead of colons in time (filesystem safe)
- Old numbered archives continue to be recognized (read compat)
- New archives always use timestamps

### Migration
None needed — old `.1.jsonl` files stay as-is and are still recognized. New compactions produce timestamp-named files.
