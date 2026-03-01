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
4. Send `/setcommands` to BotFather, select your bot, then paste:
   ```
   status - Dashboard overview
   reset - Clear session history
   model - Show or switch model
   ```
5. Find your Telegram user ID: message [@userinfobot](https://t.me/userinfobot)

## 3. Run Setup

The setup script creates a system user, builds binaries, sets up systemd, and handles permissions:

```bash
# Interactive (prompts for tokens):
sudo ./setup.sh -u foci

# Or pass config via environment:
FOCI_ANTHROPIC_TOKEN="sk-ant-..." \
FOCI_TELEGRAM_TOKEN="123456789:AAF-..." \
FOCI_TELEGRAM_USER="YOUR_USER_ID" \
sudo ./setup.sh -u foci
```

This creates:
- System user `foci` with home at `/home/foci`
- Binaries at `/usr/local/bin/` (`focigw`, `foci`, `foci-call`)
- Systemd service `foci`
- Config at `/home/foci/config/foci.toml`
- Secrets at `/home/foci/config/secrets.toml` (restricted permissions)

### Dry run

Preview what setup would do without making changes:

```bash
sudo ./setup.sh -u foci --dry-run
```

## 4. Authenticate with Anthropic

Foci uses OAuth to authenticate with your Claude subscription (Max/Pro). Run:

```bash
foci auth --config /home/foci/config/foci.toml
```

This opens a URL — authenticate in your browser, paste the code back. See [AUTH.md](AUTH.md) for details and alternative auth methods.

## 5. Configure Your Agent

Edit `/home/foci/config/foci.toml`. The example config (`foci.toml.example`) has all options documented. Minimum required:

```toml
[[agents]]
id = "myagent"
system_files = ["character/SOUL.md"]

[telegram]
allowed_users = ["YOUR_TELEGRAM_USER_ID"]
```

Bot tokens go in `secrets.toml`:

```toml
[telegram.bots.myagent]
token = "123456789:AAF-..."
```

## 6. Create Character Files

Character files define your agent's identity. Create the workspace and at least one file:

```bash
sudo -u foci mkdir -p /home/foci/myagent/character
sudo -u foci tee /home/foci/myagent/character/SOUL.md << 'EOF'
# Who I Am

I am a helpful AI assistant. I communicate clearly and directly.
EOF
```

See the README for more on character file conventions.

## 7. Start and Verify

```bash
# Start the service
sudo systemctl start foci

# Check it's running
sudo systemctl status foci

# Check logs
sudo journalctl -u foci -f

# Ping from CLI
foci ping
```

Now message your bot on Telegram. It should respond.

## Updating

Pull and re-run setup. It's idempotent — safe to run repeatedly:

```bash
cd /path/to/foci
git pull
sudo ./setup.sh -u foci
```

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
  myagent/                 ← agent workspace (one per agent)
    character/             ← identity files
    memory/                ← daily memory files
```

## Troubleshooting

### "unknown command: auth"
Make sure you're running the updated `foci` binary from `/usr/local/bin/foci`. Re-run `setup.sh` if needed.

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

## Next Steps

- [AUTH.md](AUTH.md) — detailed authentication setup
- [CONFIG.md](CONFIG.md) — full configuration reference
- [WIRING.md](WIRING.md) — how components connect
