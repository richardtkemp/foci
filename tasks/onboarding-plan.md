# First-Run Onboarding — Implementation Plan

## Current State

**What exists:**
- `setup.sh` — handles system-level setup (user, systemd, dirs, config generation). Requires sudo, prompts for tokens, creates config files. Has several bugs (see `docs/setup-review.md`): character files never created on fresh install, wrong secrets key name, no `[defaults]` section.
- `docs/GETTING-STARTED.md` — covers install → first message, but assumes setup.sh has already run. No guidance for the "I just cloned the repo" case.
- `docs/INSTALL.md` — detailed server install guide, assumes Linux, sudo, systemd.
- `foci.toml.example` / `secrets.toml.example` — well-documented example configs.
- `shared/defaults/character/` — 5 template files (SOUL.md, CRAFT.md, COHERENCE.md, USER.md, MEMORY.md) with placeholder comments. These are the intended starting point for new agents.
- Config loading (`config.Load`) — **fatal exits** if config file missing. No first-run detection. No auto-creation.
- Workspace bootstrap — silently skips missing character files (warns only).
- Secrets loading — **fatal exits** if `secrets.toml` missing.

**What's missing:**
- No way to start foci without pre-created config files.
- No in-binary first-run flow — everything depends on setup.sh or manual editing.
- No interactive Telegram-based onboarding after the bot connects.
- Character file defaults exist in `shared/defaults/character/` but nothing copies them to a new agent workspace.

## Design

### Philosophy

The onboarding should follow the SPEC philosophy: **simple over powerful**, **explicit over clever**. The user should be able to go from `git clone` to a working Telegram conversation in under 5 minutes with minimal friction. The only thing they truly need before starting is a Telegram bot token (BotFather) and Anthropic auth (OAuth or API key).

### Two-Phase Onboarding

**Phase 1: Terminal (get the bot online)**
Happens when foci detects no config. Gets the minimum needed to connect to Telegram.

**Phase 2: Telegram (configure the agent)**
Once the bot is connected, the agent itself handles remaining setup conversationally.

## User Flow

### Phase 1: Terminal Setup

```
$ ./foci
No config file found. Starting first-run setup.

Step 1: Telegram Bot
  Create a bot via @BotFather on Telegram (https://t.me/BotFather)
  Send /newbot, follow the prompts, and paste the token here.

Bot token: 123456789:AAF-...
✓ Bot token looks valid.

Step 2: Your Telegram User ID
  Message @userinfobot on Telegram to find your user ID.

User ID: 12345678
✓ Saved.

Step 3: Anthropic Authentication
  Foci needs access to Claude. Choose one:
  [1] OAuth (recommended for Max/Pro subscribers)
  [2] API key
  [3] Skip (use Claude Code credentials if available)

> 1
Opening browser for OAuth...
  (existing `foci auth` flow runs here)
✓ Authenticated.

Step 4: Agent ID
  Pick a short lowercase name for your agent (letters, numbers, hyphens).
  This becomes the agent's workspace directory and session key prefix.

Agent ID: fotini
✓ Agent ID: fotini

Creating config...
  → foci.toml
  → secrets.toml (restricted permissions)
  → fotini/character/ (seeded from defaults)

Starting foci...
✓ Bot is online. Open Telegram and message your bot to continue setup.
```

**What gets created:**

```
$CONFIG_DIR/
  foci.toml          ← minimal working config
  secrets.toml       ← bot token + auth credentials

$HOME/$AGENT_ID/
  character/
    SOUL.md          ← copied from shared/defaults/character/
    CRAFT.md         ← copied from shared/defaults/character/
    COHERENCE.md     ← copied from shared/defaults/character/
    USER.md          ← copied from shared/defaults/character/
    MEMORY.md        ← copied from shared/defaults/character/
  memory/            ← empty dir, ready for daily files
```

**Generated `foci.toml`:**

```toml
[defaults]
model = "claude-sonnet-4-20250514"

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

### Phase 2: Telegram Onboarding

When the agent starts with default (unfilled) character files, it detects the placeholder comments and enters onboarding mode. This is driven by the character files themselves — the `<!-- placeholder -->` comments signal "not yet configured."

**First message from user triggers the agent's onboarding behavior:**

The agent reads its SOUL.md and sees it's full of `<!-- your name -->` placeholders. Its system prompt already tells it to introduce itself and learn about the user (see GETTING-STARTED.md step 5). This behavior is **already designed** — we just need to make sure the character files are properly seeded so the agent has something to work with.

**What the agent handles conversationally:**
1. Introduces itself, explains it's brand new
2. Asks the user's name, preferences, timezone
3. Asks what they'd like to call the agent
4. Fills in SOUL.md and USER.md based on the conversation
5. Explains what character files are and how they work

**No special onboarding code needed for Phase 2** — the existing agent loop + character file templates + the agent's inherent instruction-following handles this naturally. The key is getting the character files seeded with the right templates.

### Character File Import (Requirement 5)

During Phase 1 terminal setup, after agent ID:

```
Step 5: Character Files
  Do you have existing character files to import?
  [1] No — use defaults (recommended for new users)
  [2] Yes — specify a directory to copy from

