# Ask Gateway (askgw)

A local Unix-socket server that lets any application on the host forward questions to the foci user's chat — presenting them as interactive multiple-choice prompts the human can answer inline (buttons on Telegram, selection UI on the Android app). The app blocks on the socket until the human responds (or the question times out).

## Why

Some applications need human judgement but run unattended — as daemons, cron jobs, or automated workflows. Without askgw, they'd need their own notification channel, authentication, and UI. Askgw reuses foci's existing interactive-button transport: the app sends a question over a local socket, foci surfaces it wherever the user already is (Telegram, Discord, the Android app), and routes the answer back.

Example: [ai-sudo](https://github.com/richardtkemp/ai-sudo) uses askgw to forward sudo approval requests to the user's phone instead of blocking a terminal no one is watching.

## Security

**Socket ownership:** The socket is owned by group `foci-askgw` (configurable), mode `0660`. The parent directory must not be group- or world-writable.

**Two-layer access control:**
1. **Unix group membership** — the connecting process must be in the `foci-askgw` group (or be the foci user).
2. **UID allow-list** — `allowed_uids` in config; accepts usernames or numeric UIDs. The connecting process's UID (checked via `SO_PEERCRED`) must be in this list.

**Agent isolation:** foci's child agent subprocesses have the `foci-askgw` group stripped via `procx` credential dropping — they cannot reach the socket, preventing an agent from asking itself questions in a loop.

## Configuration

```toml
[askgw]
enabled = true
socket_path = "/home/foci/data/askgw.sock"   # default: <data>/askgw.sock
group = "foci-askgw"                          # default
allowed_uids = ["root", "1000"]               # required when enabled; usernames or UIDs
default_agent = "arnix"                       # which agent to route questions to
default_timeout_seconds = 120                 # 0 = no timeout
max_frame_bytes = 1048576                     # 1 MiB default
```

**Required when enabled:** `allowed_uids` (at least one UID), `default_agent` (must match a configured agent).

The group (`foci-askgw`) is created at install time by `make provision`. The foci gateway process runs with `SupplementaryGroups=... foci-askgw`.

## Using askgw from a client application

Connect to the Unix socket, send an `ask` frame as a single line of JSON, and read the `answer` frame back. The protocol is `askgw/1` — see the [askgw protocol reference](https://github.com/richardtkemp/ai-sudo/blob/main/docs/askgw-protocol.md) for full frame specifications.

```bash
#!/bin/bash
# Send a yes/no question and wait for the answer
{
  printf '%s\n' '{"protocol":"askgw/1","type":"ask","id":"deploy-001","source":"deploy-bot","questions":[{"key":"go","question":"Deploy v1.2.3?","options":[{"label":"Yes"},{"label":"No"}]}]}'
  # Wait for the answer frame (blocks until human responds or timeout)
  read -r answer
  echo "$answer" | jq -r '.status // .code'
} | nc -U /home/foci/data/askgw.sock
```

## Limitations

- **No persistence across restarts.** Socket connections die on foci restart. Answers can only reach the original connection — there's no way to deliver a response after a reconnect.
- **No streaming or long-polling.** One `ask` → one `answer` (or timeout/error). The app blocks on the socket read.
- **Agent routing is session-based.** Questions route to the agent's default (most recently active) session. If the agent has no active session, the question returns `unavailable`.
