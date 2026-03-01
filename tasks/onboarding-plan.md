# First-Run Onboarding — Implementation Plan

## Current State

**What exists:**
- `setup.sh` — the entry point for installation. Creates system user, builds binaries, creates dirs, generates config, installs systemd service. Has several bugs (see `docs/setup-review.md`): character files never created on fresh install (Step 5 condition is wrong), secrets.toml uses wrong key name (`token` instead of `setup_token`), generated config uses `[agent]` instead of `[defaults]` + `[[agents]]`.
- `tasks/first-run-onboarding.md` — existing design for first-run detection using a `first_run_completed` flag in state.db + injecting a `[FIRST RUN]` system message. Designed but never implemented.
- `docs/GETTING-STARTED.md` — covers install → first message. Step 5 claims the agent will "introduce itself and explain it's brand new" on first run. **This is aspirational documentation — no such behavior is implemented.** There is no first-run prompt, no onboarding detection, and no code that injects introductory behavior. The agent just responds to messages against whatever character files exist.
- `docs/INSTALL.md` — detailed server install guide, assumes Linux, sudo, systemd.
- `foci.toml.example` / `secrets.toml.example` — well-documented example configs.
- `shared/defaults/character/` — 5 template files (SOUL.md, CRAFT.md, COHERENCE.md, USER.md, MEMORY.md) with HTML comment placeholders. These are the intended starting point for new agents.
- Config loading (`config.Load`) — **fatal exits** if config file missing. No first-run detection. No auto-creation.
- Workspace bootstrap — silently skips missing character files (warns only).
- Secrets loading — **fatal exits** if `secrets.toml` missing.
- Model aliases — already supported via `[models]` config section. Defaults: `"opus" → "claude-opus-4-6"`, `"sonnet" → "claude-sonnet-4-6"`, `"haiku" → "claude-haiku-4-5"`. The alias `claude-sonnet-4-6` (no date suffix) is Anthropic's stable pointer to the latest Sonnet 4.6.x release.

**What's missing:**
- No way to start foci without pre-created config files.
- No in-binary first-run wizard — setup.sh prompts for tokens via bash `read`, then generates config via heredoc. This should be a proper Go wizard that setup.sh invokes.
- No first-run onboarding behavior after the bot connects (the `tasks/first-run-onboarding.md` design exists but is unimplemented).
- Character file defaults exist in `shared/defaults/character/` but nothing copies them to a new agent workspace (setup.sh has its own inline templates that differ from these defaults, and the copy is broken anyway due to the Step 5 bug).
- No auto-capture of the user's Telegram ID.

## Design

### Philosophy

Follow the SPEC: **simple over powerful**, **explicit over clever**. The user should go from `git clone` to a working Telegram conversation in under 5 minutes.

### setup.sh Is First-Class

`setup.sh` is the entry point for all installations. The flow is:

1. Clone repo
2. Run `sudo ./setup.sh -u foci`
3. setup.sh does system setup (user, dirs, systemd, permissions) then invokes the in-binary onboarding wizard for config generation
4. Service starts, user messages their bot on Telegram

The in-binary wizard (`foci setup`) is a subcommand that setup.sh calls after system setup is complete. It can also be run standalone for re-configuration, but setup.sh is the expected first-run path.

### Two-Phase Onboarding

**Phase 1: Terminal — `setup.sh` + `foci setup`** (get the bot online)
setup.sh handles system-level work (user, dirs, systemd, secrets permissions), then invokes `foci setup` running as the foci user for interactive config generation.

