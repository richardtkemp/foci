# Getting Started with Foci

This guide walks you through setting up your first foci agent — from install to first conversation.

## 1. Install

Follow [INSTALL.md](INSTALL.md) for prerequisites. You'll need:
- A Linux server (Ubuntu/Debian recommended)
- Go 1.22+
- A Telegram bot token (from [@BotFather](https://t.me/BotFather))

## 2. Run Setup

Clone the repo and run setup. The script creates a system user, builds binaries, sets up systemd, and launches an interactive wizard to configure your agent:

```bash
git clone https://github.com/richardtkemp/foci.git
cd foci
sudo ./setup.sh -u foci
```

The wizard walks you through:
1. **Bot token** — paste the token from @BotFather
2. **Authentication** — OAuth (recommended for subscribers), API key, or skip
3. **User ID** — auto-detected by messaging your bot, or entered manually
4. **Agent ID** — a short name for your agent (default: `main`)
5. **Character files** — use defaults or import existing files

Setup writes `foci.toml` and `secrets.toml`, seeds character file templates, installs the systemd service, and starts the bot.

### Non-interactive setup

Pass configuration via environment variables for automated installs:

```bash
FOCI_TELEGRAM_TOKEN="123456789:AAF-..." \
FOCI_TELEGRAM_USER="5970082313" \
FOCI_AUTH_METHOD="apikey" \
FOCI_AUTH_TOKEN="sk-ant-..." \
sudo ./setup.sh -u foci
```

## 3. Send Your First Message

Open Telegram and message your bot. On first run, the agent will:

1. Introduce itself and explain it's brand new
2. Ask your name and communication preferences
3. Ask what you'd like to call it
4. Learn about you — interests, work, style
5. Update its character files based on what you share

This is the start of your relationship with your agent. Take a few minutes to help it understand who you are.

## 4. What's Next

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
When idle, the agent receives periodic `[KEEPALIVE]` messages — a chance to reflect, check on things, or just note it's still running. Configure the interval in `foci.toml`.

### Commands
In Telegram, type `/help` to see available slash commands: `/status`, `/model`, `/thinking`, `/effort`, and more.

## Troubleshooting

**Bot doesn't respond:** Check `journalctl -u foci -f` for errors. Common causes: wrong user ID, missing API key, Telegram token issues.

**Authentication errors:** Run `foci auth` again, or check that `secrets.toml` has valid credentials.

**High latency:** First message after a restart is slower (cache cold). Subsequent messages should be faster.

See the full [documentation index](../README.md) for more guides.
