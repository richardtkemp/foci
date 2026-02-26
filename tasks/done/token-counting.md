# Task: Token counting API for /context command

## What
Add support for Anthropic's `POST /v1/messages/count_tokens` endpoint. Same request shape as Messages, returns `{input_tokens: N}`. Free to call, separate rate limits.

## Use case
The `/context` command currently shows character counts and estimates tokens at ~4:1. With the counting API, we can show exact token counts per system file and for the conversation.

## Implementation
1. Add `CountTokens(ctx, req) (int, error)` method to the anthropic client
2. Update `/context` command to optionally use token counting (call once with the full request to get exact total)
3. Show both chars and tokens in `/context` output

## Note
This is low priority — character estimation works fine for compaction. The main benefit is more accurate `/context` display.

## Update docs
- SPEC.md if relevant
