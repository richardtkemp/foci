# Suggested System Tools

Tools that clod agents can use via the `exec` tool or that clod integrates with directly. None are strictly required — install what your agents need.

## Core

| Tool | Purpose | Install |
|------|---------|---------|
| **tmux** | Session management for coding agents | `apt install tmux` |
| **jq** | JSON processing — essential for JSONL logs and API responses | `apt install jq` |
| **curl** | HTTP requests from exec (prefer `http_request` tool when secrets are involved) | `apt install curl` |
| **trash-cli** | Safe file deletion (`trash` > `rm`) | `apt install trash-cli` |

## Search & Text

| Tool | Purpose | Install |
|------|---------|---------|
| **ack** | File content search (preferred over grep for searching codebases) | `apt install ack` |
| **mdq** | Query markdown files by heading — avoids reading entire large docs into context | `cargo install mdq` |

## Coding Agents

| Tool | Purpose | Install |
|------|---------|---------|
| **Claude Code** | Complex coding, refactoring, architecture tasks | `npm install -g @anthropic-ai/claude-code` |
| **OpenCode** | Straightforward coding tasks, cost-sensitive work | See [opencode.ai](https://opencode.ai) |

## Voice

| Tool | Purpose | Install |
|------|---------|---------|
| **edge-tts** | Text-to-speech for voice note replies | `pip install edge-tts` |
| **ffmpeg** | Audio format conversion (OGG voice notes) | `apt install ffmpeg` |

Voice transcription uses the Groq Whisper API (no local install needed).

## Calendar & Scheduling

| Tool | Purpose | Install |
|------|---------|---------|
| **gcalcli** | Google Calendar CLI — read/write events, check availability | `pip install gcalcli` |

## Secrets & Security

| Tool | Purpose | Install |
|------|---------|---------|
| **Bitwarden CLI** | Dynamic secret access with approval-gated unlocking | `npm install -g @bitwarden/cli` |
| **aisudo** | Telegram-approved privilege escalation | Ships with clod (see setup.sh) |

## Monitoring

| Tool | Purpose | Install |
|------|---------|---------|
| **scc** | Source code line counter (for repo stats) | `go install github.com/boyter/scc/v3@latest` |
| **Netdata** | System metrics API — CPU, memory, disk, per-process stats | See [netdata.cloud](https://netdata.cloud) |

## Optional

| Tool | Purpose | Install |
|------|---------|---------|
| **go** | Required to build clod from source, also useful for agents running Go tools | See [go.dev](https://go.dev) |
| **git** | Version control — agents commit their own work | `apt install git` |
| **node/npm** | Required for Claude Code and Bitwarden CLI | `apt install nodejs npm` |
