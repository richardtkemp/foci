package tools

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"foci/internal/procx"
)

// A direct spawn run from a background shell call (#2685) shows in the app
// twice: as a subagent (the SpawnObserver) and, through the backend's own
// tracker, as the "background command" of the shell call it runs in. The app
// hides that command entry while the spawn runs. This file links the two: the
// exec-bridge call that started the spawn came from a process (its peer pid),
// and the shell call is one of that process's ancestors, whose command line
// holds the command the backend tracked.

type callerPIDKey struct{}

// WithCallerPID records the pid of the process that made an exec-bridge call
// (the socket peer; ExecBridge.handleConn).
func WithCallerPID(ctx context.Context, pid int) context.Context {
	if pid <= 0 {
		return ctx
	}
	return context.WithValue(ctx, callerPIDKey{}, pid)
}

// CallerPIDFromContext returns the exec-bridge caller's pid, or 0.
func CallerPIDFromContext(ctx context.Context) int {
	pid, _ := ctx.Value(callerPIDKey{}).(int)
	return pid
}

// maxCallerAncestors bounds the walk up from an exec-bridge caller.
const maxCallerAncestors = 32

// callerShell returns the command lines of the processes above pid, joined by
// newlines and with quote characters removed (normalizeShellText), stopping
// below the process this gateway started (its child is the backend, e.g.
// Claude Code, whose own command line is not a shell call's). pid itself is
// left out: it is the bridge client, whose arguments hold the tool's
// parameters (a spawn's prompt), not the shell call. "" when nothing is
// readable (the caller gone).
func callerShell(pid int) string {
	if pid <= 0 {
		return ""
	}
	self := os.Getpid()
	var lines []string
	cur, _, ok := procInfo(pid)
	for i := 0; ok && i < maxCallerAncestors && cur > 1 && cur != self; i++ {
		ppid, line, pok := procInfo(cur)
		if !pok || ppid == self {
			// cur is the backend the gateway started (or unreadable): stop.
			break
		}
		if line != "" {
			lines = append(lines, line)
		}
		cur = ppid
	}
	return normalizeShellText(strings.Join(lines, "\n"))
}

// procInfo returns pid's parent and command line: from /proc where there is
// one (Linux), else from ps (macOS).
func procInfo(pid int) (ppid int, cmdline string, ok bool) {
	if _, err := os.Stat("/proc/self/stat"); err == nil {
		return procfsInfo(pid)
	}
	return psInfo(pid)
}

// procfsInfo reads /proc/<pid>/stat and /proc/<pid>/cmdline.
func procfsInfo(pid int) (int, string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, "", false
	}
	// "pid (comm) state ppid ...": comm may hold spaces and parentheses, so
	// read past the LAST ')'.
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, "", false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0, "", false
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, "", false
	}
	cmd, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return ppid, strings.TrimSpace(strings.ReplaceAll(string(cmd), "\x00", " ")), true
}

// psInfo asks ps for pid's parent and full (-ww: unwidthed) command line.
func psInfo(pid int) (int, string, bool) {
	out, err := procx.Spawn(context.Background(), procx.Trusted, "ps", "-ww", "-o", "ppid=", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, "", false
	}
	line := strings.TrimSpace(string(out))
	ppidText, args, _ := strings.Cut(line, " ")
	ppid, err := strconv.Atoi(ppidText)
	if err != nil {
		return 0, "", false
	}
	return ppid, strings.TrimSpace(args), true
}

// normalizeShellText removes every quote character. A backend runs a shell
// call's command inside a quoted string (Claude Code: eval '<command>', each
// ' written as '"'"'), so the command and the running process's command line
// agree only once quotes are gone.
func normalizeShellText(s string) string {
	return strings.NewReplacer("'", "", `"`, "").Replace(s)
}

// truncatedCommandSuffix ends a tracked command cut at its display limit
// (toolformat.FullCommand); the part before it is a prefix of the command.
const truncatedCommandSuffix = "\n…"

// ShellCallMatch reports how well a tracked shell command matches a direct
// spawn's CallerShell (RunningSpawn.CallerShell): 0 when it is not the shell
// call the spawn runs in, else the length of the matched command, so a caller
// choosing among several candidates can take the longest (the most specific:
// "foci_spawn x" is contained in "foci_spawn x; make land" too).
func ShellCallMatch(command, callerShell string) int {
	if callerShell == "" {
		return 0
	}
	c := normalizeShellText(strings.TrimSuffix(command, truncatedCommandSuffix))
	c = strings.TrimSpace(c)
	if c == "" || !strings.Contains(callerShell, c) {
		return 0
	}
	return len(c)
}
