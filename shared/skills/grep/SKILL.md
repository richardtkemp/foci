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

## Flag translation — `-r` and `-n` are the trap

**The two flags grep muscle-memory reaches for first, `-r` and `-n`, are exactly the two that mean something else in ack and rg.** Neither tool needs a recursion flag — recursion is their default — so the fix is not to translate `-rn`, it is to **stop typing it**. `ack 'pattern' path/` and `rg 'pattern' path/` are already what `grep -rn 'pattern' path/` was reaching for.

| Intent | grep | ack | rg |
|---|---|---|---|
| Recurse into subdirs | `-r` *(required)* | **default** — no flag | **default** — no flag |
| Show line numbers | `-n` | **default** — no flag | `-n` |
| Case-insensitive | `-i` | `-i` | `-i` |
| Context lines | `-C N` | `-C N` | `-C N` |
| Count matches | `-c` | `-c` | `-c` |
| Files with matches only | `-l` | `-l` | `-l` |
| Restrict to a language | `--include='*.go'` | `--go` | `-t go` |
| Fixed string, no regex | `-F` | `-Q` | `-F` |

What the two trap flags actually do:

| Flag | In grep | In ack | In rg |
|---|---|---|---|
| `-n` | line numbers | **`--no-recurse`** — searches the given dir only | line numbers |
| `-r` | recursive | recurse *(already default; harmless)* | **`--replace`** — consumes the next arg |

Both failure modes are silent and neither looks like a flag error:

```bash
ack -n 'OnSubagentText' internal/   # → 0 hits. Only searched internal/ itself,
                                    #   which holds no .go files. Reads as "absent".
ack 'OnSubagentText' internal/      # → 31 hits.

rg -rn X                            # parses as `-r n` → replaces every match with "n":
                                    #   VoiceOverlay → nOverlay, seekTo → n.
                                    #   Looks like an output/pipe bug; it is self-inflicted.
rg -n X                             # line numbers, recursion implied.
```

`ack -n` is the more dangerous of the two, because its output is a perfectly well-formed empty result. A zero-hit search and a genuine absence are indistinguishable — so a mistyped flag becomes a confident false "nothing references this" with no signal that anything went wrong. (Bit me 2026-07-27: reported "foci reads neither field — zero hits in Go source" to the user off an `ack -n` run; the search had only looked at the repo root.)

**Don't reach for a different tool to double-check an empty result** — `grep` here is a shim function (ugrep, with its own hidden/ignore exclusions), so a "second opinion" from it is not independent and can mislead in the other direction. Get the flags right the first time instead.

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
# ❌ WRONG - searching files with grep
grep -r "pattern" .

# ✅ RIGHT - use ack instead
ack "pattern"
```

### ripgrep (`rg`) is also available

`rg` is installed and is **faster than ack** (especially on large trees). The trade-off: ack's defaults are more conservative — it excludes a lot of likely-unwanted files out of the box (caches, build artifacts, VCS dirs, minified bundles) with smarter file-type detection, so its results are often cleaner without extra flags. `rg` respects `.gitignore` but doesn't have ack's broader built-in exclusion set. Reach for `rg` when speed matters or ack feels slow; stick with ack when you want its tidy defaults.

## ripgrep (rg) — fast, but different defaults

`rg` is much faster than ack and fine to reach for. Its flag traps are in the translation
table above (`-r` is `--replace` — that one cost me a misfiled foci output bug on
2026-07-07). What the table doesn't cover is that its *filtering* differs too:

- **Filtering isn't ack's.** rg honours `.gitignore`/`.ignore` and skips hidden files +
  binaries by default; ack skips build artifacts via its own built-in list but *shows*
  hidden dotfiles. So outside a git repo rg won't auto-skip build dirs, and inside one it
  hides dotfiles ack would surface — pick per what you're hunting (`rg -uu` disables the
  ignore/hidden filtering; `ack -f` lists what ack would search).
- File-type filters differ: ack `--python`/`--js` vs rg `-t py`/`-t js`.

## Configuration

Local `.ackrc` files affect searches run from that directory (based on pwd), not searches looking into that directory from elsewhere.

A repo-level `.ackrc` at the repo root applies whenever ack runs from anywhere inside that repo.

## Concluding an *absence*

**Never `2>/dev/null` a search you will conclude an *absence* from.** A permission-denied on a root-only file is silently discarded and the empty
result reads as "it doesn't exist" — seen 2026-07-23, where `grep -r /etc/cron.d/ 2>/dev/null`
made a real (mode 0600) cron job look nonexistent and I said so. Discard stderr only when
the search is *expected* to hit unreadable paths and you're acting on what it **found**, never on
what it didn't. When hunting something that may be root-owned, use `sudo` and read the warnings.

Note the `grep` shim too: bare `grep` is really ugrep with `-G --ignore-files --hidden -I
--exclude-dir=.git…`, so it skips ignored/hidden-excluded files a real `grep` would report.

**`pgrep -f` / `pkill -f` take a REGEX, not a literal.** Unescaped metacharacters in the pattern
silently change what it matches, and the result is another well-formed empty set:

```bash
pgrep -Af "time.sleep(1800)"  # → nothing. `(1800)` is a capture group, so this really
                              #   searches for the literal `time.sleep1800`, which no
                              #   cmdline contains. Reads as "the process is gone".
pgrep -Aaf "python3 -c"       # → found it, alive, 20 minutes in.
```

Seen 2026-07-27 while checking whether a long-running process had been killed at a timeout
boundary: the false negative landed exactly where a kill was expected, so it looked like a clean
positive result. Use `-F`-style literal thinking: escape the metacharacters, or match a distinctive
plain-text substring, or match a PID/lockfile instead. (Always pass `-A`/`--ignore-ancestors`, as
above — without it `-f` also matches the *calling shell's* cmdline. Separate trap, same tool.)
