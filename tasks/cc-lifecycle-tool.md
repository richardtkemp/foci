# Task: CC Lifecycle Management Tool

## Problem
Managing a coding agent (Claude Code) through tmux requires 3-5 tool calls per task just for ceremony:
1. Start tmux session (or check if one exists)
2. Resize to 300x80
3. Set up watch (55s threshold)
4. Send task instructions
5. On watch notification: read output, approve plan, wait for completion
6. Send commit/push commands
7. Send next task

This is repeated identically for every task. The orchestrating agent (Clutch) spends significant mana on this boilerplate.

## Proposed Solution
A `coding-task` tool (native Go, built into clod) that manages the full CC lifecycle.

### Interface
```
coding_task:
  session: "cc-main"              # tmux session name (reuse or create)
  task: "description or path"     # inline task text OR path to task file
  workdir: "/home/rich/git/clod"  # project directory
  agent: "claude-code"            # "claude-code" or "opencode"
  auto_commit: true               # auto-send commit/push on completion
  auto_approve: true              # auto-approve CC plans (option 2)
```

### Behaviour
1. **Session check** — if session exists and agent is at prompt (`❯`), reuse it. Otherwise start new session with agent, resize to 300x80.
2. **Send task** — if `task` is a file path, send `Read <path> for your next task.` Otherwise write task to a temp file and send the read command (avoids paste issues).
3. **Watch** — set up tmux watch with 55s threshold.
4. **On completion** — when watch fires and agent is at prompt:
   - Read last N lines of output
   - If agent produced a plan and is waiting for approval AND `auto_approve` is true, send approval (option 2)
   - If agent completed work AND `auto_commit` is true, send `git add -A && git commit -m "<inferred message>" && git push`
5. **Report** — return structured result: success/failure, summary of changes, files modified, test results.

### What it does NOT do
- Replace human judgment on design decisions — if CC asks a question that isn't a plan approval, it escalates to the orchestrating agent
- Auto-approve when it's not option 1/2 — only handles the standard plan approval prompt
- Kill sessions — that's still a human decision

### Implementation Notes
- This is a clod tool (tools/coding_task.go), not a script
- It's essentially a state machine: STARTING → SENDING → WAITING → APPROVING → COMPLETING → COMMITTING → DONE
- Each state transition happens on tmux watch notifications
- The tool returns immediately after sending the task; progress updates come through watch notifications
- Multiple coding-task instances can run in parallel (different session names)

### Queue Management (future)
Later enhancement: accept a list of tasks, execute sequentially in the same session. But start with single-task.
