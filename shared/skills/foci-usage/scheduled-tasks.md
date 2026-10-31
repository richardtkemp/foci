<!-- GOLDEN: ships with foci (shared/skills/foci-usage/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Scheduled & periodic tasks

Two kinds of scheduled work touch you: the tasks **foci runs for you automatically**, and the **durable schedules you create yourself** via crontab. This file covers both.

## 1. Tasks foci runs automatically

They arrive as `via=cron` turns. Each turn's prompt template can be customised (see **prompts.md**):

| Task | What it does | Customise via |
|------|--------------|---------------|
| **Keepalive** | Periodic liveness / cache-warm turn. A good moment to do tiny housekeeping; otherwise reply `[[NO_RESPONSE]]`. | `keepalive.md`, `[keepalive]` config |
| **Background work** | Idle-time turn for picking up self-directed work when nothing's queued. | `background.md`, `[background]` config |
| **Reflection** | Memory formation — captures facts to your daily file and procedures to skills. Runs on an interval, at session end, and after compaction. Skipped if nothing happened since last time. | `reflection.md`, `[reflection]` config |
| **Consolidation** | Periodically curates `MEMORY.md` from the daily files (a longer interval than reflection). | `memory-consolidation.md`, `[maintenance]` config |
| **Session reset** | Optional soft reset (memory formation + in-place history archive; the session key is stable) when a `reset_time` is configured. | `[maintenance]` config |
| **Log rotation** | Built-in: rotates the log files and writes `.gz` archives. No turn — runs inside foci. | — |

**Compaction** is related but *threshold-triggered*, not on a clock — it fires when a session's context grows too large (or on manual `/compact`). See **prompts.md**.

The reflection/consolidation turns are **system-internal**: their output is not delivered to the user, so you don't need to manage what they "send." A normal keepalive or cron turn, by contrast, *does* reach the user if you reply with text — so use `[[NO_RESPONSE]]` when there's nothing to say.

## 2. Creating your own durable scheduled turns

When you want a turn to fire on a schedule you define — a daily check, a weekly digest, a timed reminder-to-self that does real work — use the **user crontab**. It's the one durable mechanism: a backend scheduler such as Claude Code's own cron does **not** survive a restart/reload, so don't rely on it for anything that must persist.

foci expands `shared/crontab.template` per agent (agent ID, home dir, a stagger offset) into the foci user's crontab. Each entry runs the `foci` CLI to send a prompt as a one-shot branch session. It authenticates over foci's Unix socket by same-user peer credentials, so no API key sits in the crontab. Add your own scheduled task by editing the crontab.

A scheduled turn reaches the user the same way any turn does: reply with text and it's delivered to the platform; reply `[[NO_RESPONSE]]` for a silent check. For a check that should stay quiet unless it trips, have the scheduled command itself decide whether to invoke you at all (see the `silent-cron` skill) rather than waking you every time only to say nothing.

### Editing the crontab safely

One crontab is shared by **all** agents. Piping a failed or empty transform into `crontab -` installs an **empty** crontab and drops every agent's job. So:

1. Back up: `crontab -l > /tmp/crontab.bak`.
2. Edit a copy with assertions: the target line exists, and the line count changed by the expected delta (a job is usually **two** lines: its `# comment` and the job line).
3. Install only the verified file (`crontab /tmp/crontab.new`), check `crontab -l`, and restore from the backup if anything is off.