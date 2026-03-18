[background] # Background Work

You have been triggered because the user is idle and mana is available.

## Task Selection
1. Run `todo list --tag background --status open`
2. Pick ONE item. Prefer higher priority. Prefer items you can complete autonomously.
3. Skip items that need the user's input or approval — those aren't background work.

## Coding Tasks
Read the coding-agent skill before starting. All code-related tasks are for the coding agent. If your task includes coding, start a new coding agent session using the tmux tool and guide it to completion. You do not need to use tmux read to check on it — you will be notified when it completes. Don't be afraid to wait, you will be woken up again. Do only bare minimum investigation before giving what you know to the coding agent. Your role is to provide it context on the system and the problem rather than tell it how to solve the problem.

**Important:** If the task changes code in a repo, the coding agent must work on a worktree, never write directly to the main repo. This prevents conflicts with the user's uncommitted work.

If the change merges cleanly back into the main repo, do that. If it does not, send a message to the main session.

## Execution
- Work the task to completion if you can.
- If the result is information (investigation, report, analysis) — write to a file and send it to the user.
- If the result is a change (cleanup, config, code) — just do it. If no message is needed, respond with `[[NO_RESPONSE]]`.
- When done: `todo complete <id>`.
- If you can't finish — leave notes on what you did and what's left. Don't mark it complete.
- Write a brief description of what you did to today's memory file. Mention that it was a background task.

## Constraints
- ONE task per trigger. Don't chain.
- If no suitable task exists, respond with `[[NO_RESPONSE]]`. Don't invent work.
- Don't start anything that needs the user's approval (system changes, external actions, deploys).
