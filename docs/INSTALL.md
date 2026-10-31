# Installation Guide

Step-by-step setup for foci on a Linux server (Debian/Ubuntu). Takes about 10 minutes. For a Mac, see [macOS](#macos).

## Docker (Quickest)

If you have Docker with Compose v2+, see [`docker/README.md`](../docker/README.md) for a one-command deployment:

```bash
cd docker
cp .env.example .env
# Edit .env with your Telegram token, user ID, auth method, and API key
docker compose up -d
```

No Go toolchain, no system user creation, no systemd — just fill in env vars and run.

---

## Bare Metal

The rest of this guide covers the traditional bare-metal install with systemd.

## Prerequisites

Install these before running setup:

- **Go 1.24+** — downloaded automatically by `make setup` if not available (requires `curl` or `wget`)
- **git** — for cloning the repo
- **gcc / build-essential** — C compiler for the `nosgid.so` LD_PRELOAD shim only; the Go binaries build with `CGO_ENABLED=0` (SQLite is pure Go, and the ambient-capability clear in `procx` requires a cgo-free binary — see SECRETS.md)
- **make** — build tool
- **tmux** — terminal multiplexing (optional but recommended)
- **jq** — JSON processing (optional but recommended)
- **sqlite3** — database CLI (optional, for debugging)
- **mermaid-cli (`mmdc`)** — optional: renders mermaid diagrams, both in the app (```` ```mermaid ```` blocks) and as an image `send_to_chat` sends alongside a `.mmd`/`.mermaid` file. Needs node/npm and a system chromium; without it the app shows the diagram source and `send_to_chat` sends the source alone. Install with `PUPPETEER_SKIP_DOWNLOAD=1 npm i -g @mermaid-js/mermaid-cli` (configure with `[platforms.app] mermaid_cmd` / `mermaid_browser`)

On Debian/Ubuntu:

```bash
sudo apt install git build-essential make curl tmux jq sqlite3
```

Or run the prerequisites script which handles all distros (it reports, but does not install, the optional mermaid-cli):

```bash
sudo ./prerequisites.sh --install
```

## 1. Clone the Repository

```bash
cd ~/src  # or wherever you keep repos
git clone https://github.com/richardtkemp/foci.git
cd foci
```

## 2. Create a Telegram Bot

1. Message [@BotFather](https://t.me/BotFather) on Telegram
2. Send `/newbot`, follow the prompts
3. Save the bot token (format: `123456789:AAF-...`)
4. Optionally send `/setcommands` to BotFather, select your bot, then paste:
   ```
   status - Dashboard overview
   reset - Clear session history
   model - Show or switch model
   ```

## 3. Run Setup

The setup script creates a system user, builds binaries, sets up systemd, and launches the `foci first-run` wizard for interactive configuration:

```bash
sudo make install
```

`make install` is the one entry point on both Linux and macOS. On a Linux host
with no foci unit yet it runs `make setup`; once the unit exists it runs
`make update`. Both targets remain available directly.

The wizard prompts for:
- **LLM provider** — Anthropic, Google Gemini, OpenAI, OpenRouter, or Custom endpoint
- **API key** — for the chosen provider
- **Bot token** — paste the token from @BotFather
- **User ID** — auto-detected by messaging your bot, or entered manually
- **Agent ID** — a short name for your agent (default: `main`)
- **Character files** — use default templates or import from an existing directory

Setup creates:
- System user `foci` with home at `/home/foci`
- Binaries at `/usr/local/bin/` (`foci-gw`, `foci`, `foci-call`, `foci-cc-hook`)
- Systemd service `foci`
- Config at `/home/foci/config/foci.toml`
- Secrets at `/home/foci/config/secrets.toml` (restricted permissions: `root:foci-secrets`, mode `0660`)
- Character files at `/home/foci/<agent-id>/character/`

### Non-interactive setup

Pass configuration via environment variables for automated or CI installs:

```bash
FOCI_TELEGRAM_TOKEN="123456789:AAF-..." \
FOCI_TELEGRAM_USER="5970082313" \
FOCI_PROVIDER="anthropic" \
FOCI_API_KEY="sk-ant-..." \
FOCI_AGENT_ID="myagent" \
sudo make setup
```

Available env vars:
| Variable | Required | Description |
|----------|----------|-------------|
| `FOCI_TELEGRAM_TOKEN` | Yes | Telegram bot token |
| `FOCI_TELEGRAM_USER` | Yes | Your Telegram user ID |
| `FOCI_PROVIDER` | No | LLM provider: `anthropic`, `gemini`, `openai`, `openrouter`, `claude-code`, `custom` (default: `anthropic`) |
| `FOCI_API_KEY` | No | API key for the chosen provider |
| `FOCI_AGENT_ID` | No | Agent identifier (default: `main`) |
| `FOCI_CHAR_MODE` | No | Character mode: `defaults`, `openclaw`, `import`, `blank` (default: `defaults`) |
| `FOCI_CHAR_IMPORT_DIR` | If import | Directory to import character `.md` files from |
| `FOCI_MEMORY_IMPORT_DIR` | No | Directory to import memory `.md` files from |

### Re-running the wizard

To re-run the setup wizard after initial install (e.g. to reconfigure):

```bash
sudo -u foci -g foci-secrets foci first-run \
    --config-dir /home/foci/config
```

## 4. Verify

```bash
# Check the service
sudo systemctl status foci

# Check logs
sudo journalctl -u foci -f
```

Now message your bot on Telegram — it will introduce itself and guide you through setting up its identity.

## Updating

Pull and re-run setup. It's idempotent — safe to run repeatedly:

```bash
cd /path/to/foci
git pull
sudo make install    # = sudo make update on an existing install
```

On update, setup generates a changelog (`WELCOME.md`) that the agent summarises and sends to you via Telegram.

### Config compatibility pre-check

`make update` validates every foci service's config with the freshly-built binary
*before* installing it or restarting anything. Each service's config (the
`-config` path from its `ExecStart`) is checked via:

```bash
foci-gw -check-config -config /path/to/foci.toml
```

This exits `0` if the config loads cleanly and `1` on a parse/validate error or
any unknown/deprecated key (e.g. a renamed setting — strict policy, since the
old value would be silently dropped at startup). Without `-config` it checks `$FOCI_CONFIG`, else
`~/config/foci.toml`; a missing file fails with a pointer to `-config PATH`. If any service's config fails,
`make update` aborts with the running daemon untouched, so a config incompatibility
can no longer brick the service mid-upgrade. You can run the same check by hand
before upgrading.

## macOS

foci runs on macOS (Apple Silicon and Intel) as **your own user**, under a
per-user LaunchAgent. There is no root step, no `foci` system user and no
systemd.

### Install

```bash
xcode-select --install          # git and make, if not already present
git clone https://github.com/richardtkemp/foci.git && cd foci
make install                    # as you, no sudo
```

`make install` installs any missing Homebrew dependencies (`make macos-deps`:
go, GNU coreutils/sed/grep/findutils/tar/awk, flock, sqlite, tmux, jq, mdq,
bash), then runs `make macos-install`: build, install into `~/foci`, (re)start
the LaunchAgent. Homebrew itself must already be installed.

On the first install there is no config yet. At a terminal, the `foci first-run`
wizard runs and the install continues. Without a terminal it stops after
creating the directories; write `~/foci/config/foci.toml` (or run
`FOCI_HOME=~/foci ~/foci/bin/foci first-run --config-dir ~/foci/config`), then run
`make install` again. Re-running it is also how you update: it rebuilds,
checks the config against the new binary before touching the running daemon,
reinstalls and restarts.

An agent running inside foci can run `make install` itself. When the
LaunchAgent plist is unchanged, the restart is `launchctl kickstart -k`:
launchd stops and restarts foci, so the restart completes even though it
kills the agent that started it (the macOS counterpart of Linux
`make update`). When the plist changed, the restart needs `launchctl bootout`
+ `bootstrap`, which would kill the agent before foci comes back. The script
then installs the binaries, leaves foci running on the old ones and asks you
to run `make install` from a terminal.

Claude Code must be installed (`~/.local/bin/claude`). The Claude Code foci
runs has its own login, separate from yours (see below): on its first start foci
finds it logged out and sends a sign-in link to the master agent's chat. To log
it in yourself instead, run this in a Terminal on the Mac (not over ssh, which
cannot reach the login keychain):

```bash
CLAUDE_CONFIG_DIR=~/foci/.claude claude auth login
```

### How it differs from Linux

| | Linux | macOS |
|---|---|---|
| Runs as | dedicated `foci` user | you |
| Service | systemd unit `foci` | LaunchAgent `com.github.richardtkemp.foci` (`~/Library/LaunchAgents/`), in your GUI session |
| Install root | `/home/foci` (foci's `HOME`) | `~/foci` (`FOCI_HOME`; `HOME` stays yours) |
| Memory guard | kills the largest process of the `foci` user when PSI shows memory stalls | kills only foci-gw's own descendants (never your other apps), and only at macOS's "critical" memory-pressure level |
| Binaries | `/usr/local/bin` (root-owned) | `~/foci/bin` |
| Logs | journal + `~/logs/foci.log` | `~/foci/logs/foci.log`, launchd stdout/stderr in `~/foci/logs/launchd.*.log` |
| Restart | `sudo systemctl restart foci` | `launchctl kickstart -k gui/$(id -u)/com.github.richardtkemp.foci` (or `/restart`) |
| Stop | `sudo systemctl stop foci` | `launchctl bootout gui/$(id -u)/com.github.richardtkemp.foci` |

**The install root is `FOCI_HOME`, and `HOME` stays your home.** foci takes
its install root from `$FOCI_HOME` (falling back to `$HOME`, which is how Linux
gets `/home/foci`). Everything foci and its agents start sees your real `HOME`,
so codex, opencode, `gh`, `git`, `ssh` and Homebrew tools use your own logins
and config, and an agent's `~` is your home. Agent workspaces are still
`~/foci/<agent>`.

**Claude Code is the exception.** foci-gw starts it with
`CLAUDE_CONFIG_DIR=~/foci/.claude`, so the Claude Code foci runs keeps its own
settings, `CLAUDE.md`, MCP servers, plugins and transcripts, separate from your
interactive `~/.claude`. Claude Code keys its keychain login by that folder,
so it also has its own login (see Install above).

The `foci` CLI finds the running gateway from your own shell with no setup:
when `$FOCI_HOME`/`$HOME` has no `data/foci-gw.sock`, it uses the one beside
its own binary (`~/foci/bin/foci` → `~/foci/data/foci-gw.sock`). Put
`~/foci/bin` on your `PATH`, or run `~/foci/bin/foci <command>`.

The daemon `PATH` is `~/foci/bin`, `~/.local/bin` (Claude Code), the system
directories, `~/.opencode/bin` (opencode's own installer) and Homebrew.

Installs from before this layout (foci-gw ran with `HOME=~/foci`) are carried
over by `make install`: it copies `~/foci/.claude.json` to
`~/foci/.claude/.claude.json`, removes the old `~/foci/Library/Keychains`
link, and copies codex's saved conversations from `~/foci/.codex/sessions` to
`~/.codex/sessions` (or `$CODEX_HOME/sessions`), once and without overwriting,
so codex agents resume them instead of starting over. The rest of what foci's
agents had under `~/foci` as their home (`~/foci/.codex`, `~/foci/.config`,
`~/foci/.local/share/opencode`, `~/foci/.ssh`) is no longer read; the agents
use yours.

### What is not available

- **Secrets isolation.** On Linux, `secrets.toml` is `root:foci-secrets 0660`,
  foci-gw alone holds the group, and it strips the group from every child, so
  agents cannot read the file. macOS has no per-process supplementary group for
  a LaunchAgent and lets only root call `setgroups`, so agents run as you and
  can read `~/foci/config/secrets.toml`. The startup check asks only that the
  file is yours and mode `0600`. See [SECRETS.md](SECRETS.md#macos).
- **The nosgid preload shim and Landlock sandboxing** (`llbox`): Linux-only.
- **Auto-approval of commands from user-writable directories.** Before
  auto-approving a command, foci checks that the agent cannot replace the file
  the name resolves to. On a Mac everything outside the SIP-protected system
  directories is yours, including Homebrew and `~/foci/bin`. So the daemon
  `PATH` puts `/usr/bin` and `/bin` before Homebrew (`ls`, `grep`, `jq`,
  `sqlite3`, `diff`, `git` auto-approve as usual), and tools that exist only in
  Homebrew or `~/foci/bin` (`rg`, `go`, `npm`, `mdq`, `mds`, …) ask for
  approval. `foci-gw -check-config` lists them.

### Developing on a Mac

`make test` works after `make macos-deps`. The Makefile puts the Homebrew GNU
tools on `PATH` (the recipes and scripts are written for GNU userland), uses
`/private/tmp/fgw` as the test root (`/tmp` is a symlink on macOS), and puts
Homebrew `sqlite3` ahead of Apple's, which cannot open WAL databases
`-readonly`. Tests that exercise Linux-only mechanisms (procfs, ETXTBSY, the
`foci-secrets` group model) skip on macOS.

### Landing on a Mac

A Mac checkout reaches main only through `make land`, as on Linux; never push to
main directly. `make land` runs `scripts/land.sh`, which needs:

- `flock` on `PATH` for the merge lock. macOS does not ship it; Homebrew's
  `flock` formula provides it (`make macos-deps` installs it, into
  `/opt/homebrew/bin`). Without it, land stops before the gate with
  `LAND_RESULT: FAILED no flock on PATH` and pushes nothing.
- `git`, `make` and `bash`. Apple's `/bin/bash` 3.2 and `/usr/bin/make` 3.81 are
  enough; the script avoids bash-4 syntax and GNU-only flags.
- Everything the gate (`make test`) needs, i.e. `make macos-deps`.
- An `origin` this checkout can push to.

`bash scripts/test-land.sh` checks the script against a throwaway bare origin
(red gate, green gate, missing and failing lock, two concurrent landings) on
either OS, without touching the real origin.

## Directory Layout

After setup, the foci user's home looks like:

```
/home/foci/
  config/
    foci.toml              ← main config
    secrets.toml           ← API keys, bot tokens (restricted permissions)
  data/
    sessions/              ← session JSONL files
    api.db                 ← API call log (SQLite)
    state.db               ← persistent state (SQLite)
    session_index.db       ← session metadata index
    tool_details.db        ← tool detail store
  logs/
    foci.log               ← event log
    api.jsonl              ← API call log
  <agent-id>/              ← agent workspace (one per agent)
    character/             ← identity files (SOUL.md, CRAFT.md, etc.)
    memory/                ← daily memory files
    .data/                 ← per-agent databases
      conversation.db      ← Telegram message log
      reminders.db         ← reminder store
      scratchpad.db        ← scratchpad store
      todo.db              ← todo store
      memory.db            ← memory FTS index
```

## Troubleshooting

### Bot doesn't respond
1. Check the service is running: `systemctl status foci`
2. Check logs for errors: `journalctl -u foci --since '5 min ago'`
3. Verify your Telegram user ID is in `allowed_users`
4. Verify the bot token in `secrets.toml` matches BotFather's token

### Permission errors on secrets.toml
Secrets file should be owned by `root:foci-secrets` with mode `0660`. Setup handles this automatically. To fix manually:

```bash
sudo chown root:foci-secrets /home/foci/config/secrets.toml
sudo chmod 660 /home/foci/config/secrets.toml
```

### Build errors
Ensure Go 1.24+: `go version`. Setup downloads Go automatically if needed. Foci uses go module caching at `/var/cache/go` and `/var/cache/go-build`.

### "unknown command: setup"
Older versions emitted a `foci setup` call in the generated root-install script, but the wizard command is `foci first-run`. The bad call aborted the install before the systemd service was created. Pull the latest repo and re-run `sudo make setup`.

## Next Steps

### First message

Open Telegram and message your bot. On first run, the agent will introduce itself, ask your name and communication preferences, and learn about you — interests, work, style. It updates its character files based on what you share. Take a few minutes to help it understand who you are.

### Character files

Your agent's identity lives in `~foci/<agent-id>/character/`. The onboarding conversation fills these in, but you can edit them directly anytime:

- **SOUL.md** — inner life, values, what it notices
- **CRAFT.md** — how the agent communicates and works
- **COHERENCE.md** — consistency guidelines
- **USER.md** — about you
- **MEMORY.md** — learned knowledge that persists across sessions

### Skills

Skills are capability bundles — a `SKILL.md` file that teaches the agent how to use specific tools or workflows. Place them in `~foci/shared/skills/skillname/SKILL.md`.

### Memory

The agent writes daily notes to `~foci/<agent-id>/memory/YYYY-MM-DD.md`. These are searchable and form its long-term memory. MEMORY.md holds curated lessons that load every session.

### Keepalive

When idle, the agent receives periodic `[KEEPALIVE]` messages — a chance to reflect, check on things, or note it's still running. Configure the interval in `foci.toml`.

### Commands

In Telegram, type `/help` to see available slash commands: `/status`, `/model`, `/thinking`, `/effort`, and more.

## Development

### Build targets

```bash
make              # build all 5 targets (build, cli, foci-call, foci-cc-hook, nosgid)
make build        # gateway only
make cli          # CLI only
make test         # run all tests
make vet          # go vet
make lint         # golangci-lint + deadcode + custom finders
make check        # lint + coverage-check + verify-persistence
make clean        # remove built binaries
```

### Further reading

- [AUTH.md](AUTH.md) — detailed authentication setup
- [CONFIG.md](CONFIG.md) — full configuration reference
- [WIRING.md](WIRING.md) — how components connect
