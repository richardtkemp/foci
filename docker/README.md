# Foci — Docker Deployment

Run foci with a single command using Docker Compose.

## Prerequisites

- [Docker](https://docs.docker.com/get-docker/) with Compose v2+
- A Telegram bot token (from [@BotFather](https://t.me/BotFather))
- Your Telegram user ID
- An API key or setup token for LLM access

## Quick Start

```bash
cd docker

# 1. Create your .env file
cp .env.example .env

# 2. Edit .env — fill in your Telegram token, user ID, and API key
nano .env

# 3. Launch
docker compose up -d
```

That's it. On first run, foci runs its setup wizard automatically using your `.env` values, then starts the gateway. Message your bot on Telegram — it will introduce itself.

## What Happens

**First run:** The entrypoint detects no config exists, runs `foci setup` with your env vars to generate `foci.toml` and `secrets.toml`, then starts `foci-gw`.

**Subsequent runs:** Config already exists in the persistent volume, so foci starts immediately.

## Managing

```bash
# View logs
docker compose logs -f

# Restart
docker compose restart

# Stop
docker compose down

# Stop and remove all data (config, sessions, memory)
docker compose down -v
```

## Persistent Data

All state is stored in the `foci-home` Docker volume, which maps to `/home/foci` inside the container. This includes:

- `config/` — `foci.toml`, `secrets.toml`
- `data/` — sessions, databases, state
- `<agent-id>/` — character files, memory
- `shared/` — docs, skills, defaults

The volume survives `docker compose down`. Only `docker compose down -v` removes it.

## Configuration

After first run, you can edit the config directly:

```bash
# Find the volume mount path
docker volume inspect docker_foci-home --format '{{ .Mountpoint }}'

# Edit config (as root, since Docker volumes are root-owned on the host)
sudo nano "$(docker volume inspect docker_foci-home --format '{{ .Mountpoint }}')/config/foci.toml"

# Restart to apply
docker compose restart
```

Or exec into the container:

```bash
docker compose exec foci bash
vi ~/config/foci.toml
```

Then restart to apply changes.

## Rebuilding

To pick up code changes (e.g. after `git pull`):

```bash
docker compose build
docker compose up -d
```

Your config and data persist across rebuilds.

## Environment Variables

Required on first startup only, to seed the config file.

| Variable | Required | Description |
|----------|----------|-------------|
| `FOCI_TELEGRAM_TOKEN` | Yes | Telegram bot token from @BotFather |
| `FOCI_TELEGRAM_USER` | Yes | Your Telegram user ID |
| `FOCI_AUTH_METHOD` | No | `apikey`, `setup-token`, or `skip` (default: `skip`) |
| `FOCI_AUTH_TOKEN` | Conditional | API key or setup token (required if auth method is not `skip`) |
| `FOCI_AGENT_ID` | No | Agent identifier (default: `main`) |
| `FOCI_CHAR_MODE` | No | Character mode: `defaults`, `openclaw`, `import`, `blank` |

## Importing Character and Memory Files

To seed your agent with character or memory files on first run, place `.md` files in:

- `docker/character/` — character definition files
- `docker/memory/` — memory files

These are baked into the image at build time and imported during setup. Only used on first run (when no config exists yet).

## Security

The Docker deployment replicates the same OS-level secrets protection used by the systemd setup. The entrypoint starts as root, hardens `secrets.toml` (owned by `root:foci-secrets`, mode `0660`), then drops to the `foci` user via `setpriv` with:

- `--init-groups` — gives foci-gw the `foci-secrets` supplementary group (can read secrets)
- `--ambient-caps=+setgid` — allows foci-gw to drop `foci-secrets` from child processes

This means agent-spawned subprocesses (shell commands, tmux, scripts) cannot read `secrets.toml` — the kernel denies access. See `docs/SECRETS.md` for details.

The `cap_add: [SETGID]` in `compose.yml` is required to make this work.

## Troubleshooting

**Bot doesn't respond:**
1. Check logs: `docker compose logs -f`
2. Verify your Telegram user ID and bot token in `.env`
3. Ensure the container is running: `docker compose ps`

**Want to re-run setup from scratch:**
```bash
docker compose down -v   # removes the volume
docker compose up -d     # fresh start
```
