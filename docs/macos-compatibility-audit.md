# macOS Compatibility Audit

**TODO #235** — What's needed for macOS support.

## Blockers

### 1. /proc filesystem (HIGH)
macOS has no `/proc`. Multiple packages depend on it:

| File | Usage | Impact |
|------|-------|--------|
| `resources/memory_guard.go` | `/proc/meminfo`, `/proc/[pid]/status`, `/proc/pressure/memory` | Memory guard won't function at all |
| `tools/tmux.go` | `/proc/<pid>/task/<pid>/children` for process tree walking | tmux kill child cleanup won't work |
| `tools/tmux_memory.go` | `/proc/{pid}/status`, `/proc/meminfo` | tmux memory monitoring broken |
| `startup/diagnosis.go` | `/proc/uptime` | Crash diagnosis won't work |
| `secrets/secrets.go` | `/proc/self/environ` (blocked path list) | Minor — just a security blocklist entry |

**Alternative:** Use `sysctl` for system memory, `ps` for per-process RSS, `libproc` (via cgo or `ps`) for process trees. PSI (Pressure Stall Information) has no macOS equivalent — memory guard pressure gating would need a different signal (e.g. memory_pressure dispatch source via `libdispatch`, or just skip the PSI gate on macOS).

### 2. systemd (HIGH)
No systemd on macOS.

| File | Usage | Impact |
|------|-------|--------|
| `command/builtins.go` | `systemctl restart foci` in /restart command | Can't restart via systemd |
| `setup.sh` | Creates systemd unit file, enables service | Install script won't work |
| `secrets/secrets.go` | References `SupplementaryGroups` in systemd unit error messages | Misleading error messages |
| `tools/procattr.go` | References systemd `AmbientCapabilities` in warnings | Misleading warnings |

**Alternative:** launchd plist for service management. `/restart` could use `launchctl kickstart`. setup.sh needs a macOS branch.

### 3. aisudo (HIGH)
Pre-compiled ELF x86-64 Linux binary. Not in the foci repo — external dependency.

**Alternative:** Needs cross-compilation for macOS (both x86_64 and arm64/Apple Silicon). Source location unknown from this audit.

### 4. Unix user/group model (MEDIUM)
| Area | Issue |
|------|-------|
| `secrets/secrets.go` | Requires `foci-secrets` group, checks `root` ownership (uid 0), uses `syscall.Stat_t` |
| `tools/procattr.go` | Drops supplementary groups, sets `syscall.Credential` on child processes |
| `setup.sh` | `useradd`, `groupadd`, `usermod`, `chown`, `chmod` — 10 references |

macOS uses `dscl` / `sysadminctl` instead of `useradd`/`groupadd`. The `syscall.Credential` and `syscall.Stat_t` types exist on macOS (Darwin) but behaviour may differ. File permission model is similar but group management commands differ entirely.

**Alternative:** Conditional setup.sh (detect OS), or separate macOS install script. Go code using `syscall.Stat_t` should work on Darwin — test needed.

### 5. Process management (LOW)
| File | Usage | macOS status |
|------|-------|-------------|
| `command/builtins.go` | `syscall.Kill(-pgid, SIGKILL)` — process group kill | Works on macOS ✅ |
| `tools/procattr.go` | `Setpgid`, `Setsid` in `SysProcAttr` | Works on macOS ✅ |
| `resources/memory_guard.go` | `SIGTERM`/`SIGKILL` | Works on macOS ✅ |

Signal handling is POSIX-compatible — no issues expected.

## Non-blockers
- **tmux** — available via Homebrew, works identically
- **Go build** — `GOOS=darwin` cross-compilation supported, no build tags or platform-specific files detected
- **SQLite (state.db, session_index.db, tool_details.db)** — cross-platform ✅
- **Telegram bot API** — cross-platform ✅
- **Anthropic API** — cross-platform ✅

## Summary

| Category | Severity | Effort |
|----------|----------|--------|
| /proc filesystem | HIGH | Large — needs platform abstraction layer or build tags for ~5 files |
| systemd → launchd | HIGH | Medium — setup.sh + /restart + error messages |
| aisudo binary | HIGH | Unknown — depends on source availability |
| User/group management | MEDIUM | Medium — setup.sh macOS branch |
| Process management | LOW | None — already POSIX-compatible |

## Recommendation

The minimum viable approach:
1. **Build tags** (`//go:build linux` / `//go:build darwin`) for /proc-dependent code, with macOS implementations using `sysctl`/`ps`
2. **Disable memory guard on macOS** initially (no PSI equivalent)
3. **launchd plist** instead of systemd unit
4. **macOS install script** or OS detection in setup.sh
5. **aisudo** needs its own macOS build

Estimated effort: 2-3 days of focused work for a developer familiar with both platforms.
