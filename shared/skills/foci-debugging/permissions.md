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
