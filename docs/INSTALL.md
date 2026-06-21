# Installation Guide

Step-by-step setup for foci on a Linux server (Debian/Ubuntu). Takes about 10 minutes.

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

- **Go 1.24+** — downloaded automatically by `setup.sh` if not available (requires `curl` or `wget`)
- **git** — for cloning the repo
- **gcc / build-essential** — C compiler (needed for SQLite CGO)
- **make** — build tool
- **tmux** — terminal multiplexing (optional but recommended)
- **jq** — JSON processing (optional but recommended)
- **sqlite3** — database CLI (optional, for debugging)

On Debian/Ubuntu:

```bash
sudo apt install git build-essential make curl tmux jq sqlite3
```

Or run the prerequisites script which handles all distros:

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
./setup.sh
```

The wizard prompts for:
- **LLM provider** — Anthropic, Google Gemini, OpenAI, OpenRouter, or Custom endpoint
- **API key** — for the chosen provider
- **Bot token** — paste the token from @BotFather
- **User ID** — auto-detected by messaging your bot, or entered manually
- **Agent ID** — a short name for your agent (default: `main`)
- **Character files** — use default templates or import from an existing directory

Setup creates:
- System user `foci` with home at `/home/foci`
- Binaries at `/usr/local/bin/` (`foci-gw`, `foci`, `foci-call`)
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
./setup.sh
```

Available env vars:
| Variable | Required | Description |
|----------|----------|-------------|
| `FOCI_TELEGRAM_TOKEN` | Yes | Telegram bot token |
| `FOCI_TELEGRAM_USER` | Yes | Your Telegram user ID |
| `FOCI_PROVIDER` | No | LLM provider: `anthropic`, `gemini`, `openai`, `openrouter` (default: `anthropic`) |
| `FOCI_API_KEY` | No | API key for the chosen provider |
| `FOCI_AGENT_ID` | No | Agent identifier (default: `main`) |
| `FOCI_CHAR_MODE` | No | Character mode: `defaults`, `openclaw`, `import`, `blank` (default: `defaults`) |
| `FOCI_CHAR_IMPORT_DIR` | If import | Directory to import character `.md` files from |
| `FOCI_MEMORY_IMPORT_DIR` | No | Directory to import memory `.md` files from |

### Dry run

Preview what setup would do without making changes:

```bash
./setup.sh --dry-run
```

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
./setup.sh
```

On update, setup generates a changelog (`WELCOME.md`) that the agent summarises and sends to you via Telegram.

### Config compatibility pre-check

`update.sh` validates every foci service's config with the freshly-built binary
*before* installing it or restarting anything. Each service's config (the
`-config` path from its `ExecStart`) is checked via:

```bash
foci-gw -check-config -config /path/to/foci.toml
```

This exits `0` if the config loads cleanly and `1` on a parse/validate error or
any unknown/deprecated key (e.g. a renamed setting — strict policy, since the
old value would be silently dropped at startup). If any service's config fails,
`update.sh` aborts with the running daemon untouched, so a config incompatibility
can no longer brick the service mid-upgrade. You can run the same check by hand
before upgrading.

## Directory Layout

After setup, the foci user's home looks like:

```
/home/foci/
  config/
    foci.toml              ← main config
    secrets.toml           ← API keys, bot tokens (restricted permissions)
  data/
    sessions/              ← session JSONL files
    conversation.db        ← Telegram message log
    state.db               ← persistent state (SQLite)
    memory.db              ← memory FTS index
    todo.db                ← todo store
    reminders.db           ← reminder store
  logs/
    foci.log               ← event log
    api.jsonl              ← API call log
  <agent-id>/              ← agent workspace (one per agent)
    character/             ← identity files (SOUL.md, CRAFT.md, etc.)
    memory/                ← daily memory files
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
Older `setup.sh` versions emitted a `foci setup` call in the generated root-install script, but the wizard command is `foci first-run`. The bad call aborted the install before the systemd service was created. Pull the latest repo and re-run `./setup.sh --install`.

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
make              # build all 3 binaries (foci-gw, foci, foci-call)
make build        # gateway only
make cli          # CLI only
make test         # run all tests
make vet          # go vet
make lint         # vet + errcheck (production only) + gocyclo/gocognit (>75/100 threshold)
make check        # test + lint
make clean        # remove built binaries
```

### Static analysis tools

`make lint` requires these tools (install once):

```bash
go install github.com/kisielk/errcheck@latest
go install github.com/fzipp/gocyclo/cmd/gocyclo@latest
go install github.com/uudashr/gocognit/cmd/gocognit@latest
```

### Further reading

- [AUTH.md](AUTH.md) — detailed authentication setup
- [CONFIG.md](CONFIG.md) — full configuration reference
- [WIRING.md](WIRING.md) — how components connect
