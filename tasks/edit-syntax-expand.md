# Task: Expand edit tool syntax validation to more formats

## Context
The edit tool currently validates .json, .toml, and .go files. Expand to cover any common format with easily verifiable syntax.

## New formats to add

1. **YAML** (.yaml, .yml) — use `gopkg.in/yaml.v3` Unmarshal to validate
2. **XML** (.xml) — use `encoding/xml` stdlib Decoder to validate well-formedness
3. **Python** (.py) — shell out to `python3 -c "import ast; ast.parse(open('FILE').read())"` — syntax check only, no execution. If python3 isn't available, skip validation gracefully (don't error)
4. **Shell** (.sh, .bash) — shell out to `bash -n FILE` — syntax check only, no execution. If bash isn't available, skip gracefully

## Requirements
- Follow the existing registry pattern in tools/syntax.go (map extension to validator function)
- For shell-out validators (python, bash): if the binary isn't available, return nil (skip check), don't fail
- Tests for each new format (valid→invalid rejection, already-invalid pass-through)
- Update SPEC.md and docs/WIRING.md tool descriptions to list all supported formats
- Commit and push when done
