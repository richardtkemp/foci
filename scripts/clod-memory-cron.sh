#!/bin/bash
# Periodic memory write prompt for clod
CLOD_ADDR=127.0.0.1:18791
export CLOD_ADDR

clod wake "Review this session and write any important memories to your memory files. Include: lessons learned, decisions made, project state changes, and anything you'd want to know next session. Be specific — vague summaries are useless." 2>/dev/null
