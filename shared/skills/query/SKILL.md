---
name: query
description: "Query structured data (JSON, JSONL, TOML, YAML, XML, CSV, Markdown, SQLite) using jq, mdq, yq, and sqlite3. Use instead of grep/cat/sed for structured files."
owner: foci
seeded: true
---

> **This `SKILL.md` is yours to customise** (seed-if-missing — override it, add your own sibling files). Changes to it survive a restart, but they live only on this install: to change the skill for every agent, edit it in the foci repo (`shared/skills/query/`).

# Structured Query Tools

Query structured data instead of dumping it into context. Three tools, one per format family.

## jq — JSON and JSONL

```bash
# Extract a field
jq '.field' file.json

# Extract nested field
jq '.parent.child' file.json

# Array iteration
jq -r '.[] | .name' file.json

# Filter JSONL logs
cat log.jsonl | jq 'select(.level == "ERROR")'

# Multiple fields
jq '{name: .name, cost: .cost_usd}' file.json

# Count entries
cat log.jsonl | jq -s 'length'

# Sort and limit
jq -s 'sort_by(.cost_usd) | reverse | .[:5]' file.jsonl
```

**Never grep JSONL** — lines are multi-KB JSON blobs. Always use jq.

## mdq — Markdown

```bash
# Extract a section by heading
mdq '# Section Name' file.md

# Nested section
mdq '## Parent > ### Child' file.md

# List items
mdq '# Section > list' file.md

# Code blocks
mdq '# Section > code' file.md

# Tables
mdq '# Section > table' file.md
```

Use instead of `cat` for large markdown files. Extract just the section you need.

**mdq selects sections, not headings.** `mdq '# Foo'` returns the heading AND all its content.

### mds — find the section you need without reading the file

`mds` (shipped in `shared/scripts/`, on PATH as `mds`) is the discovery front-end to mdq, for
when you don't know a file's structure. Two moves: see the shape, then take one section.

```bash
mds file.md                # list all headings (a TOC)
mds file.md deploy         # match a heading, extract just that section
mds file.md "setup toml"   # multi-word: matches a heading containing both, in order
mds SPEC.md websocket      # find the WebSocket section without knowing its exact heading
```

Matching is tiered, most precise first — whole word, then substring (`auth` finds
`Authentication`), then all-words-in-order, then fuzzy subsequence if `fzf` happens to be
installed. Only that last tier needs fzf and it degrades silently without it. No match prints
the available headings so you can refine rather than guess again.

**Prefer `mds` over `mdq` when exploring an unfamiliar file** — reading a whole large markdown
file to answer one question is the expensive mistake this exists to avoid.

## yq — TOML, YAML, XML, CSV, and more

yq auto-detects format from file extensions. No `-p` flag needed for `.toml`, `.yaml`, `.yml`, `.xml`, `.csv` files.

```bash
# Read TOML
yq '.section.key' file.toml
yq -oy '.' file.toml                  # TOML → YAML (readable)
yq -oj '.' file.toml                  # TOML → JSON

# Read YAML
yq '.key' file.yaml
yq '.items[0].name' file.yaml

# Read XML
yq '.root.element' file.xml

# Read CSV
yq '.[0]' file.csv                    # First row

# List all keys at a level
yq 'keys' file.toml
yq '.section | keys' file.toml

# Filter arrays
yq '.agents[] | select(.id == "myagent")' file.toml

# Format conversion
yq -o json file.toml                  # TOML → JSON
yq -o toml file.yaml                  # YAML → TOML

# Piping (no file extension — use -p to specify input format)
cat something | yq -p toml '.key'
```

**Supported formats:** YAML, JSON, TOML, XML, CSV, TSV, HCL, properties, base64, URI, shell, Lua, INI.

**Output format flag:** `-oy` (YAML), `-oj` (JSON), `-ot` (TOML), `-ox` (XML).

**TOML root must be a mapping:** for `keys`, or any filter that emits a list or multiple roots, ask for `-oy`/`-oj` instead — TOML cannot represent them at the top level, so the default TOML output errors on a query that is otherwise correct.

**Input format flag (`-p`):** Only needed when piping or reading files with non-standard extensions.

## When to use what

| Data format | Tool | Notes |
|-------------|------|-------|
| `.json`, `.jsonl` | jq | Pipe JSONL, don't load whole file |
| `.md` | mdq | Query by heading structure |
| `.toml` | yq | Config files, foci.toml |
| `.yaml`, `.yml` | yq | Docker compose, k8s, etc. |
| `.xml` | yq | |
| `.csv` | yq | |
| `.db`, `.sqlite` | sqlite3 -readonly | foci's own stores: state.db, api.db, app-frames.db |

## sqlite3 — `.db` files

**A numeric column compared against `strftime()`/`date()` matches nothing, silently.** Those return TEXT, and SQLite orders every INTEGER below every TEXT, so `epoch_col > strftime('%s', ...)` is never true — zero rows, no error, reads as absence. Cast the bound to INTEGER, or compare on the formatted side (`datetime(col/1000,'unixepoch','localtime') > '2026-09-02 12:00'`).

## Philosophy

**Structured queries beat line numbers.** Line numbers change on edits; section titles and keys usually don't. You can guess or fuzzy-match headings and keys when you can't guess line numbers.

**Optimistic pattern:** Try the key you expect, fall back to listing keys:
```bash
yq '.agents[0].thinking' file.toml || yq '.agents[0] | keys' file.toml
```
