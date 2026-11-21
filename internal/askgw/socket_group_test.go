package askgw

import (
	"net"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"

	"foci/internal/question"
)

// startGroupServer starts a server at sockPath whose socket group is group,
// with the caller's UID allowed. Start must succeed.
func startGroupServer(t *testing.T, sockPath, group string) {
	t.Helper()
	srv, err := NewServer(ServerDeps{
		SocketPath:  sockPath,
		AllowedUIDs: []string{strconv.Itoa(os.Getuid())},
		Group:       group,
		Present: func(string, string, string, string, string, string, string, []question.Choice, func(string)) (string, PresentOutcome) {
			return "", Presented
		},
		CancelPrompt:   func(string, string) {},
		ResolveSession: func(string) (string, string) { return "a", "a/c" },
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server with group %q: %v (#3237: a group failure must not disable askgw)", group, err)
	}
	t.Cleanup(srv.Close)
}

// socketModeGID returns the socket's permission bits and group id.
func socketModeGID(t *testing.T, path string) (os.FileMode, uint32) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no Stat_t for %s", path)
	}
	return fi.Mode().Perm(), st.Gid
}

// foreignGroup returns an existing group the test process is not in, so a
// chown of a socket created at sockPath to it fails with EPERM for a non-root
// caller. It also skips the group of sockPath's directory: on BSD/macOS a new
// file inherits that group, so a chown to it is a no-op that succeeds
// (/private/tmp is root:wheel).
func foreignGroup(t *testing.T, sockPath string) string {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("root can chown to any group")
	}
	mine, err := os.Getgroups()
	if err != nil {
		t.Fatalf("getgroups: %v", err)
	}
	mine = append(mine, os.Getgid())
	if fi, err := os.Stat(filepath.Dir(sockPath)); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			mine = append(mine, int(st.Gid))
		}
	}
	for _, name := range []string{"root", "wheel", "daemon", "bin", "sys", "nogroup", "staff", "tty"} {
		g, err := user.LookupGroup(name)
		if err != nil {
			continue
		}
		gid, err := strconv.Atoi(g.Gid)
		if err != nil || slices.Contains(mine, gid) {
			continue
		}
		return name
	}
	t.Skip("no existing group outside this process's groups")
	return ""
}

// A group the gateway user is not in (chown EPERM) must not disable askgw:
// WARN naming the cause, and serve on an owner-only 0600 socket (#3237,
// Dick's ruling 2026-11-21).
func TestStart_GroupChownNotPermittedFallsBackTo0600(t *testing.T) {
	sock := shortSockPath(t)
	group := foreignGroup(t, sock)
	logs := captureLog(t)
	startGroupServer(t, sock, group)

	mode, _ := socketModeGID(t, sock)
	if mode != 0o600 {
		t.Errorf("socket mode = %o, want 600", mode)
	}
	waitLogLine(t, logs, "WARN", group, "not permitted", "0600")

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("owner cannot dial the 0600 socket: %v", err)
	}
	_ = conn.Close()
}

// A missing group gets the same treatment: WARN and 0600, not 0660 with the
// process's default group (#3237).
func TestStart_GroupNotFoundFallsBackTo0600(t *testing.T) {
	const group = "foci-askgw-no-such-group-3237"
	if _, err := user.LookupGroup(group); err == nil {
		t.Skipf("group %q unexpectedly exists", group)
	}
	logs := captureLog(t)
	sock := shortSockPath(t)
	startGroupServer(t, sock, group)

	mode, _ := socketModeGID(t, sock)
	if mode != 0o600 {
		t.Errorf("socket mode = %o, want 600", mode)
	}
	waitLogLine(t, logs, "WARN", group, "not found", "0600")
}

// The happy path is unchanged: a group the process is in is set on the
// socket, mode 0660.
func TestStart_GroupSetKeeps0660(t *testing.T) {
	g, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Skipf("primary group has no name: %v", err)
	}
	sock := shortSockPath(t)
	startGroupServer(t, sock, g.Name)

	mode, gid := socketModeGID(t, sock)
	if mode != 0o660 {
		t.Errorf("socket mode = %o, want 660", mode)
	}
	if strconv.Itoa(int(gid)) != g.Gid {
		t.Errorf("socket gid = %d, want %s", gid, g.Gid)
	}
}
