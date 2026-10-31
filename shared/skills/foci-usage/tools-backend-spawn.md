<!-- GOLDEN: ships with foci (shared/skills/foci-usage/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Backend tools — spawns

Per-tool reference for the Claude Code (shell) backend. How `foci_*` shell functions work is in **tools-backend.md**. Every tool accepts `-h`/`--help`; read it before first use this session.

### `foci_spawn` vs CC's `Agent` tool
- `foci_spawn "<task>"` runs a sub-call. The default mode is `character`: your full system prompt, tools and permission rules, but not this conversation. So the task text must carry everything it needs (ticket id, file paths, what done means).
- It returns its result directly. With `--background` it runs in its own background session, returns at once with a spawn id, and you keep taking messages. A background spawn's result arrives ONCE as a `[SPAWN RESULT]` message; nothing else wakes you.
- `--context clone` forks this session (a copy of your whole conversation, far more expensive). Use it only when the task needs so much of the conversation that you could not write it into a prompt. `--context raw` (Claude Code only) is just your prompt; `--context explore` is read-only research.
- `--model` is passed to your backend as written. `--timeout` defaults to 120 s direct and 3600 s background, max 86400 s. A spawn that runs out is stopped and reported failed.
- `--agent <id>` runs AS another agent: its character, tools, workspace, permission rules, backend and model. The result comes back to you; nothing reaches that agent's chats. It implies character mode (do not pass `--context`); `--background` works. Its `send_to_chat` lands in YOUR chat; it cannot `send_to_session` or `remind`.
- Every spawn first checks the plan allowance of the backend it runs on (the target's, with `--agent`). With less than `[tools] spawn_reserve_percent` (default 10%) left in the 5-hour or weekly window it is refused, naming the window, what is left and when it resets. `--use-reserve` runs it anyway. A backend with no usage data runs unchecked.
- A clone's permission prompts reach the user headed `🐣 From spawn <id>` and never block your own turn. A character spawn's prompts are decided by the auto-approve rules, never asked. In auto mode nobody is asked. `/stop` stops your spawns too. A background spawn cannot start another.

### `foci_spawn_send --id <spawn-id> "<message>"`
- Tell a spawn more. A running spawn gets it folded into its current turn. A finished one resumes on its branch with its earlier context and reports a new `[SPAWN RESULT]`.

### `foci_spawn_status [--id <spawn-id>] [--last N]`
- With no id: the list of running and recently finished spawns of every mode (a direct spawn is listed while it runs too). With an id: its state and its last N activity entries (what it was told, its text, its tool calls).

### `foci_send_to_spawner <message>` (or piped stdin)
- Inside a spawn: message the session that spawned you while you run.
- From a background spawn it arrives now; your spawner answers with `foci_spawn_send`.
- From a spawn your spawner waits on, it is kept and heads your result, because your spawner cannot read anything until you finish.
- A raw or explore spawn on Claude Code has no shell, so it cannot call this.

### `foci_spawn_stop <spawn-id|all>`
- Stop a running spawn of any mode. A background one reports `[SPAWN RESULT]` as stopped; a direct one ends its call as stopped.
- A direct spawn is also stopped when its call ends without its result (the `foci_spawn` process is killed, or its Bash call is stopped).
