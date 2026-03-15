---
name: query
description: "Query structured data (JSON, JSONL, TOML, YAML, XML, CSV, Markdown) using jq, mdq, and yq. Use instead of grep/cat/sed for structured files."
---
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

## Philosophy

**Structured queries beat line numbers.** Line numbers change on edits; section titles and keys usually don't. You can guess or fuzzy-match headings and keys when you can't guess line numbers.

**Optimistic pattern:** Try the key you expect, fall back to listing keys:
```bash
yq '.agents[0].thinking' file.toml || yq '.agents[0] | keys' file.toml
```
