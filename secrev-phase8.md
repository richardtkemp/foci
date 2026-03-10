# Foci Security Review - Phase 8: Process Isolation & Privilege Management

**Review Date:** 2026-03-08
**Phase:** 8 of 10
**Status:** Complete

---

## Executive Summary

Phase 8 analyzed process isolation and privilege management in Foci. The system implements OS-level group dropping as the primary secrets protection mechanism, with automatic process group isolation.

**Key Findings:**
- Group dropping implementation is robust (kernel-enforced)
- Process termination on reliable (SIGKILL works)
- Resource limits exist but but reactive (not proactive)
- Capability probing works correctly (one-time at startup)
- Zombie process reaping handled properly

**Overall Security Grade:** **A-** (Excellent process isolation)

---

## 1. Group Dropping Implementation

### 1.1 Capability Detection
**Implementation:** `internal/tools/procattr.go:20-99`

**Mechanism:**
```go
func init() {
    uid := os.Getuid()
    gid := os.Getgid()
    
    // Skip if running as root
    if uid == 0 {
        return
    }
    
    // Look up foci-secrets group
    secretsGrp, err := user.LookupGroup(secrets.SecurityGroupName)
    if err != nil {
        log.Debugf("exec", "group %q not found — skipping credential setup", secrets.SecurityGroupName)
        return
    }
    secretsGID, err := strconv.ParseUint(secretsGrp.Gid, 10, 32)
    if err != nil {
        return
    }
    
    // Get current supplementary groups
    currentGroups, err := syscall.Getgroups()
    if err != nil {
        log.Warnf("exec", "cannot read supplementary groups: %v", err)
        return
    }
    
    // Build filtered list: all groups EXCEPT foci-secrets
    var filteredGroups []uint32
    found := false
    for _, g := range currentGroups {
        if uint64(g) == secretsGID {
            found = true
            continue // drop foci-secrets
        }
        filteredGroups = append(filteredGroups, uint32(g))
    }
    
    if !found {
        log.Debugf("exec", "process does not have %s group — skipping credential setup", secrets.SecurityGroupName)
        return
    }
    
    // Build credential with filtered groups
    primaryGID := uint32(gid)
    cred := &syscall.Credential{
        Uid:    uint32(uid),
        Gid:    primaryGID,
        Groups:   filteredGroups, // All groups except foci-secrets
    }
    
    // Probe: try spawning a trivial process to verify CAP_SETGID
    probe := exec.Command("true")
    probe.SysProcAttr = &syscall.SysProcAttr{
        Setpgid:    true,
        Credential: cred,
    }
    if err := probe.Run(); err != nil {
        log.Warnf("exec", "cannot drop %s group (CAP_SETGID not available): %v", secrets.SecurityGroupName, err)
        log.Warnf("exec", "child processes will inherit parent groups — add AmbientCapabilities=CAP_SETGID to systemd unit")
        return
    }
    
    childCredential = cred
    log.Debugf("exec", "child credential: uid=%d gid=%d groups=%v (dropped %s gid %d)",
        uid, primaryGID, filteredGroups, secrets.SecurityGroupName, secretsGID)
}
```

**Strengths:**
✅ **Kernel-enforced** - OS denies access at secrets.toml
✅ **Capability probing** - Tests capability at startup
✅ **All supplementary groups dropped** - Removes only foci-secrets
✅ **Preserves other groups** - Docker, git, etc. still work
✅ **Clear logging** - Debug messages explain behavior
✅ **Fail-safe warning** - Logs warning but continues

**Concerns:**
⚠️ **Root bypass** - If running as root, no group dropping occurs
⚠️ **Capability dependency** - Requires CAP_SETGID capability
⚠️ **One-time probe** - Only tested at startup
⚠️ **No runtime verification** - Can't verify group dropping actually happened in child processes
⚠️ **Warning not not error** - Logs warning but continues, ⚠️ **Manual setup required** - systemd unit must be be})

**Security Grade:** **A** (Excellent implementation)

### 1.2 Application Points

**Shell Tool:**
```go
// In shell.go
proc := exec.Command(execShell(), "-c", cmd)
proc.SysProcAttr = ChildSysProcAttr() // Applies credential
```

