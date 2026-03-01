# TODO #41: Automatic System Diagnosis on Crashes/Reboots

## Goal
When foci starts, detect if the restart was unexpected (crash, OOM, reboot) and automatically run diagnostics, reporting findings to the user.

## Current State
- `SendStartupNotification()` in `telegram/bot.go:1249` sends a simple "restarted at HH:MM:SS" message
- No crash/reboot detection exists
- System signals available: `/proc/uptime`, `systemctl show foci -p ActiveEnterTimestamp`, `journalctl -u foci`

## Design

### 1. Track last clean shutdown
On graceful shutdown (SIGTERM/SIGINT handler in main.go), write a timestamp to the state store:
```
state.Set("system:last_clean_shutdown", time.Now().Unix())
```

### 2. On startup, classify the restart
Read `system:last_clean_shutdown` and compare with current time:
- **Clean restart**: shutdown timestamp exists and is recent (< 5 min before service start)
- **Crash/unexpected**: no shutdown timestamp, or large gap between shutdown and restart
- **System reboot**: system uptime (`/proc/uptime`) < service uptime gap (system booted after last shutdown)

### 3. Run diagnostics on unexpected restart
When classified as crash/unexpected, gather:
- `journalctl -u foci --since <last_shutdown> -p err` — service errors before crash
- `dmesg | grep -i oom` — OOM killer activity
- `journalctl -k --since <last_shutdown> | grep -iE 'oom|panic|error'` — kernel issues
- Previous exit code from systemd: `systemctl show foci -p ExecMainStatus`

### 4. Report via startup notification
Enhance `SendStartupNotification` to include diagnosis:
- Clean: keep current simple message
- Unexpected: append diagnostic summary (exit code, OOM status, last errors)

## Implementation

### Files to change
- **main.go**: Add `state.Set("system:last_clean_shutdown", ...)` in the shutdown handler (near signal handling)
- **main.go** or new **startup/diagnosis.go**: Add `DiagnoseRestart(stateStore) *DiagnosisResult` function
- **telegram/bot.go**: Enhance `SendStartupNotification` to accept optional diagnosis info

### Notes
- Diagnostics need `journalctl` access — foci user may not have permission without sudo. Check if `systemd-journal` group membership suffices, or read from foci's own log files in `~/logs/` instead.
- Keep it simple: don't block startup on diagnostics. Run async, send report when ready.
- The foci log files at `~/logs/` are the easiest source — no sudo needed. Check for ERROR/FATAL lines in the most recent log file.
