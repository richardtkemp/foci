---
name: foci-config
description: Read foci's config or change a setting in foci.toml. foci.toml is read-only to agents; `foci config set` asks the user to approve one change in chat and waits for the answer. Use whenever a task needs a config value read or changed — never edit foci.toml directly.
owner: foci
seeded: true
---

> **This `SKILL.md` is yours to customise** (seed-if-missing — override it, add your own sibling files). Changes to it survive a restart, but they live only on this install: to change the skill for every agent, edit it in the foci repo (`shared/skills/foci-config/`).

# foci-config — reading and changing foci's config

## Why direct edits fail

foci.toml is owned by root and writable only by the gateway (`root:foci-secrets 0664`). You can read it; any write from your tools, your shell or a script fails with "permission denied", and the gateway refuses to start on a file anyone else has made writable. That is by design: a setting changes only when the user approves it.

## When to use it

- You need a config value: `foci config get` (what the file says) or `foci config show` (the running config).
- A task needs a setting changed (a model, a nudge, a path, an agent's option): `foci config set`, or `foci config unset` to fall back to the default.
- To check a config is valid without changing anything: `foci check config` (the `foci-usage` skill covers `foci check`).

Ask for one change per request. Say in chat why you are asking before you run it, so the user knows what the prompt is for.

## Operations

| Command | What it does |
|---|---|
| `foci config show` | The running config, secrets redacted (the same as `/config toml`). |
| `foci config get <path>` | The value at `<path>` in the config file, or `<path> is unset` (its default applies). |
| `foci config set <path> <value>` | Ask the user to approve setting `<path>` to `<value>`; waits for the answer. |
| `foci config unset <path>` | Ask the user to approve removing `<path>`; waits for the answer. |
| `foci config add <path> <value>` | Ask the user to approve appending one element to the array at `<path>`; waits for the answer. |
| `foci config wait <id>` | Follow a request made earlier, e.g. one `/config set` made over the HTTP API. |

Flags: `--wait <duration>` sets how long the user has to answer (default 10m, at most 1h); `--session <key>` overrides `$FOCI_SESSION_KEY`; `--config <file>` points `get` at another file.

## Paths

TOML dotted keys. `agents[<id>]` selects one agent's `[[agents]]` block. Quote a segment that is not a bare key.

```
agent_loop.max_tool_loops          a key in a table
master_agent                       a top-level key
agents[main].model                 a key in agent "main"
agents[main].nudge.enable          a nested key in agent "main"
models."claude.fast".id            a quoted segment
blocked_paths                      a whole array of tables
agents[main].platforms             one agent's array of tables
blocked_paths[etc]                 ONE element: the table whose name (else id) is "etc"
rules[tool=Bash]                   ONE element: the table whose tool is "Bash"
rules[3]                           ONE element: index 3, counting from 0
paths["/tmp/x"]                    ONE element: the string "/tmp/x" (quote non-bare values)
```

To change one element of an array, name it with a selector instead of resending the whole array: `set` replaces just that element, `unset` removes it (and the comment lines directly above it), and the user's prompt shows only that element. `add` appends one.

## Values

TOML value syntax. Quote the whole value for your shell, and quote a path with `agents[...]` in it (the brackets are a shell glob).

```bash
foci config set agent_loop.max_tool_loops 20
foci config set 'agents[main].model' '"opus"'                     # a string needs its TOML quotes
foci config set 'agents[main].nudge.enable' false
foci config set reflection.interval '"2h"'
foci config set 'agents[main].system.system_files' '["character/SOUL.md", "character/CRAFT.md"]'
foci config set blocked_paths '[{path = "/etc"}, {path = "/root"}]'   # replaces the whole array
foci config unset 'agents[main].effort'
foci config unset 'agents[main].backend_config.pretool_rules[old_rule]'     # remove one rule
foci config set 'agents[main].backend_config.pretool_rules[old_rule]' '{name = "old_rule", tool = "Bash", command = "x", reason = "y"}'
foci config add 'agents[main].backend_config.pretool_rules' '{name = "new_rule", tool = "Bash", command = "x", reason = "y"}'
```

A set of a whole array replaces every entry: list all the entries you want to keep. Only the entries that differ are rewritten, so the others keep their layout and comments. For one entry, prefer a selector.

## What the user sees

A message in their chat with **Approve** and **Deny** buttons: the session asking, the setting, its current value and the new value. Nothing changes unless they approve. Before the prompt is shown, foci checks that the change touches only that setting and that the changed file passes `foci check config`; a change that fails either check is refused at once and the user never sees it.

## Outcomes

The command prints one line and exits 0 only for the first:

| Outcome | Meaning |
|---|---|
| `applied: <path> = <new> (was <old>)` | Approved and written. Live-appliable settings take effect now; others on the next restart. |
| `denied: ...` | The user said no. Do not ask again for the same change unless they tell you to. |
| `timed out: ...` | Nobody answered in time. Nothing changed. |
| `failed: <reason>` | Refused or not written: an invalid path or value, a change the config check rejects, the setting changed since you asked, or the gateway restarted. Read the reason. |

## /config set over HTTP

`foci command "/config set k=v"` (or `/config unset`) from your shell does not write either: it creates the same approval request and replies at once with the request id. Follow it with `foci config wait <id>`. The `/config set` wizard, `/agents new` and `/android` write foci.toml and can only be started from chat. `foci command "/agents rename OLD NEW"` sends the user an Approve/Deny prompt for the rename (with its plan); nothing changes unless they approve.
