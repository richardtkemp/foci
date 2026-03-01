# Task: Review Uncommitted Changes in telegram/bot.go

Review the uncommitted changes in `telegram/bot.go`. These were made manually (not by a coding agent) and need a proper review before committing.

Run `git diff telegram/bot.go` to see the changes.

## What changed

1. **New `sendReplyWithFullThinking` function** — sends thinking (italic) + divider + response as a single HTML message
2. **Moved the `show_thinking == "true"` branch** — from before the edit-attempt logic to after it, and now calls the new function instead of string concatenation
3. **Updated `formatThinkingExpanded`** — uses italic HTML tags and a `————` divider instead of `~~~~`

## Review criteria

1. **Correctness** — Any bugs or edge cases? Does the message flow still work correctly with the moved branch?
2. **HTML safety** — Is the escaping correct? Could thinking text break the HTML?
3. **Message splitting** — `splitMessage` is called on the full HTML. Does it handle splitting mid-tag safely?
4. **Consistency** — Does the compact mode toggle still work correctly with the new formatting in `formatThinkingExpanded`?
5. **Anything else** that's wrong, risky, or improvable.

## Output

Write your review to `/home/rich/git/foci/tasks/review-thinking-result.md`. Be specific — cite code, concrete issues. If you find issues worth fixing, fix them and commit.
