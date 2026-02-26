# Task: Unified table alignment utility

## Problem
Column alignment is implemented separately in multiple places:
- `config/display.go` — `formatTable()` for config key-value tables
- `telegram/markdown.go` — `formatTable()` for markdown table rendering  
- `command/builtins.go` — one-off alignment in /cache, /cost, /tools, /sessions, /bitwarden (at least 6 separate implementations)
- `command/sessions.go` — another one-off alignment

The config table has a specific bug: column padding doesn't match the headings.

## Requirements

1. Create a single `table` package (or utility in an appropriate existing package) with a reusable plain-text table formatter
   - Takes headers and rows ([][]string)
   - Auto-measures column widths using display width (handles unicode/emoji)
   - Generates aligned output with separator line
   - Supports left-alignment (default) and optionally right-alignment per column
2. Refactor all the above callsites to use it
3. Fix the config table heading/column mismatch
4. The telegram/markdown.go formatTable is different (it reformats *markdown* pipe-tables from LLM output) — that one may stay separate if it genuinely needs different logic, but evaluate whether it can share the core width-measurement
5. Tests for the shared formatter
6. Update any relevant docs
7. Commit and push when done

## Note
This is a refactor — behaviour should be identical (or better) after, not different. The goal is one alignment function, not N.
