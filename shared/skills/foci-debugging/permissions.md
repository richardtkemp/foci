<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Permission-rule failures

A malformed or misordered allow rule surfaces far from its cause: a task fails for no visible reason.

## `Permission allow rule Write(...) is not matched by file permission checks`

Seen in `~/logs/foci.log` as (the pre-#1962 wording; batch runs now log
`consolidation batch failed: consolidation batch: ...`):

```
[keepalive:<agent>] consolidation RunOnce failed: claude --print failed: exit status 1
(stderr: Permission allow rule (...): Write(<path>) is not matched by file permission checks
 — only Edit(path) rules are...)
```

**Where the transcript is.** A batch run (consolidation, nudge extraction, foci_summary) is a
stream-json session on an ephemeral child key (`<agent>/c<id>/b<ns>`, session_type
`background-task`), so it leaves a CC transcript (until ephemeral-session GC) and an api.db row with
`purpose` set (#1962). The failing task need not have tried to write that path: the error comes from
validating the rule list, not from the task's content.

**Mechanism.** CC rejects `Write(<path>)` as a rule type for file paths (`Edit(...)` covers every
file-editing tool). CC evaluates rules in **list order**; the first match for a path wins. So exposure
depends on *order*, not presence:

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

**Fix.** Remove the `Write(path)` lines; `Edit(path)` grants the same access. **The auto-mode
classifier blocks edits to `.claude/settings.json` / `settings.local.json`**: you need the user's
explicit approval in words ("remove the deprecated rules"), or the user makes the edit.

**If it resurfaces, check the seed first.** The rules are baked into every CC agent's
`--allowedTools` at launch from `internal/config/cc_backend.go`'s `DefaultCCAllowedTools`. That
constant is what would silently reintroduce this for any *newly created* agent, so fixing only the
deployed settings files leaves the trap armed. After checking it, re-run the `jq` order-audit above
across all agents — a new CC release can deprecate a different rule type the same way
(`NotebookEdit(path)` and `Glob(path)` are already flagged in the changelog as the same category).

## "Why is/isn't this command auto-approved?" — read the env a CHILD has, not `/proc/environ`

Beyond rule matching, `internal/execguard` vetoes any command whose executable the foci process
could overwrite — independently of which rule matched, **including the built-in read-only group**.
It resolves bare names against the PATH **at check time**, so it sees what a tool shell sees.

To read a running process's live environment, read it from a **child it spawned**:

```
mp=$(systemctl show -p MainPID --value foci.service)   # pgrep -f foci-gw does NOT find it
tr '\0' '\n' < /proc/$(pgrep -A -P $mp | head -1)/environ | grep '^PATH='
```

**Never `/proc/<the daemon's own pid>/environ`.** It is the env at exec. The operator's dotfile env
is captured once (`shellenv.Load`) and handed only to agent-shell spawns (`procx.SetOperatorEnv`,
#1914), so the daemon's environ never shows the `PATH` a tool shell searches. It looks authoritative
and is wrong: it misled two independent investigations of one bug.

- **`-check-config` previews the drop only as the service user.** As root it skips the check. As a
  user it sources the rc file only if the file and every directory on its path is writable by
  that user and root alone; otherwise (group/world-writable, foreign owner, symlink, `..`,
  relative path) it refuses with one warning; then read the startup log. A missing or unsourceable
  rc file is not a refusal: it warns and judges the bare PATH, as startup does.
- **Built-in groups are reported too**: one WARN per substitutable built-in command, only for
  agents that still prompt (delegated, not `skip_permissions`). Uninstalled tools are silent.
- **Only the FIRST WORD of each `&&`/`||`/`;`/`|` segment is inspected.** Arguments are never
  checked, so wrapping a script in an interpreter restores the auto-approval without restoring the
  safety. Do not "fix" a vetoed entry that way.

- **The guard only governs commands an *agent* issues.** foci-gw's own launches (`bash`, `tmux`,
  `go`, `git`, by bare name via `procx.Spawn`) get no approval prompt and no substitutability
  check. If you are asking "why wasn't this checked", first ask *who ran it* — the daemon's own
  execution path is outside every control foci has.
