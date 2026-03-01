# Code Review: Overnight Commits (618eed20..HEAD)

## Task
Review all Go code changes between `618eed20` and HEAD (~2,173 insertions, 266 deletions across 25 files). These were written overnight by OpenCode (GLM-5) and background spawn agents, with minimal human review.

## Review Criteria
- **Correctness**: Does the code do what it claims? Edge cases handled?
- **DRY**: Any duplicated logic that should be extracted?
- **Simplicity**: Over-engineered? Simpler approach available?
- **Elegance**: Clean interfaces, good naming, idiomatic Go?
- **Bugs introduced**: Race conditions, nil derefs, error handling gaps?
- **Test quality**: Do tests actually test the right things? Brittle?

## Commits by Purpose

### Security
- `99ba1da4` — **#214: Block read/write/edit from secrets.toml.** File tools had zero path restrictions while exec properly blocked. Added IsBlockedPath() checks.

### Bug Fixes
- `6f1b806c` — **#215: Multiball display settings.** Shared pool bots got display settings at startup but never updated when acquired by an agent. Extracted `applyAgentDisplaySettings` helper.
- `675ff417` — Tests for the above.
- `82453893` — **#141: Tmux bare send-keys.** Allow empty keys when enter=true (send bare Enter).
- `33c9e3e2` — **#213: Flaky TestTmuxBraindeadAutoUnwatch.** `lastContent` started as zero-value so first poll always reset activity timer. Fix: capture initial hash.
- `63f8f6b2` — **#206: Remove misleading 'cannot compact' warning.** No_compact sessions showed stale "can't compact" warnings.
- `33eb90ed` — Fix #153 test failures (RFC3339 second precision).
- `5d73ac85` — Fix test for compaction_effort in config display.
- `ef72876a` — **#130: Multiball routing.** Use BotForSession for multiball sessions in async_notify and sessionNotifyFn. (Note: another background session said #130 was already fixed — verify this commit is needed and doesn't duplicate existing logic.)
- `3e8a54cb` — **#207: Stale context in /status.** Mark compaction API calls in log, skip in /status context display.

### Features
- `469ec4e0` — **#153: Todo timestamps.** Added updated_at column, relative time display.
- `92483390` — **#154: Batch todo operations.** ids array param for complete/edit/remove.
- `c4d80c8a` — **#216: Compaction effort override.** `effort` field in `[agents.compaction]` config.
- `95551eee` — **#151: Environment block visibility.** Show resolved show_tool_calls/show_thinking in env block.
- `a698f977` — **#41: Startup crash diagnosis.** New `startup/` package — clean/crash/reboot classification, log scanning.
- `65319d75` — **#167: Spawn isolation.** None-mode spawns get temp dirs, file tools restricted via BaseDir.

### Docs
- `ef55cc3f` — **#4: README + MULTIBALL.md + CACHING.md.** New feature docs, expanded README docs section.
- `82365b55` — **#209: INSTALL.md.** End-to-end installation guide.
- `7f93a3db` — **#210: foci.toml.example + secrets.toml.example.** Comprehensive rewrite with all sections.
- `3676e112` — **#212: Skills audit.** docs/skills-audit.md — which skills ship with foci.
- `198b62cd` — **#150: GETTING-STARTED.md.** Install-to-first-conversation walkthrough. Also includes first-run onboarding task spec.
- `0ad22f16` — **#160: Anthropic web search task spec.**
- `bfd23226` — **#207: Stale context task spec.**
- `e6b22f0a` — Task specs and codebase review docs.

Review docs for: accuracy, completeness, consistency with actual code behaviour, broken links, outdated references.

### Benchmark
- `ff74fd13`, `ee2b7f0e`, `ecb4b935`, `7c713054` — **#96: Compaction benchmark harness.** 201 loading messages, 50 quiz questions, 3 quiz modes. In `benchmark/`.

Review for: script correctness, quiz question quality, whether the scoring logic makes sense.

## How to Review

```bash
# See all Go changes:
git diff 618eed20..HEAD -- '*.go'

# Per-commit review:
git log --oneline 618eed20..HEAD -- '*.go'
# then: git show <hash> -- '*.go'
```

## Output
Write findings to `docs/review-overnight.md`. For each issue found:
1. File and line
2. What's wrong
3. Suggested fix

Group by severity: critical (bugs/security), moderate (DRY/design), minor (style/naming).

If you find issues that need fixing, list them but don't fix them — we'll decide what to address before deploying.
