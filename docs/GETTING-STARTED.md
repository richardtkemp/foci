# Getting Started with Foci

This guide walks you through setting up your first foci agent — from install to first conversation.

## 1. Install

Follow [INSTALL.md](INSTALL.md) to build and install foci. You'll need:
- A Linux server (Ubuntu/Debian recommended)
- Go 1.22+
- A Telegram bot token (from [@BotFather](https://t.me/BotFather))

The install script (`setup.sh`) creates a system user, builds the binaries, and sets up the service.

```bash
sudo ./setup.sh -u myagent
```

## 2. Configure

The install creates a config file at `~myagent/config/foci.toml`. Edit it:

```bash
sudo -u myagent nano ~myagent/config/foci.toml
```

At minimum, you need:
- A Telegram bot token in `secrets.toml`
- Your Telegram user ID in the agent config
- An Anthropic API key or OAuth authentication

See [foci.toml.example](../foci.toml.example) for all available options with comments.

## 3. Authenticate

Foci needs access to the Anthropic API. You have two options:

**Option A: OAuth (recommended for Max/Pro subscribers)**
```bash
sudo -u myagent foci auth --config ~myagent/config/foci.toml
```
Follow the browser prompts. Tokens are saved to `secrets.toml` and refresh automatically.

**Option B: API key**
Add your key to `secrets.toml`:
```toml
[anthropic]
setup_token = "sk-ant-..."
```

See [AUTH.md](AUTH.md) for full details.

## 4. Start the Service

```bash
sudo systemctl start foci
sudo systemctl status foci
```

Check the logs:
```bash
journalctl -u foci -f
```

## 5. Send Your First Message

Open Telegram and message your bot. On first run, the agent will:

1. Introduce itself and explain it's brand new
2. Ask your name and communication preferences
3. Ask what you'd like to call it
4. Learn about you — interests, work, style
5. Update its character files based on what you share

This is the start of your relationship with your agent. Take a few minutes to help it understand who you are.

## 6. What's Next

### Character files
Your agent's identity lives in `~myagent/character/`. The onboarding conversation fills these in, but you can edit them directly anytime:
- **IDENTITY.md** — who the agent is
- **SOUL.md** — inner life, values, what it notices
- **USER.md** — about you
- **MEMORY.md** — learned knowledge that persists across sessions

### Skills
Skills are capability bundles — a `SKILL.md` file that teaches the agent how to use specific tools or workflows. Place them in `~myagent/shared/skills/skillname/SKILL.md`.

### Memory
The agent writes daily notes to `~myagent/character/memory/YYYY-MM-DD.md`. These are searchable and form its long-term memory. MEMORY.md holds curated lessons that load every session.

### Keepalive
When idle, the agent receives periodic `[KEEPALIVE]` messages — a chance to reflect, check on things, or just note it's still running. Configure the interval in `foci.toml`.

### Commands
In Telegram, type `/help` to see available slash commands: `/status`, `/model`, `/thinking`, `/effort`, and more.

## Troubleshooting

**Bot doesn't respond:** Check `journalctl -u foci -f` for errors. Common causes: wrong user ID, missing API key, Telegram token issues.

**Authentication errors:** Run `foci auth` again, or check that `secrets.toml` has valid credentials.

**High latency:** First message after a restart is slower (cache cold). Subsequent messages should be faster.

See the full [documentation index](../README.md) for more guides.
