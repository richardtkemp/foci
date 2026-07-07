---
name: grep
description: Finding text on disk — use ack, not grep (grep is only for filtering the piped output of other commands). Conventions for fast, correct file-content search across a codebase.
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

## ripgrep (rg) — fast, but different defaults and a flag trap

`rg` is much faster than ack and fine to reach for — but don't assume its flags or
filtering match ack's:

- **`-r` means `--replace`, not "recursive".** rg is recursive by default. `rg -rn X`
  parses as `-r n` → "replace every match with the string `n`", so matches print
  mangled: `VoiceOverlay` → `nOverlay`, `seekTo` → `n`. It looks like a display/pipe
  bug; it is self-inflicted. For line numbers use `-n` (or `--line-number`); recursion
  needs no flag. (Bit me 2026-07-07 — I misfiled it as a foci output bug.)
- **Filtering isn't ack's.** rg honours `.gitignore`/`.ignore` and skips hidden files +
  binaries by default; ack skips build artifacts via its own built-in list but *shows*
  hidden dotfiles. So outside a git repo rg won't auto-skip build dirs, and inside one it
  hides dotfiles ack would surface — pick per what you're hunting (`rg -uu` disables the
  ignore/hidden filtering; `ack -f` lists what ack would search).
- File-type filters differ: ack `--python`/`--js` vs rg `-t py`/`-t js`.

## Configuration

Local `.ackrc` files affect searches run from that directory (based on pwd), not searches looking into that directory from elsewhere.
