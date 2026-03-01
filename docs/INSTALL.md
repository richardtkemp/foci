# Installation Guide

Step-by-step setup for foci on a Linux server (Debian/Ubuntu). Takes about 10 minutes.

## Prerequisites

Install these before running setup:

```bash
# Go 1.22+ (for building from source)
sudo apt install golang-go    # or https://go.dev/dl/

# Required tools
sudo apt install tmux jq git

# Optional but recommended
sudo apt install ack           # file search (used by grep skill)
pip install yq                 # TOML/YAML/XML querying (used by query skill)
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

The setup script creates a system user, builds binaries, sets up systemd, and launches the `foci setup` wizard for interactive configuration:

```bash
sudo ./setup.sh -u foci
```

The wizard prompts for:
- **Bot token** — paste the token from @BotFather
- **Authentication** — OAuth (recommended for subscribers), API key, or skip
- **User ID** — auto-detected by messaging your bot, or entered manually
- **Agent ID** — a short name for your agent (default: `main`)
- **Character files** — use default templates or import from an existing directory

Setup creates:
- System user `foci` with home at `/home/foci`
- Binaries at `/usr/local/bin/` (`focigw`, `foci`, `foci-call`)
- Systemd service `foci`
- Config at `/home/foci/config/foci.toml`
- Secrets at `/home/foci/config/secrets.toml` (restricted permissions: `root:foci-secrets`, mode `0660`)
- Character files at `/home/foci/<agent-id>/character/`

### Non-interactive setup

Pass configuration via environment variables for automated or CI installs:

```bash
FOCI_TELEGRAM_TOKEN="123456789:AAF-..." \
FOCI_TELEGRAM_USER="5970082313" \
FOCI_AUTH_METHOD="apikey" \
FOCI_AUTH_TOKEN="sk-ant-..." \
FOCI_AGENT_ID="myagent" \
sudo ./setup.sh -u foci
```

Available env vars:
| Variable | Required | Description |
|----------|----------|-------------|
| `FOCI_TELEGRAM_TOKEN` | Yes | Telegram bot token |
| `FOCI_TELEGRAM_USER` | Yes | Your Telegram user ID |
| `FOCI_AUTH_METHOD` | No | `oauth`, `apikey`, or `skip` (default: `skip`) |
| `FOCI_AUTH_TOKEN` | If apikey | Anthropic API key |
| `FOCI_AGENT_ID` | No | Agent identifier (default: `main`) |

### Dry run

Preview what setup would do without making changes:

```bash
sudo ./setup.sh -u foci --dry-run
```

### Re-running the wizard

To re-run the setup wizard after initial install (e.g. to reconfigure):

```bash
sudo -u foci -g foci-secrets foci setup \
    --config-dir /home/foci/config \
    --home /home/foci \
    --defaults-dir /path/to/foci/shared/defaults/character
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
sudo ./setup.sh -u foci
```

On update, setup generates a changelog (`WELCOME.md`) that the agent summarises and sends to you via Telegram.

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
    state.json             ← persistent state
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
Ensure Go 1.22+: `go version`. Foci uses go module caching at `/var/cache/go` and `/var/cache/go-build`.

### "unknown command: setup"
Make sure you're running the updated `foci` binary from `/usr/local/bin/foci`. Re-run `setup.sh` to rebuild.

## Next Steps

- [AUTH.md](AUTH.md) — detailed authentication setup
- [CONFIG.md](CONFIG.md) — full configuration reference
- [WIRING.md](WIRING.md) — how components connect
