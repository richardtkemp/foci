<!-- GOLDEN: ships with foci (shared/skills/foci-debugging/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Permission-rule failures

Claude Code evaluates an agent's permission-allow rules to decide whether a tool call proceeds.
When a rule is malformed or ordered wrongly, the failure surfaces a long way from its cause — as a
task failing for no visible reason, not as "your config is wrong."

## `Permission allow rule Write(...) is not matched by file permission checks`

Seen in `~/logs/foci.log` as:

```
[keepalive:<agent>] consolidation RunOnce failed: claude --print failed: exit status 1
(stderr: Permission allow rule (...): Write(<path>) is not matched by file permission checks
 — only Edit(path) rules are...)
```

**First, don't go looking for a transcript.** `RunOnce` — used by consolidation, nudge extraction
and onboarding — calls `claude --print --no-session-persistence`, so **nothing is left behind**.
And don't assume the failing task actually tried to write that path: the failure is a
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
  and `RunOnce` hard-fails.

**Auditing exposure across agents.** Read the *order*, not just the presence:

```bash
jq '.permissions.allow' <agent>/.claude/settings.local.json   # and the project settings.json
```

Cross-reference with
`grep -E '\[keepalive:<agent>\] (firing memory consolidation|consolidation RunOnce)' ~/logs/foci.log`.
An agent can be **latently exposed** — bad order, but its RunOnce tasks simply haven't touched that
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

**Never `/proc/<the daemon's own pid>/environ`.** That is the environment at *exec* time and is
never updated by `os.Setenv` — and `main()` calls `shellenv.Apply()`, which sets the operator's
dotfile env over the unit's. Reading it gives a confident, wrong answer that looks authoritative;
two independent investigations of the same bug were both misled by it. Agreement between two
readings of one broken instrument is not corroboration.

- **`-check-config` no longer previews dropped entries.** The hygiene pass had to move after
  `shellenv.Apply`, and as root every `access(2)` succeeds so the preview was lying anyway. The
  per-entry warnings fire at **startup** — read the log, not the pre-flight.
- **Built-in rule groups never appear in that report at all.** `CommonReadonlyRules` is assembled
  at backend rule-build time, not from `cfg.Permissions.AutoApprove`. The match-time veto still
  covers them, so this is a reporting gap — but a built-in can stop working with no warning.
- **Only the FIRST WORD of each `&&`/`||`/`;`/`|` segment is inspected.** Arguments are never
  checked, so wrapping a script in an interpreter restores the auto-approval without restoring the
  safety. Do not "fix" a vetoed entry that way.

- **The guard only governs commands an *agent* issues.** foci-gw's own launches (`bash`, `tmux`,
  `go`, `git`, by bare name via `procx.Spawn`) get no approval prompt and no substitutability
  check. If you are asking "why wasn't this checked", first ask *who ran it* — the daemon's own
  execution path is outside every control foci has.
