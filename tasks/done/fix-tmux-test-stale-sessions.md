# Task: Fix stale tmux session test failure (#48)

## Problem

`TestTmuxWakeRoutesToCorrectAgent` fails when stale tmux sessions exist from previous interrupted test runs. The test creates tmux sessions but if a previous test run was interrupted, those sessions still exist and the new test fails.

## Fix

Simple: in the test setup, kill any pre-existing session with the same name before creating a new one. Add a `tmux kill-session -t NAME 2>/dev/null` (or equivalent cleanup) at the start of the test, before creating sessions.

## When done: commit with descriptive message, push.