> 1
✓ Seeded default character files to fotini/character/
```

If they choose import: copy `.md` files from the specified directory into `$AGENT_ID/character/`, warn if any expected files are missing.

## Files to Change

### New Files

| File | Purpose |
|------|---------|
| `cmd/setup.go` | Terminal-based first-run wizard (Phase 1). Detects missing config, runs interactive prompts, generates config + secrets + workspace. |
| `cmd/setup_test.go` | Tests for config generation, token validation, directory creation. |

### Modified Files

| File | Change |
|------|--------|
| `main.go` | Before `config.Load()`, check if config file exists. If not and stdin is a terminal, run the setup wizard (`cmd/setup.go`). If not a terminal, print instructions and exit. |
| `config/config.go` | Add `GenerateDefault(opts SetupOptions) (string, string)` function that returns valid `foci.toml` and `secrets.toml` content from structured input. This replaces the heredoc generation in setup.sh with something testable. |
| `config/config_test.go` | Tests for `GenerateDefault`. |
| `docs/GETTING-STARTED.md` | Rewrite to cover the new flow: clone → `./foci` → setup wizard → Telegram. Remove references to setup.sh as the primary path. Keep setup.sh docs for server/systemd deployments. |
| `docs/INSTALL.md` | Add a "Quick Start" section at the top for the new `./foci` first-run flow. Keep the existing setup.sh server install as "Production Deployment." |

### Files to Leave Alone

| File | Why |
|------|-----|
| `setup.sh` | Still needed for production/systemd deployments. Fix bugs separately (tracked in `docs/setup-review.md`). |
| `shared/defaults/character/*` | Templates are good as-is. No changes needed. |
| `workspace/bootstrap.go` | Already handles missing files gracefully. No changes. |

## Open Questions

1. **Config file location for first-run.** When the user runs `./foci` with no `-config` flag, where should the generated config go? Options:
   - (a) `./foci.toml` in CWD (simple, matches current default)
   - (b) `~/.config/foci/foci.toml` (XDG-style)
   - (c) Ask the user during setup
   - **Recommendation:** (a) — CWD. Matches the existing `config.ParseFlags()` default of `"foci.toml"`. Simple, explicit, discoverable.

2. **Workspace location.** Related to above. If config is in CWD, workspace defaults to `$HOME/$AGENT_ID`. Should the wizard let the user override this?
   - **Recommendation:** Use the default. Advanced users can edit `foci.toml` after. Keep the wizard minimal.

3. **Secrets file permissions.** The setup wizard runs as the current user (not root), so it can't set `root:foci-secrets` ownership. Options:
   - (a) Create secrets.toml with `0600` (user-only), print a note about running `setup.sh` for production hardening
   - (b) Skip permission hardening entirely for dev/personal setups
   - **Recommendation:** (a). Set `0600`, add `skip_security_checks = true` to generated config, print a note. Production users use setup.sh.

4. **Multiple agents.** The wizard creates one agent. Should it offer to create more?
   - **Recommendation:** No. One agent. Add more by editing config. Keep it simple.

5. **Model selection.** Should the wizard ask which model to default to?
   - **Recommendation:** No. Default to `claude-sonnet-4-20250514`. Users can change it via `/model` in Telegram or by editing config.

6. **Should the agent know it's in onboarding?** Should we inject a special system prompt or flag for the first session, or just let the character file placeholders drive the behavior?
   - **Recommendation:** No special flag. The unfilled character templates are sufficient signal. The agent sees `<!-- your name -->` and knows it needs to introduce itself and learn. This is simpler and more robust than tracking onboarding state.

7. **BotFather `/setcommands`.** Should the wizard remind the user to set bot commands via BotFather, or should foci register them programmatically via the Telegram API?
   - **Recommendation:** Register programmatically via `setMyCommands` API on startup. This is a one-liner and removes a manual step. If this is out of scope, at minimum print the command list for the user to paste.

## Implementation Order

1. **`config/config.go` — `GenerateDefault()`** — config generation logic, testable in isolation
2. **`config/config_test.go`** — test the generation
3. **`cmd/setup.go`** — the interactive wizard, calls `GenerateDefault()`, copies character files
4. **`cmd/setup_test.go`** — test wizard components (token validation, directory creation)
5. **`main.go`** — hook: if no config file + is terminal → run wizard, then continue normal startup
6. **`docs/GETTING-STARTED.md`** — rewrite for new flow
7. **`docs/INSTALL.md`** — add quick start section

## Scope Boundaries

**In scope:**
- Terminal wizard for first-run config generation
- Character file seeding from `shared/defaults/character/`
- Character file import from existing directory
- Config + secrets file generation
- Docs updates

**Out of scope (do separately):**
- Fixing setup.sh bugs (tracked in `docs/setup-review.md`)
- Telegram-side onboarding prompts (the agent handles this naturally via character templates)
- Multi-agent setup
- Voice/WebSocket setup
- Bitwarden integration setup
- Programmatic bot command registration (nice-to-have, not blocking)
