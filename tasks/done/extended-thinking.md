# Task: Extended thinking / adaptive thinking mode

## Context
Todo #11. Foci currently sends no `thinking` config to the Anthropic API — Claude uses no extended thinking. Opus 4.6 supports adaptive thinking with interleaved thinking between tool calls, which is a major reasoning boost.

## Requirements

1. Add `thinking` config (global and per-agent)
   - Values: `"off"` (default, current behaviour), `"adaptive"` 
   - When adaptive: send `thinking.type = "adaptive"` in API requests
   - Interleaved thinking (think between tool calls) is automatically enabled with adaptive mode
2. Add the `ThinkingConfig` struct to the Anthropic API client:
   ```go
   type ThinkingConfig struct {
       Type         string `json:"type"`                    // "adaptive" or "enabled"
       BudgetTokens int    `json:"budget_tokens,omitempty"` // only for "enabled"
   }
   ```
3. Wire through MessageRequest and the agent layer
4. Thinking blocks in responses need to be handled — they come back as content blocks with type "thinking". They should NOT be sent to Telegram (internal reasoning), but should be preserved in session history
5. Update docs (CONFIG.md, SPEC.md, WIRING.md) and write tests
6. Commit and push when done

## Notes
- Don't implement "enabled" mode with budget_tokens yet — just "off" and "adaptive"
- Thinking tokens count toward mana, so this is opt-in per agent
- The effort parameter interacts with thinking — both should work together
