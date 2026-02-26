# Task: Slash commands — catch unknown commands with suggestions

## Problem
Messages starting with `/` that don't match a command are passed through to the agent as normal messages. They should be intercepted by the command system and responded to with "did you mean?" suggestions.

## Requirements

### 1. Unknown commands should NOT pass through to the agent
When `Dispatch` receives a `/something` that doesn't match any registered command, it should return a helpful message instead of `("", false)`.

### 2. Fuzzy matching / suggestions
When a command isn't found:
- Check for close matches using Levenshtein distance or prefix matching against registered command names
- If there are close matches (distance ≤ 2, or shared prefix ≥ 3 chars), suggest them: "Unknown command `/sesstion`. Did you mean `/sessions`?"
- If no close matches, show: "Unknown command `/foo`. Type `/help` to see available commands."
- Return `(suggestion, true)` so it's handled by the command system, not the agent

### 3. Don't break existing behaviour
- Messages NOT starting with `/` still pass through to the agent normally
- Valid commands still work as before
- The wizard system still intercepts when active

## Implementation
- Add a simple Levenshtein distance function (or prefix match — keep it simple)
- Modify `Dispatch` to return suggestions instead of `("", false)` when the message starts with `/` but command isn't found
- The suggestions should be sent as a Telegram message, not injected into the agent conversation

## Update docs
- SPEC.md — mention command fuzzy matching