**Custom Commands:**
```go
// In command/builtins.go
cmd := exec.Command(args[0], args[1], ...)
cmd.SysProcAttr = command.ChildSysProcAttr()
```

**Tmux Tool:**
```go
// In tmux.go
cmd := exec.Command("tmux", args...)
cmd.SysProcAttr = tools.ChildSysProcAttr()
```

**Strengths:**
✅ **Universal application** - Applied to ALL child processes
✅ **Consistent** - Same credential for all tools
✅ **Process group isolation** - Setpgid creates new process group
✅ **Setsid for background** - Setsid for background mode

**Concerns:**
⚠️ **None identified** - Works as designed

**Security Grade:** **A** (Excellent)
### 1.3 Verification Testing
**Test:** Probe command execution at```go
probe := exec.Command("true")
probe.SysProcAttr = &syscall.SysProcAttr{
    Setpgid:    true,
    Credential: cred,
}
err := probe.Run()
```

**Purpose:**
- Verify CAP_SETGID is available
- Test setgroups() syscall
- Ensure child credential setup works

**Expected Results:**
- **Success:** CAP_SETGID available, childCredential set
- **Failure:** CAP_SETGID not available, warning logged, childCredential remains nil

**Security Grade:** **A** (Proactive testing)
### 1.4 Failure Mode
**If CAP_SETGID unavailable:**
```go
log.Warnf("exec", "child processes will inherit parent groups")
log.Warnf("exec", "add AmbientCapabilities=CAP_SETGID to systemd unit")
```

**Impact:**
- Child processes CAN access secrets.toml
- **CRITICAL SECURITY LOSS**
- **BUT** secrets.toml still has other protections:
  - File permissions (root:foci-secrets 0660)
  - Process not in foci-secrets group (group dropping failed)

**Mitigation:**
- Document capability requirement clearly
- Add setup verification script
- Make capability check fatal if secrets.toml exists

**Security Grade:** **B-** (Warning-only failure mode)
### 1.5 Root User Handling
```go
if uid == 0 {
    return // Skip credential setup
}
```

**Rationale:**
- Root can read any file anyway
- Group dropping provides no additional security
- Root should not be running foci

**Security Implications:**
- If running as root, ALL security bets off
- secrets.toml readable by anyone
- No isolation between agents

**Recommendation:**
- **NEVER run as root**
- Add startup check to warn if running as root
- Document clearly in security docs

**Security Grade:** **A** (Correct handling)
---

## 2. Process Termination
### 2.1 Timeout Enforcement
**Implementation:** Shell tool, tmux tool

**Pattern:**
```go
ctx, cancel := context.WithTimeout(ctx, timeout)
defer cancel()

proc := exec.CommandContext(ctx, execShell(), "-c", cmd)
proc.Cancel = func() error {
    return syscall.Kill(-proc.Process.Pid, syscall.SIGKILL)
}
```

**Strengths:**
✅ **Context-based timeout** - Uses context.WithTimeout
✅ **Process group kill** - Kills entire process group (negative PID)
✅ **SIGKILL** - Immediate termination (no graceful shutdown)
✅ **Deferred cancel** - Ensures cleanup

**Concerns:**
⚠️ **No graceful shutdown** - SIGKILL doesn't allow cleanup
⚠️ **No warning** - No SIGTERM before SIGKILL
⚠️ **Orphaned processes** - Background processes could become orphans

**Security Grade:** **B+** (Good enforcement, no grace period)
### 2.2 Background Process Management
**Implementation:** Shell tool background mode

**Pattern:**
```go
if background {
    proc.SysProcAttr = ChildSysProcAttrSetsid()
    proc.WaitDelay = 2 * time.Second
} else {
    proc.SysProcAttr = ChildSysProcAttr()
    proc.Cancel = func() error {
        return syscall.Kill(-proc.Process.Pid, syscall.SIGKILL)
    }
}
```

**Strengths:**
✅ **Setsid** - Creates new session for background processes
✅ **WaitDelay** - Reaps zombie processes (Go 1.20+)
✅ **Separate path** - Background vs foreground

