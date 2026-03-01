# Task: Anthropic Built-in Web Search Tool (#160)

## Problem
Foci uses Brave Search API for web search. Anthropic now offers a built-in server-side web search tool (`web_search_20250305`) that's free on Max plan and produces better results (includes encrypted page content that Claude can read).

## How Server Tools Work
Server tools are different from client tools:
1. You declare them in the `tools` array with `type: "web_search_20250305"` (not `type: "custom"`)
2. Anthropic's servers execute the search in a loop — you may get multiple searches in a single API call
3. Response includes `server_tool_use` and `web_search_tool_result` content blocks
4. These blocks must be preserved in conversation history but don't need client-side handling
5. The response `stop_reason` is `end_turn` (not `tool_use`) since the server handled it

## API Shape
```json
{
  "tools": [
    {
      "type": "web_search_20250305",
      "name": "web_search",
      "max_uses": 5,
      "allowed_domains": ["example.com"],  // optional
      "blocked_domains": ["spam.com"],     // optional
      "user_location": {                    // optional
        "type": "approximate",
        "city": "London",
        "region": "England", 
        "country": "GB",
        "timezone": "Europe/London"
      }
    }
  ]
}
```

Response content blocks include:
- `type: "server_tool_use"` with `name: "web_search"` — Claude's decision to search
- `type: "web_search_tool_result"` with search results (URLs, titles, encrypted content)
- `type: "text"` — Claude's response using the search results

## Implementation Plan

### 1. Anthropic Package Changes (`anthropic/`)
- Add server tool types to the tool/content block definitions
- `ServerToolUse` content block type
- `WebSearchToolResult` content block type  
- Support `type: "web_search_20250305"` in tool definitions (alongside existing `type: "custom"`)
- Ensure these content blocks are preserved in message serialization/deserialization

### 2. Tool Registry Changes (`tools/`)
- New tool type concept: "server tool" vs "client tool"
- Server tools are declared to the API but NOT executed client-side
- When processing tool_use responses, skip server_tool_use blocks (already handled)
- The existing web_search tool becomes a fallback

### 3. Config
```toml
[tools]
web_search_provider = "anthropic"  # or "brave" (default: "anthropic" if on Max plan)

[tools.web_search]
max_uses = 5
# user_location derived from agent config or USER.md
```

### 4. Exec Bridge
- `foci_web_search` should continue to work — it can route to either provider
- If using Anthropic's server tool, the bridge can't intercept it (it's server-side)
- Keep Brave as the explicit bridge search, Anthropic as the implicit model search

### 5. Migration Path
- Default: Anthropic web search (server tool) + Brave as `foci_web_search` in exec
- Config toggle to use Brave for everything (for users without Max plan)
- The `web_search` tool definition changes from client to server type

### 6. Also: Web Fetch Tool
Anthropic also has `web_fetch_20250305` — same pattern. Consider adding both.

## Key Complexity
- Message serialization must handle new content block types without breaking existing sessions
- Compaction must preserve or summarize web search results
- Token counting for server tool results
- The tool_use loop in agent.go needs to distinguish server vs client tool calls

## Files to Modify
- `anthropic/types.go` — new content block types
- `anthropic/api.go` — tool definition serialization  
- `agent/agent.go` — tool loop to skip server tool results
- `tools/web.go` — conditional tool registration
- `main.go` — config wiring
- `config/config.go` — new config fields

## Tests
- Round-trip serialization of server tool content blocks
- Agent loop correctly handles mixed server+client tool responses
- Config toggle between providers