**Phase 2: Telegram — first-run agent prompt** (configure the agent's identity)
Once the bot is connected, a one-time system message injection guides the agent through introducing itself and learning about the user.

## User Flow

### Phase 1: `setup.sh` + `foci setup`

```
$ sudo ./setup.sh -u foci
[+] Step 1: System user
  User foci exists
[+] Step 1b: Secrets group (foci-secrets)
  Group foci-secrets exists
[+] Step 2: Build binaries from source
  Building focigw, foci, foci-call...
  Installed to /usr/local/bin
[+] Step 3: Directories
  Directories ready
[+] Step 4: Config
  No config found — launching setup wizard...

──────────────────────────────────────────
  Foci First-Run Setup
──────────────────────────────────────────

Step 1: Telegram Bot
  Create a bot via @BotFather on Telegram (https://t.me/BotFather)
  Send /newbot, follow the prompts, and paste the token here.

Bot token: 123456789:AAF-...
✓ Bot token validated.

Step 2: Your Telegram User ID
  Starting bot temporarily to detect your user ID.
  Send any message to your bot on Telegram now...

  Waiting for message... ✓ Got it.
  User ID: 12345678 (from: Rich)

Step 3: Anthropic Authentication
  Foci needs access to Claude. Choose one:
  [1] OAuth (recommended for Max/Pro subscribers)
  [2] API key
  [3] Skip (use Claude Code credentials if available)

> 1
  Opening browser for OAuth...
  (existing `foci auth` flow)
✓ Authenticated.

Step 4: Agent ID
  Pick a short lowercase name for your agent (letters, numbers, hyphens).
  This becomes the agent's workspace directory and session key prefix.

Agent ID [main]: fotini
✓ Agent ID: fotini

Step 5: Character Files
  Do you have existing character files to import?
  [1] No — use defaults (recommended for new users)
  [2] Yes — import from a directory

> 2
  Directory path: /home/rich/openclaw/character/

  Found 12 .md files:
    1. [x] SOUL.md (4.2 KB)
    2. [x] CRAFT.md (6.1 KB)
    3. [x] COHERENCE.md (3.8 KB)
    4. [x] USER.md (2.1 KB)
    5. [x] MEMORY.md (8.4 KB)
    6. [ ] README.md (0.5 KB)
    7. [ ] CHANGELOG.md (12.3 KB)
    8. [ ] ARCHITECTURE.md (5.7 KB)
    9. [ ] TODO.md (1.2 KB)
   10. [ ] NOTES.md (3.3 KB)
   11. [ ] SCRATCH.md (0.8 KB)
   12. [ ] IDEAS.md (2.0 KB)

  Known character files are pre-selected. Toggle with number, 'a' for all, Enter to confirm.
> 6
    6. [x] README.md (0.5 KB)
> [Enter]
✓ Imported 6 files to /home/foci/fotini/character/

Creating config...
  → /home/foci/config/foci.toml
  → /home/foci/config/secrets.toml (root:foci-secrets, 0660)
  → /home/foci/fotini/character/ (6 files)
✓ Setup complete.

──────────────────────────────────────────

[+] Step 5: Hardening secrets.toml
  secrets.toml already hardened (root:foci-secrets, 0660)
[+] Step 6: systemd service
  Service installed and enabled
[+] Step 7: Polkit rule
  Polkit rule exists
[+] Step 8: Service
  Starting foci

[+] Done.
  Status:  systemctl status foci
  Logs:    journalctl -u foci -f
  Now message your bot on Telegram — it will introduce itself.
```

**How user ID auto-capture works:**

Instead of asking the user to find their ID via @userinfobot, the wizard:
1. Starts the Telegram bot in a temporary polling mode (no agent loop — just the raw bot API)
2. Prints "Send any message to your bot on Telegram now..."
3. Waits for the first incoming message (with a timeout, e.g. 2 minutes)
4. Extracts the user ID and display name from the message
5. Confirms with the user: `User ID: 12345678 (from: Rich)`
6. Stops the temporary bot

This eliminates a manual step and removes a common source of errors (wrong user ID).

**How `setup.sh` invokes the wizard:**

In setup.sh Step 4, instead of the current bash `read` prompts and heredoc config generation:

```bash
# Step 4: Config
if [[ -f "$FOCI_HOME/config/foci.toml" ]]; then
    info "  Config exists, not touching it"
else
    info "  No config found — launching setup wizard..."
    # Run as foci user, with secrets group for writing secrets.toml
    sudo -u "$FOCI_USER" -g "$SECRETS_GROUP" \
        foci setup \
        --config-dir "$FOCI_HOME/config" \
        --home "$FOCI_HOME" \
        --defaults-dir "$SCRIPT_DIR/shared/defaults/character"
fi
```

The wizard writes both `foci.toml` and `secrets.toml`. setup.sh then hardens permissions in Step 4b as it already does.

**Character file import — selective copy:**

When importing from a directory, the wizard:
1. Lists all `.md` files found in the directory
2. Pre-selects files matching known character file names (SOUL.md, CRAFT.md, COHERENCE.md, USER.md, MEMORY.md)
3. Presents a toggleable checklist — user can add/remove files by number
4. Copies only selected files to the agent's `character/` directory

This prevents blindly importing READMEs, changelogs, or other unrelated markdown.

### Phase 2: Telegram Onboarding

**First-run detection** uses a `first_run_completed` flag in the agent's state store (`data/state.db`), as designed in `tasks/first-run-onboarding.md`. This is a proper persistent flag — not content parsing of character files.

**On first startup (flag not set):**

A one-time system message is injected before the first user message, loaded from an embedded prompt file (`prompts/first-run.md`):

```
[FIRST RUN] This is your first session. Your character files are templates
waiting to be filled in.

Guide your human through initial setup:
1. Introduce yourself — you're new, your character files are blank templates
2. Ask their name and how they'd like to communicate
3. Ask what they'd like to call you
4. Learn about them — interests, work, communication style, timezone
5. Update the character files (SOUL.md, USER.md, MEMORY.md) as you learn
6. Confirm what you've written and ask if anything needs adjusting

Be warm but not sycophantic. This is the start of a relationship.
```

This is injected the same way `WELCOME.md` is injected for updates — a one-time message appended to the session before the first user message is processed.

**Completion:** After the agent has updated at least SOUL.md and USER.md (detected by checking file modification times, or after N turns), set `first_run_completed = true`. The onboarding message never appears again.

**Migration for existing installs:** On first startup after deploying this feature, if `first_run_completed` is not set, check whether character files have been modified from the defaults (compare against `shared/defaults/character/`). If they differ, auto-set the flag silently. This prevents existing agents from getting the onboarding prompt.

### Generated Config

**`foci.toml`:**

```toml
[defaults]
model = "claude-sonnet-4-6"

[[agents]]
id = "fotini"
system_files = [
  "character/SOUL.md",
  "character/CRAFT.md",
  "character/COHERENCE.md",
  "character/USER.md",
  "character/MEMORY.md",
]

[telegram]
allowed_users = ["12345678"]

[sessions]
compaction_threshold = 0.8

[http]
port = 18791
bind = "127.0.0.1"
```

**`secrets.toml`:**

```toml
[anthropic]
# Populated by foci auth (OAuth) or manually (setup_token)
oauth_access_token = "..."
oauth_refresh_token = "..."
oauth_expires_at = 1772334580401

[telegram.bots.fotini]
token = "123456789:AAF-..."
```

**Model default:** Uses `claude-sonnet-4-6` — Anthropic's stable alias that always points to the latest Sonnet 4.6.x snapshot. No date suffix, no hardcoded stale ID. The existing model alias system (`[models]` config section) already maps `"sonnet" → "claude-sonnet-4-6"` by default, so this is consistent.

**Note on dynamic model discovery:** Anthropic has a `/v1/models` list endpoint, but it's not needed here. Their versioning scheme uses stable aliases without date suffixes (e.g. `claude-sonnet-4-6`) that always point to the latest patch. Querying the API at startup would add latency and a failure mode for zero benefit. If the model family changes (e.g. Sonnet 5), the user can update their config.

## Files to Change

### New Files

| File | Purpose |
|------|---------|
| `cmd/foci/setup.go` | `foci setup` subcommand — the interactive wizard. Prompts for bot token, auto-captures user ID, runs auth flow, gets agent ID, handles character file import/seeding, generates config files. |
| `cmd/foci/setup_test.go` | Tests for config generation, token validation, directory creation, character file selection logic. |
| `prompts/first-run.md` | Embedded onboarding prompt injected on first session. Tells the agent to introduce itself and learn about the user. |

### Modified Files

| File | Change |
|------|--------|
| `setup.sh` | **Step 4 rewrite:** Replace bash `read` prompts and heredoc config generation with invocation of `foci setup`. Fix Step 5 bug (character files never created) by removing it — character file creation moves into `foci setup`. Fix secrets.toml key name (`token` → `setup_token` path removed; wizard writes proper structure). Update `--help` text to mention OAuth as the primary auth path. |
| `config/config.go` | Add `GenerateDefault(opts SetupOptions) (configTOML string, secretsTOML string)` function that produces valid config from structured input. This is what the wizard calls — testable in isolation, replaces the heredoc in setup.sh. |
| `config/config_test.go` | Tests for `GenerateDefault` — verify output parses correctly, required fields present, secrets in right format. |
| `main.go` | Add first-run detection in the agent message path: check `first_run_completed` flag in state store; if not set, inject the first-run prompt (from `prompts/first-run.md`) before the first user message. Add migration check for existing installs. Similar pattern to existing WELCOME.md injection. |
| `docs/GETTING-STARTED.md` | Rewrite to match the actual flow: clone → `sudo ./setup.sh` → message bot → agent guides you through identity setup. Remove the false claim about the agent auto-introducing itself (replace with the real behavior once first-run injection is implemented). |
| `docs/INSTALL.md` | Align with the new flow. setup.sh remains the primary entry point; `foci setup` is the wizard it invokes. |
| `shared/defaults/character/*` | No content changes. But these files now serve double duty: they're the source for seeding new agent workspaces AND the baseline for detecting whether an existing install has been customized (migration check). |

### Files to Leave Alone

| File | Why |
|------|-----|
| `workspace/bootstrap.go` | Already handles missing files gracefully. No changes needed. |

## Decisions (Resolved)

1. **Config file location.** `/home/foci/config/foci.toml` — the foci user's home, where setup.sh creates the config directory. The wizard receives `--config-dir` from setup.sh. No ambiguity.

2. **Workspace location.** `$FOCI_HOME/$AGENT_ID` — the existing convention. Not configurable in the wizard; advanced users edit the config after.

3. **Secrets file permissions.** setup.sh handles this. The wizard writes secrets.toml; setup.sh Step 4b hardens it to `root:foci-secrets` mode `0660`. No `skip_security_checks` in generated config.

4. **Multiple agents.** One agent per wizard run. Add more by editing config.

5. **Model default.** `claude-sonnet-4-6` (stable alias, no date suffix). Not asked during setup.

6. **First-run detection.** `first_run_completed` flag in state.db — a proper persistent boolean. NOT content parsing of character files (placeholder comments are unreliable and a user could legitimately write them).

7. **User ID capture.** Auto-detected by temporarily starting the bot and waiting for the first message. Eliminates the @userinfobot step.

8. **BotFather `/setcommands`.** Register programmatically via `setMyCommands` API on startup. Out of scope for this plan but worth doing.

## Open Questions

1. **`foci setup` invocation context.** The wizard needs to run as the foci user (to write to its home dir) but also needs the `foci-secrets` group (to write secrets.toml before setup.sh hardens it). The proposed `sudo -u foci -g foci-secrets foci setup ...` should work, but needs testing. Alternative: wizard writes secrets to a temp file, setup.sh moves it into place with correct ownership.

2. **Auto-capture timeout.** If the user doesn't message the bot within the timeout (e.g. 2 minutes), what happens? Suggestion: fall back to manual entry ("Enter your Telegram user ID:") with a link to @userinfobot.

3. **Character file import: nested directories.** If the import source has subdirectories (e.g. `character/memory/`), should we recurse? Suggestion: only import `.md` files from the top level of the specified directory. Memory files are session-specific and shouldn't be imported.

4. **First-run completion heuristic.** What triggers `first_run_completed = true`? Options:
   - (a) Agent has modified SOUL.md and USER.md (check mtime)
   - (b) After N turns in the first session (e.g. 10)
   - (c) Agent explicitly calls a "complete onboarding" action
   - (d) After first `/reset`
   - Recommendation: (a) — file modification is the most meaningful signal that onboarding actually happened. Fall back to (b) as a safety net so the prompt doesn't persist forever.

5. **Non-interactive setup.sh.** The current setup.sh supports env vars for non-interactive installs (CI, automation). `foci setup` should also support flags (`--bot-token`, `--user-id`, `--agent-id`, `--auth-method`) for the same use case. setup.sh passes env vars through as flags.

## Implementation Order

1. **`config/config.go` — `GenerateDefault()`** — config generation function, testable in isolation
2. **`config/config_test.go`** — test the generation
3. **`prompts/first-run.md`** — the onboarding prompt text
4. **`cmd/foci/setup.go`** — the interactive wizard (bot token prompt, user ID auto-capture, auth flow, agent ID, character file import, calls GenerateDefault, writes files)
5. **`cmd/foci/setup_test.go`** — test wizard components
6. **`setup.sh`** — rewrite Step 4 to invoke `foci setup`, remove Step 5 (character files), fix bugs from setup-review.md
7. **`main.go`** — first-run detection + prompt injection (state flag check, migration logic, message injection)
8. **`docs/GETTING-STARTED.md`** — rewrite for actual flow
9. **`docs/INSTALL.md`** — align with new flow

## Scope Boundaries

**In scope:**
- `foci setup` subcommand (interactive wizard)
- Config + secrets generation via Go (replacing bash heredocs)
- User ID auto-capture via temporary bot polling
- Character file seeding from `shared/defaults/character/`
- Character file selective import from existing directory
- setup.sh integration (invoke wizard, fix bugs)
- First-run detection via state flag + onboarding prompt injection
- Migration check for existing installs
- Docs updates

**Out of scope (do separately):**
- Multi-agent setup wizard
- Voice/WebSocket setup
- Bitwarden integration setup
- Programmatic bot command registration via `setMyCommands`
- Dynamic model discovery via `/v1/models` API