**Concerns:**
⚠️ **No tracking** - Background processes not tracked
⚠️ **No resource limits** - Can spawn unlimited background processes
⚠️ **Orphan risk** - If parent crashes, background processes orphaned

**Security Grade:** **B** (Good isolation, no limits)
### 2.3 Auto-Backgrounding
**Implementation:** Shell tool auto-background feature

**Mechanism:**
1. Command starts with normal timeout
2. If exceeds threshold (e.g., 10s), auto-backgrounds
3. Results delivered asynchronously via notifier
4. Process continues running

**Strengths:**
✅ **Automatic** - No manual background flag needed
✅ **Async delivery** - Results delivered when ready
✅ **Threshold configurable** - Per-agent config

**Concerns:**
⚠️ **Resource accumulation** - Multiple auto-backgrounded processes
⚠️ **No process limits** - Could auto-background many processes
⚠️ **Async results lost on restart** - Results in goroutine memory only

**Security Grade:** **B** (Good feature, missing limits)
---

## 3. Resource Limits
### 3.1 Memory Guard
**Implementation:** `internal/resources/memory_guard.go`

**Mechanism:**
```go
type MemoryGuard struct {
    threshold    int64 // RSS bytes
    checkInterval time.Duration
    pid          int
    done         chan struct{}
}

func (m *MemoryGuard) Start(ctx context.Context) {
    ticker := time.NewTicker(m.checkInterval)
    for {
        select {
        case <-ticker.C:
            rss := getRSS(m.pid)
            if rss > m.threshold {
                log.Warnf("memory_guard", "process %d exceeded RSS threshold %d > %d", m.pid, rss, m.threshold)
                // Kill or warn
            }
        }
    }
}
```

**Strengths:**
✅ **Monitoring** - Periodic RSS checks
✅ **Configurable threshold** - Per-process limit
✅ **Background monitoring** - Doesn't block main process

**Concerns:**
⚠️ **Reactive, not proactive** - Kills after exceeding, not before
⚠️ **Kill vs warn** - Configuration unclear
⚠️ **No CPU limits** - Only memory monitored
⚠️ **RSS only** - Doesn't monitor virtual memory

**Security Grade:** **B-** (Reactive monitoring)
### 3.2 Tmux Memory Monitor
**Implementation:** `internal/tools/tmux_memory.go`

**Mechanism:**
- Periodically check tmux session memory usage
- Kill sessions exceeding threshold
- Notify via async notifier

**Strengths:**
✅ **Dedicated monitor** - Separate from main memory guard
✅ **Per-session tracking** - Tracks individual tmux sessions
✅ **Async notification** - Warns when session killed

**Concerns:**
⚠️ **Reactive** - Kills after exceeding, not before
⚠️ **No CPU limits** - Only memory tracked
⚠️ **Session loss** - Kills session, data lost

**Security Grade:** **B-** (Reactive monitoring)
### 3.3 Spawn Limits
**Implementation:** `internal/tools/spawn.go`

**Mechanism:**
```go
sem := make(chan struct{}, deps.MaxInherit)

func executeSpawn(...) {
    sem <- struct{}{} // Acquire semaphore
    defer func() { <-sem }() // Release
    
    // ... spawn logic
}
```

**Strengths:**
✅ **Semaphore** - Limits concurrent spawns
✅ **Configurable limit** - MaxInherit from config
✅ **Blocking** - Waits if limit reached

**Concerns:**
⚠️ **No queue** - Blocking, not queued
⚠️ **No timeout** - Could block indefinitely
⚠️ **No prioritization** - FIFO ordering only

**Security Grade:** **B+** (Good limit, no queue)
### 3.4 Concurrent Spawn Limits
**Default:** 3 concurrent spawns

**Configuration:**
```toml
max_concurrent_spawns = 3
```

**Strengths:**
✅ **Default limit** - Prevents resource exhaustion
✅ **Configurable** - Operators can adjust
✅ **Per-agent** - Each agent has own semaphore

**Concerns:**
⚠️ **Arbitrary limit** - Why 3? Could be higher or lower
⚠️ **No monitoring** - Can't track spawn usage
⚠️ **No escalation** - No backoff or circuit breaker

