<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Permission-rule failures

Claude Code evaluates an agent's permission-allow rules to decide whether a tool call proceeds.
When a rule is malformed or ordered wrongly, the failure surfaces a long way from its cause — as a
task failing for no visible reason, not as "your config is wrong."

## `Permission allow rule Write(...) is not matched by file permission checks`

Seen in `~/logs/foci.log` as (the pre-#1962 wording; batch runs now log
`consolidation batch failed: consolidation batch: ...`):

```
[keepalive:<agent>] consolidation RunOnce failed: claude --print failed: exit status 1
(stderr: Permission allow rule (...): Write(<path>) is not matched by file permission checks
 — only Edit(path) rules are...)
```

**Where the transcript is.** Since #1962 a batch run (consolidation, nudge extraction,
foci_summary) is an ordinary stream-json session on an ephemeral child key
(`<agent>/c<id>/b<ns>`, session_type `background-task`), so it DOES leave a CC transcript (until
the ephemeral-session GC) and an api.db row with `purpose` set. Before #1962 it was
`claude --print --no-session-persistence` and left nothing behind. And don't assume the failing task actually tried to write that path: the failure is a
permission-*list* artifact, raised while validating rules, not evidence about the task's content.

**Mechanism.** Claude Code rejects `Write(<path>)` as an invalid rule type for file paths —
`Edit(...)` is meant to cover every file-editing tool, `Write` included. CC evaluates rules in
**list order**, first match for a path wins. So whether an agent is affected is *order*-dependent,
not presence-dependent:

- If `Edit(<path>)` appears **before** `Write(<path>)` in the effective merged list (project
  `.claude/settings.json` + `settings.local.json`, both checked before the shared global
  `~/.claude/settings.json`), the Edit rule matches first, the write succeeds, and the invalid
  Write rule sits there harmlessly.
- If the only rule for that path comes from the shared global file (an agent with no local
  override), or a local file that *also* orders Write before Edit, the invalid rule is hit first
  and the batch run hard-fails.

**Auditing exposure across agents.** Read the *order*, not just the presence:

```bash
jq '.permissions.allow' <agent>/.claude/settings.local.json   # and the project settings.json
```

Cross-reference with
`grep -E '\[keepalive:<agent>\] (firing memory consolidation|consolidation (RunOnce|batch))' ~/logs/foci.log`.
An agent can be **latently exposed** — bad order, but its batch runs simply haven't touched that
path yet — without ever having logged a failure. Absence of a failure is not absence of the bug.

**Fix.** Remove the invalid `Write(path)` lines; `Edit(path)` already grants the same access, per
CC's own error text, so nothing is lost. Note that **editing `.claude/settings.json` /
`settings.local.json` directly is blocked by the auto-mode classifier** — self-modifying permission
config is treated as sensitive — so expect to need explicit user approval, or to have the user make
the edit. Once they confirm in words ("remove the deprecated rules"), the edit goes through.

**If it resurfaces, check the seed first.** The rules are baked into every CC agent's
`--allowedTools` at launch from `internal/config/cc_backend.go`'s `DefaultCCAllowedTools`. That
constant is what would silently reintroduce this for any *newly created* agent, so fixing only the
deployed settings files leaves the trap armed. After checking it, re-run the `jq` order-audit above
across all agents — a new CC release can deprecate a different rule type the same way
(`NotebookEdit(path)` and `Glob(path)` are already flagged in the changelog as the same category).

*(Swept and fixed everywhere found on 2026-07-17: the shared global settings, several per-agent
local overrides, and the `DefaultCCAllowedTools` seed.)*

## "Why is/isn't this command auto-approved?" — read the env a CHILD has, not `/proc/environ`

Beyond rule matching, `internal/execguard` vetoes any command whose executable the foci process
could overwrite — independently of which rule matched, **including the built-in read-only group**.
It resolves bare names against the PATH **at check time**, so it sees what a tool shell sees.

To read a running process's live environment, read it from a **child it spawned**:

```
mp=$(systemctl show -p MainPID --value foci.service)   # pgrep -f foci-gw does NOT find it
tr '\0' '\n' < /proc/$(pgrep -A -P $mp | head -1)/environ | grep '^PATH='
```

**Never `/proc/<the daemon's own pid>/environ`.** That is the environment at *exec* time — and
since #1914 the operator's dotfile env is never applied to the daemon's own environment at all:
it is captured once as a value (`shellenv.Load`) and handed to every agent-shell spawn
(`procx.SetOperatorEnv`). So the daemon's environ never shows the `PATH` a tool shell searches,
no matter when you read it. Reading it gives a confident, wrong answer that looks authoritative;
two independent investigations of the same bug were both misled by it. Agreement between two
readings of one broken instrument is not corroboration.

- **`-check-config` previews the drop again, but only as the service user.** As root it says it
  did not check (root's `access(2)` succeeds for every file) and sources nothing. As a user it
  sources the rc file only when the file and every directory on its path is writable only by the
  invoking user and root — a group-writable home (e.g. mode 2770), a world-writable directory,
  another user's file, a symlink, or a bare relative `shell_env_file` all refuse with one warning.
  A configured rc file that is missing, or that cannot be sourced, is not a refusal (startup gets
  no overlay from it either, so the verdicts agree) but gets its own warning naming the PATH judged.
  In the refused states read the startup log instead.
- **Built-in rule groups appear in both reports.** Startup logs one WARN per substitutable
  built-in (`CommonReadonlyRules`/`CommonSafeWriteRules` commands, named once per group and
  command — six read-only rules share the token `go`, and `go` is one line), and `-check-config`
  lists them with a summary count — group, command, reason and path in both. The groups are
  reported only for the agents they are live for (delegated backends that still ask for approval,
  which is where `buildAutoApproveRules` runs; an API agent or a `skip_permissions` agent cannot
  stop auto-approving and is never named). Only substitutable commands are named; a built-in whose
  tool is not installed on the host (`ack`, `yq`) is silent.
- **Only the FIRST WORD of each `&&`/`||`/`;`/`|` segment is inspected.** Arguments are never
  checked, so wrapping a script in an interpreter restores the auto-approval without restoring the
  safety. Do not "fix" a vetoed entry that way.

- **The guard only governs commands an *agent* issues.** foci-gw's own launches (`bash`, `tmux`,
  `go`, `git`, by bare name via `procx.Spawn`) get no approval prompt and no substitutability
  check. If you are asking "why wasn't this checked", first ask *who ran it* — the daemon's own
  execution path is outside every control foci has.
