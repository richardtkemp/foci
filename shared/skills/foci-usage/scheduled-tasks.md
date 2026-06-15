# Scheduled & periodic tasks

Two kinds of scheduled work touch you: the tasks **foci runs for you automatically**, and the **durable schedules you create yourself** via crontab. This file covers both.

## 1. Tasks foci runs automatically

Foci's periodic loop fires these on their own schedules — you don't set them up, but it helps to recognise them when they show up as `via=cron` turns (and to know each is driven by a prompt template you can customise — see **prompts.md**):

| Task | What it does | Customise via |
|------|--------------|---------------|
| **Keepalive** | Periodic liveness / cache-warm turn. A good moment to do tiny housekeeping; otherwise reply `[[NO_RESPONSE]]`. | `keepalive.md`, `[keepalive]` config |
| **Background work** | Idle-time turn for picking up self-directed work when nothing's queued. | `background.md`, `[background]` config |
| **Reflection** | Memory formation — captures facts to your daily file and procedures to skills. Runs on an interval, at session end, and after compaction. Skipped if nothing happened since last time. | `reflection.md`, `[reflection]` config |
| **Consolidation** | Periodically curates `MEMORY.md` from the daily files (a longer interval than reflection). | `memory-consolidation.md`, `[maintenance]` config |
| **Session reset** | Optional soft reset (memory formation + session-key rotation) when a `reset_time` is configured. | `[maintenance]` config |
| **Log rotation** | Built-in: rotates the log files and writes `.gz` archives. No turn — runs inside foci. | — |

**Compaction** is related but *threshold-triggered*, not on a clock — it fires when a session's context grows too large (or on manual `/compact`). See **prompts.md**.

The reflection/consolidation turns are **system-internal**: their output is not delivered to the user, so you don't need to manage what they "send." A normal keepalive or cron turn, by contrast, *does* reach the user if you reply with text — so use `[[NO_RESPONSE]]` when there's nothing to say.

## 2. Creating your own durable scheduled turns

When you want a turn to fire on a schedule you define — a daily check, a weekly digest, a timed reminder-to-self that does real work — use the **user crontab**. It's the one durable mechanism: a backend scheduler such as Claude Code's own cron does **not** survive a restart/reload, so don't rely on it for anything that must persist.

Foci expands `shared/crontab.template` per-agent (substituting the agent ID and home dir, with a stagger offset so agents don't all fire at once) and installs the result into the foci user's crontab. Each entry invokes the `foci` CLI to run a prompt as a one-shot branch session — authenticated over foci's local Unix socket by same-user kernel peer credentials, so no API key sits in the crontab. Editing the crontab is the way to add your own ad-hoc scheduled task; for anything durable, always prefer it over a backend scheduler.

A scheduled turn reaches the user the same way any turn does: reply with text and it's delivered to the platform; reply `[[NO_RESPONSE]]` for a silent check. For a check that should stay quiet unless it trips, have the scheduled command itself decide whether to invoke you at all (see the `silent-cron` skill) rather than waking you every time only to say nothing.