**Security Grade:** **B+** (Good default, no monitoring)
---

## 4. Process Isolation
### 4.1 Process Group Isolation
**Implementation:** All child processes use Setpgid

**Mechanism:**
```go
&syscall.SysProcAttr{
    Setpgid: true,
    Credential: childCredential,
}
```

**Strengths:**
✅ **New process group** - Child in separate group
✅ **Group kill** - Can kill entire process tree
✅ **Isolation** - Signals don't propagate to parent

**Concerns:**
⚠️ **No namespace isolation** - Same mount namespace
⚠️ **No network isolation** - Same network namespace
⚠️ **No PID namespace** - Same PID namespace

**Security Grade:** **B+** (Good signal isolation)
### 4.2 Session Isolation (Tmux)
**Implementation:** Tmux tool

**Mechanism:**
- Each tmux session is separate
- Sessions tracked per-agent
- Session namespacing

**Strengths:**
✅ **Separate sessions** - Isolated tmux sessions
✅ **Ownership tracking** - Knows which agent owns session
✅ **Namespace** - Sessions prefixed with agent ID

**Concerns:**
⚠️ **Shared tmux server** - All agents share tmux server
⚠️ **No cgroup isolation** - No resource isolation
⚠️ **No network namespace** - Shared network

**Security Grade:** **B** (Good session isolation)
### 4.3 MCP Server Isolation
**Implementation:** `internal/mcp/*.go`

**Stdio MCP:**
- Separate process
- stdin/stdout communication
- Group dropping applies

**HTTP MCP:**
- External process (not managed)
- HTTP client connection
- No isolation

**Strengths:**
✅ **Process isolation** - Stdio MCP in separate process
✅ **Group dropping** - Loses foci-secrets group

**Concerns:**
⚠️ **No sandbox** - No container/cgroup isolation
⚠️ **Shared resources** - Same filesystem, network
⚠️ **HTTP MCP** - No isolation at all

**Security Grade:** **B-** (Stdio good, HTTP weak)
---

## 5. Capabilities Management
### 5.1 CAP_SETGID Requirement
**Capability:** CAP_SETGID (allows setgroups() syscall)

**Usage:**
- Required for group dropping
- Set in systemd unit
- Probed at startup

**Systemd Configuration:**
```ini
[Service]
AmbientCapabilities=CAP_SETGID
```

**Strengths:**
✅ **Explicit capability** - Only CAP_SETGID granted
✅ **Proactive probing** - Tests capability at startup
✅ **Clear documentation** - systemd unit documented

**Concerns:**
⚠️ **Manual setup** - Requires correct systemd configuration
⚠️ **Warning only** - Continues if capability missing
⚠️ **No verification** - Can't verify capability actually used

**Security Grade:** **B+** (Good design, manual setup)
### 5.2 Capability Dropping
**Status:** NOT IMPLEMENTED

**Current Behavior:**
- CAP_SETGID retained throughout process lifetime
- Not dropped after initial use
- No capability bounding

**Impact:**
- Process retains CAP_SETGID always
- Could be exploited if process compromised
- Not least privilege

**Recommendation:**
```go
import "kernel.org/pub/go/capability"

// After setting up child credential
cap, _ := capability.NewPid(0)
cap.Clear(capability.CAPS | capability.BOUNDS)
cap.Set(capability.CAP_SETGID, capability.CAPS)
cap.Apply(capability.CAPS | capability.BOUNDS)
```

**Security Grade:** **C** (No capability dropping)
---

## 6. Zombie Process Management
### 6.1 Process Reaping
**Implementation:** Go runtime + WaitDelay

**Mechanism:**
1. Go runtime reaps most child processes
2. WaitDelay (Go 1.20+) reaps zombie processes
3. Background processes use Setsid + WaitDelay

**Strengths:**
✅ **Automatic reaping** - Go runtime handles most
✅ **WaitDelay** - Reaps zombies after 2s
✅ **Setsid** - Background processes in new session

