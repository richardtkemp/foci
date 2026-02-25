# Task: Preserve recent messages through compaction

## Context
When compaction runs, the entire conversation is replaced with a summary. This loses the actual recent messages — tone, specifics, exact wording. We want to preserve the most recent N messages so the post-compaction context includes both the summary AND the real recent conversation.

## Requirements

1. New config key `compaction_preserve_messages` (integer, global and per-agent, default 25)
2. When compaction runs, the last N user/assistant messages from before compaction are preserved
3. The post-compaction session should look like:
   - System prompt (unchanged)
   - Compaction summary message — append text like "The last N messages from before compaction follow." to the end of the summary
   - The N preserved messages (in their original user/assistant roles)
   - New conversation continues after
4. The preserved messages count toward the context window — the compaction threshold logic should account for this
5. Update docs (CONFIG.md, SPEC.md) and write tests
6. Commit and push when done
