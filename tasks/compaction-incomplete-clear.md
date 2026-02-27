# Bug: Compaction not clearing old messages

## Problem
After compaction at 00:46 on 2026-02-27, the session retained 652 messages and ~153K tokens. The compaction summary itself was fine (~5K tokens), but the old messages weren't cleared. A second compaction triggered at 08:40 with only ~8K tokens of new conversation — it worked correctly, dropping to 29 messages.

## Evidence
From `api-payload.jsonl` (session `agent:clutch:chat:5970082313`):
- At 08:03 (first call after 00:46 compaction): 652 messages, 153K tokens
- At 08:40 (second compaction triggers): 678 messages, 161K tokens  
- At 08:41 (after second compaction): 29 messages, 26K tokens

The second compaction clearly worked. The first one generated a good summary but didn't remove the old messages.

## Session file
`/home/foci/data/sessions/agent/clutch/chat/5970082313.jsonl`

## Process logs
`/home/foci/logs/foci.log`

## API payload logs  
`/home/foci/logs/api-payload.jsonl`

## What to investigate
1. Look at the compaction code — what's supposed to happen after the summary is generated? How are old messages replaced?
2. Check foci.log around 00:46 on 2026-02-27 for any errors or warnings during compaction
3. Is there a race condition or error path where the summary gets prepended but old messages aren't removed?
4. Why did the second compaction (08:40) work correctly but the first (00:46) didn't?
5. Update SPEC.md and relevant docs with any fixes. Push when done.