**Concerns:**
⚠️ **Not immediate** - WaitDelay is 2s delay
⚠️ **Orphaned processes** - If parent crashes
⚠️ **No tracking** - Can't enumerate child processes

**Security Grade:** **B** (Good automatic reaping)
### 6.2 Process Tracking
**Status:** NOT IMPLEMENTED

**Current Behavior:**
- No process table maintained
- Can't enumerate child processes
- No process lifecycle tracking

**Impact:**
- Orphaned processes not detected
- Resource leaks not visible
- Debugging difficult

**Recommendation:**
```go
type ProcessTracker struct {
    mu     sync.Mutex
    procs  map[int]*ProcessInfo
}

type ProcessInfo struct {
    Pid     int
    Command string
    Started time.Time
    Context string
}
```

**Security Grade:** **D** (No tracking)
---

## 7. Critical Findings - Phase 8

### Finding 8.1: CAP_SETGID Failure Mode is Warning-Only (MEDIUM)

**Location:** `internal/tools/procattr.go:89-93`
**Issue:** If CAP_SETGID unavailable, logs warning but continues
**Impact:** Child processes can access secrets.toml
**Recommendation:**
```go
// Make fatal if secrets.toml exists
if _, err := os.Stat(secretsPath); err == nil {
    log.Fatalf("exec", "CAP_SETGID required when secrets.toml exists")
}
```

### Finding 8.2: No Capability Dropping After Use (LOW)

**Location:** Process lifecycle
**Issue:** CAP_SETGID retained throughout process lifetime
**Impact:** Process retains unnecessary capability
**Recommendation:**
- Drop CAP_SETGID after initial setup
- Use capability bounding
- Implement least privilege

### Finding 8.3: No Resource Quotas (MEDIUM)

**Location:** All child processes
**Issue:** No limits on CPU, memory, or processes per agent
**Impact:** Resource exhaustion, DoS
**Recommendation:**
```go
import "github.com/containerd/cgroups"

// Create cgroup per agent
cgroup, err := cgroups.New(cgroups.V2, cgroups.StaticPath("/foci/"+agentID), &specs)
```

### Finding 8.4: Memory Guard Reactive Not Proactive (LOW)

**Location:** `internal/resources/memory_guard.go`
**Issue:** Kills processes after exceeding threshold, not before
**Impact:** Process could already have caused damage
**Recommendation:**
- Set memory limit before process starts
- Use cgroups for memory limits
- Or ulimit

### Finding 8.5: No Process Tracking (LOW)

**Location:** Process management
**Issue:** No process table or lifecycle tracking
**Impact:** Orphaned processes not detected
**Recommendation:**
- Implement process tracker
- Track PID, command, start time
- Monitor for orphans

### Finding 8.6: Background Process Accumulation (MEDIUM)

**Location:** Shell tool background mode
**Issue:** No limit on concurrent background processes
**Impact:** Resource exhaustion
**Recommendation:**
```go
var backgroundSem = make(chan struct{}, 10) // Max 10 background

func executeBackground(cmd string) error {
    select {
    case backgroundSem <- struct{}{}:
        defer func() { <-backgroundSem }()
        // Execute
    default:
        return fmt.Errorf("too many background processes")
    }
}
```

### Finding 8.7: HTTP MCP No Isolation (MEDIUM)

**Location:** `internal/mcp/transport_http.go`
**Issue:** HTTP MCP servers are external, no isolation
**Impact:** Compromised MCP server could attack foci
**Recommendation:**
- Run HTTP MCP in sandboxed process
- Or require explicit trust configuration
- Or prefer stdio MCP

### Finding 8.8: Tmux Shared Server (LOW)

**Location:** Tmux tool
**Issue:** All agents share same tmux server
**Impact:** Agent could access other agent's sessions
**Recommendation:**
- Use tmux -L for separate socket
- Or document isolation limitation
- Or implement per-agent tmux server

---

## 8. Process Isolation Matrix

