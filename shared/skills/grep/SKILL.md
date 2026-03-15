---
name: grep
description: File search conventions using ack and grep. Use ack exclusively for searching file contents on disk. Reserve grep only for filtering the output of other commands via pipes.
---

# Grep

## Convention

**Use `ack` for file searching. Use `grep` only for pipes.**

### When to Use ack

Use `ack` whenever searching for patterns in files on disk:

```bash
# Search for pattern in current directory and subdirectories
ack "pattern"

# Search with case-insensitivity
ack -i "pattern"

# Search only specific file types
ack --python "pattern"
ack --js "pattern"

# Search with context lines
ack -C 3 "pattern"

# List files that would be searched
ack -f
```

**Why ack:**
- Automatically respects `.gitignore` and `.ackrc`
- Defaults to recursive search
- Smart file type detection
- Better default output formatting
- Excludes binary files and common build artifacts by default
- Long lines (e.g. jsonl) are automatically truncated and centred on the match. Use **jq** for structured JSON searching.

### When to Use grep

Use `grep` ONLY for filtering output from other commands:

```bash
# Filter command output
ps aux | grep nginx
docker ps | grep -v CONTAINER
journalctl -f | grep error

# Filter file content passed via stdin
cat file.txt | grep pattern
```

**Never use grep for file searching:**
```bash
# WRONG - searching files with grep
grep -r "pattern" .

# RIGHT - use ack instead
ack "pattern"
```

## Configuration

Local `.ackrc` files affect searches run from that directory (based on pwd), not searches looking into that directory from elsewhere.
