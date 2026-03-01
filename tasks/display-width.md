# Task: Add display_width Config Parameter

Add a `display_width` config option that controls the character width used for visual elements like dividers.

## Config

- **Key:** `display_width` (integer)
- **Default:** 44
- **Available at:** `[telegram]` (global), `[telegram.defaults]`, and per-agent `[telegram.agents.<name>]`
- **Cascades** like other per-agent config: agent → defaults → global → 44

## Usage

Use `display_width` to determine how many characters wide to draw dividers. Currently the thinking separator in `sendReplyWithFullThinking` and `formatThinkingExpanded` uses a hardcoded `————————————————` string. Replace that with em-dashes repeated to fill `display_width`.

## Implementation notes

- Add the field to the config structs alongside existing display options like `show_thinking`
- Make it available on the Bot (or however the other display config reaches the send functions)
- Replace hardcoded divider strings with `strings.Repeat("—", displayWidth)`
- Update SPEC.md, docs/CONFIG.md with the new option

## Tests

- Config cascade works (agent overrides defaults overrides global overrides 44)
- Divider string is correct width