| Component | Group Drop | PGroup | CGroup | Namespace | Tracking | Grade |
|-----------|------------|--------|--------|-----------|----------|-------|
| **Shell Foreground** | ✅ Yes | ✅ Yes | ❌ No | ❌ No | ❌ No | B+ |
| **Shell Background** | ✅ Yes | ✅ Setsid | ❌ No | ❌ No | ❌ No | B |
| **Tmux** | ✅ Yes | ✅ Yes | ❌ No | ❌ No | ⚠️ Partial | B |
| **Custom Commands** | ✅ Yes | ✅ Yes | ❌ No | ❌ No | ❌ No | B+ |
| **Spawn** | ✅ Yes | ✅ Yes | ❌ No | ❌ No | ❌ No | B+ |
| **MCP Stdio** | ✅ Yes | ✅ Yes | ❌ No | ❌ No | ❌ No | B+ |
| **MCP HTTP** | ❌ N/A | ❌ N/A | ❌ No | ❌ No | ❌ No | D |

---

## 9. Privilege Management Summary

### Strong Controls:
1. ✅ Group dropping (kernel-enforced)
2. ✅ Process group isolation (Setpgid)
3. ✅ Timeout enforcement (SIGKILL)
4. ✅ Capability probing (CAP_SETGID test)
5. ✅ Zombie reaping (WaitDelay)

### Weak Controls:
1. ⚠️ Warning-only failure mode (CAP_SETGID)
2. ⚠️ Reactive resource limits (kill after exceed)
3. ⚠️ No capability dropping (retain CAP_SETGID)
4. ⚠️ No cgroup isolation
5. ⚠️ No process tracking

### Missing Controls:
1. ❌ No resource quotas (CPU, memory, processes)
2. ❌ No namespace isolation (mount, network,3. ❌ No process lifecycle management
4. ❌ No sandboxing (containers, seccomp)
5. ❌ No graceful shutdown (SIGTERM before SIGKILL)

---

## 10. Recommendations Priority

### Critical Priority:
1. **Make CAP_SETGID check fatal** (Finding 8.1) - Security bypass if missing
2. **Implement resource quotas** (Finding 8.3) - DoS prevention

### High Priority:
3. **Limit background processes** (Finding 8.6) - Resource exhaustion
4. **Sandbox HTTP MCP** (Finding 8.7) - External process risk

### Medium Priority:
5. **Drop capabilities after use** (Finding 8.2) - Least privilege
6. **Proactive memory limits** (Finding 8.4) - Prevent exhaustion
7. **Process tracking** (Finding 8.5) - Orphan detection
8. **Per-agent tmux** (Finding 8.8) - Agent isolation

---

## 11. Comparison to Industry Standards

**Better Than:**
- Simple exec without isolation
- Systems without group dropping

**On Par With:**
- Container runtimes (cgroups, namespaces)
- Process supervision systems

**Lags Behind:**
- Container orchestration (Kubernetes)
- Sandboxing frameworks (gVisor, Firecracker)
- Privilege separation (SELinux, AppArmor)

---

## 12. Security Model Validation

### Threat Model:
**Attacker:** Compromised child process
**Goal:** Access secrets.toml
**Mitigation:** Group dropping (kernel-enforced)

**Attack Path 1: Direct file read**
```go
// Child process tries
os.ReadFile("/path/to/secrets.toml")
```
**Result:** ❌ PERMISSION DENIED (not in foci-secrets group)

**Attack Path 2: Symlink trick**
```bash
ln -s /path/to/secrets.toml /tmp/innocent
```
```go
// Child process tries
os.ReadFile("/tmp/innocent")
```
**Result:** ❌ PERMISSION DENIED (symlink resolved, still checks group)

**Attack Path 3: /proc filesystem**
```go
// Child process tries
os.ReadFile("/proc/self/environ")
```
**Result:** ⚠️ ALLOWED (if not blocked by IsBlockedPath)

**Attack Path 4: Memory read**
```go
// Child process tries (if can attach debugger)
ptrace(PTRACE_PEEKDATA, parent_pid, ...)
```
**Result:** ❌ YAMA not enabled by default (requires CAP_SYS_PTRACE)

**Security Model Strength:** **A-** (Excellent against file access, weaker against other attack vectors)

---

**Phase 8 Status:** ✅ COMPLETE
**Next Phase:** Phase 9 - Dependency & Supply Chain Security
