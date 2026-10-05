<!-- GOLDEN: ships with foci (shared/skills/foci-usage/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# PreToolUse rules — calls foci refuses before they run

Applies to **Claude Code agents** (`backend = "claude-code"`) only. Codex, opencode and API-loop agents have no rule engine.

foci checks every call to a named tool against a set of **deny rules** before the call runs. If a rule matches, the call does not run. You get the rule's `reason` as the tool's error result, usually as `PreToolUse:<Tool> hook error: ...`. Rules can only deny. There is no allow rule, because a hook allow would bypass foci's permission prompts.

Two rules ship preinstalled. `ask_user_question` redirects CC's `AskUserQuestion` to `foci_ask`. `cron_create` redirects CC's `CronCreate` to crontab or `foci_remind`, because CC's cron jobs are lost on restart, reload or compaction. Everything else comes from `foci.toml`, either for every CC agent (`[[cc_backend.pretool_rules]]`) or for one agent (`[[agents.backend_config.pretool_rules]]`).

## When a call is denied

- **Read the reason and do what it says.** A good reason names the alternative: `git -C` instead of `cd`, the `run_in_background` parameter instead of `&`, a worktree instead of main.
- **Do not retry the same call, and do not rephrase it to slip past the pattern.** The rule exists because someone decided against that call. A workaround that dodges the regex breaks the rule's intent.
- **A deny that looks wrong is a finding.** Check it with `foci pretool test ... -v` (below). If the rule really misfires, report it to the user or file a todo with the exact command. Do not edit the rule mid-task to get your call through.

## Seeing and testing your rules

The `foci` CLI checks rules offline, against the config file, with no session needed:

```bash
foci pretool list                                   # your resolved rules (--agent defaults to you)
foci pretool test --bash 'cd /r && git add -A' -v    # which rule denies it, and why
foci pretool test --bash 'git commit -m x' --cwd /home/rich/git/foci -v
foci pretool test --tool Edit --input '{"file_path":"/home/foci/shared/skills/x/SKILL.md"}'
```

`test` prints the denying rule's name, or `no match`. `-v` adds the reason and, for Bash, each simple command the patterns see, with its shell facts (background, subshell, output, directory, operator, pipe). `when` checks run for real, in `--cwd`.

## Writing a rule

Only when the user asks you to (rules change how an agent may act, often how *other* agents may act). The full reference is **`docs/CONFIG.md` → "PreToolUse rules"** in the foci repo, deployed to `~/shared/docs/CONFIG.md`. Read it before writing one. The short version:

```toml
[[agents.backend_config.pretool_rules]]
name = "no_force_push"                          # unique; reusing a name overrides that rule
tool = "Bash"                                   # exact tool name
command = 'git (\S+ )*push (\S+ )*--force'      # regex, or a list of them
reason = "Force-pushing is not allowed. Push normally, or ask the user."
```

- **`command`** (Bash only) is matched from the start of **each simple command** of the parsed script, with quotes removed. So `git add` matches inside `cd /r && git add .`, but not inside `echo git add` or a commit message. `(\S+ )*` means "any words".
- **`input.<field>`** matches a raw tool-input field, e.g. `input.file_path` for `Read`/`Edit`/`Write`.
- **`background` / `subshell` / `output`** (Bash only) match parsed shell structure. Use them instead of regexing `input.command` for `&`, `cd` or pipes. Raw text cannot tell a real `&` from one in a heredoc or a quoted string.
- **`when`** is a bash script for live state (the branch, uncommitted edits, a file's header). The rule denies **only if it exits 0**. Exit 1 is "no". Any other exit status or a timeout **fails open** (the call runs and a WARN is logged). Map other statuses explicitly, e.g. `git diff --quiet` exits 1 when there ARE changes. Keep it fast and read-only: it runs on every matching call, with a 2 s limit.
- **Set `enabled = false`** under a rule's `name` to switch it off for one agent.
- **An unknown key is a config load error**, not a warning. A misspelled constraint would otherwise be dropped, and the rule would deny every call to its tool.

**Test before you rely on it.** Run `foci pretool test` with a call the rule should deny and one it should allow, and check both results. A rule you have only seen pass proves nothing.

**When it takes effect.** Rules are read each time foci launches a CC process: a new session, or a resume after idle shutdown or a foci restart. **A CC process that is already running, including your own current one, keeps the rules it launched with.** So `foci pretool test` shows what the next launch will enforce, not what applies to your live session now. If the config fails to load, a running gateway keeps the last good rules.

Every deny is logged as `pretool_rule_deny`.

For the end-of-turn sibling, **stop rules** (which block a turn that ends with a "starting X now" reply and no background job launched), see `docs/CONFIG.md` → "Stop rules".
