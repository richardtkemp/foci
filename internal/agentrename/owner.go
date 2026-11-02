package agentrename

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"foci/internal/config"
	"foci/internal/fociroot"
	"foci/internal/procx"
)

// Owner is the user a root run writes as (#2786): the user the gateway runs
// as, who owns the install root. A rename run as root writes foci.toml,
// secrets.toml and the crontab directly (no sudo approval, which on an aisudo
// host cannot arrive while the gateway is stopped), and gives every file and
// directory it creates or replaces the owner a run as this user would leave:
// a replaced file keeps the owner and group of the file it replaces, a new
// one gets UID and the group its directory hands out (the directory's group
// when it is setgid, else GID). Without this, the gateway could not write
// the databases, sessions and journal a root run rewrote.
type Owner struct {
	User     string // user name: whose crontab, and named in messages
	UID, GID int    // owner and group of the install root
}

// ownership applies Owner to the paths a run writes. The zero value (and a
// nil pointer) is a run as the install user itself: every method is a no-op.
type ownership struct {
	o *Owner
	// fake, in tests only, replaces os.Lchown and the owner read from a
	// stat: a test process cannot give files away, so it simulates them.
	fake ownerFS
}

// ownerFS is the test seam of ownership.
type ownerFS interface {
	chown(path string, uid, gid int) error
	ownerOf(fi fs.FileInfo) (uid, gid int, ok bool)
}

func newOwnership(o *Owner, fake ownerFS) *ownership {
	return &ownership{o: o, fake: fake}
}

func (w *ownership) chown(path string, uid, gid int) error {
	if w.fake != nil {
		return w.fake.chown(path, uid, gid)
	}
	return os.Lchown(path, uid, gid)
}

func (w *ownership) ownerOf(fi fs.FileInfo) (uid, gid int, ok bool) {
	if w.fake != nil {
		return w.fake.ownerOf(fi)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

func (w *ownership) on() bool { return w != nil && w.o != nil }

// keep gives path the owner and group of ref, the file it replaces.
func (w *ownership) keep(path string, ref fs.FileInfo) error {
	if !w.on() {
		return nil
	}
	uid, gid, ok := w.ownerOf(ref)
	if !ok {
		return fmt.Errorf("owner of %s: no stat data", path)
	}
	if err := w.chown(path, uid, gid); err != nil {
		return fmt.Errorf("give %s its original owner: %w", path, err)
	}
	return nil
}

// fresh gives a new path the install owner (see newGID for the group).
func (w *ownership) fresh(path string) error {
	if !w.on() {
		return nil
	}
	if err := w.chown(path, w.o.UID, w.newGID(filepath.Dir(path))); err != nil {
		return fmt.Errorf("give %s to %s: %w", path, w.o.User, err)
	}
	return nil
}

// newGID is the group of a new file in dir: dir's group when dir is setgid
// (what the kernel gives the gateway's own new files there), else the
// install root's group.
func (w *ownership) newGID(dir string) int {
	if pi, err := os.Stat(dir); err == nil && pi.Mode()&os.ModeSetgid != 0 {
		if _, gid, ok := w.ownerOf(pi); ok {
			return gid
		}
	}
	return w.o.GID
}

// like gives tmp (about to be renamed over dst) dst's owner when dst exists,
// else the owner of a new file in dst's directory.
func (w *ownership) like(tmp, dst string) error {
	if !w.on() {
		return nil
	}
	info, err := os.Lstat(dst)
	switch {
	case err == nil:
		return w.keep(tmp, info)
	case errors.Is(err, fs.ErrNotExist):
		if err := w.chown(tmp, w.o.UID, w.newGID(filepath.Dir(dst))); err != nil {
			return fmt.Errorf("give %s to %s: %w", dst, w.o.User, err)
		}
		return nil
	default:
		return err
	}
}

// mkdirAll is os.MkdirAll that gives every directory it creates to the
// install owner.
func (w *ownership) mkdirAll(dir string, perm os.FileMode) error {
	var created []string
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		created = append(created, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	for i := len(created) - 1; i >= 0; i-- { // parents first: fresh reads the parent's setgid bit
		if err := w.fresh(created[i]); err != nil {
			return err
		}
	}
	return nil
}

// sidecars gives a database's -wal, -shm and -journal files, when present,
// the owner and group of the database itself: SQLite creates a missing one
// as whoever opens the file, and a root-owned -wal locks the gateway out.
func (w *ownership) sidecars(db string) error {
	if !w.on() {
		return nil
	}
	ref, err := os.Stat(db)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, side := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(db + side); err != nil {
			continue
		}
		if err := w.keep(db+side, ref); err != nil {
			return err
		}
	}
	return nil
}

// checkDataDir refuses a root run whose install user does not own the data
// dir: the files would be handed to the wrong user.
func (w *ownership) checkDataDir(dataDir string) error {
	if !w.on() {
		return nil
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		return err
	}
	uid, _, ok := w.ownerOf(info)
	if !ok {
		return nil
	}
	if uid != w.o.UID {
		return fmt.Errorf("running as root for user %s (uid %d), but the data dir %s is owned by %s: pass --user %s; nothing was changed",
			w.o.User, w.o.UID, dataDir, userName(uid), userName(uid))
	}
	return nil
}

func userName(uid int) string {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return u.Username
	}
	return strconv.Itoa(uid)
}

// DirectRunner is the Privileged of a run that already is root: it runs the
// command itself, with no sudo and no approval.
type DirectRunner struct{}

// Run implements Privileged.
func (DirectRunner) Run(argv []string, stdin []byte) ([]byte, error) {
	cmd := procx.Spawn(context.Background(), procx.Operator, argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(string(stdin))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	return out, nil
}

// RootRun prepares a run of rename-agent as root (#2786). user names the
// install's user (the --user flag); empty means $SUDO_USER, the user who ran
// sudo. It points HOME, USER and LOGNAME at that user, so the install root,
// the default config and every path derived from them resolve as they would
// for a run as that user (FOCI_HOME, when set, still names the install root),
// and returns the Owner and the config path to use (configPath, or the
// default when empty). It refuses, naming what to pass, when the user is
// unknown, does not own the install root, or the config is not that user's.
func RootRun(name, configPath string) (*Owner, string, error) {
	if name == "" {
		// sudo run by root itself names root: not an install user.
		if name = os.Getenv("SUDO_USER"); name == "root" {
			name = ""
		}
	}
	if name == "" {
		return nil, "", errors.New("running as root: pass --user NAME, the user the gateway runs as (e.g. sudo foci rename-agent --user foci OLD NEW)")
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, "", fmt.Errorf("running as root: --user %s: %w", name, err)
	}
	for k, v := range map[string]string{"HOME": u.HomeDir, "USER": name, "LOGNAME": name} {
		if err := os.Setenv(k, v); err != nil {
			return nil, "", err
		}
	}
	root, err := fociroot.Dir()
	if err != nil {
		return nil, "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, "", fmt.Errorf("running as root for user %s: install root: %w", name, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, "", fmt.Errorf("running as root: cannot read the owner of %s", root)
	}
	if strconv.Itoa(int(st.Uid)) != u.Uid {
		return nil, "", fmt.Errorf("running as root for user %s, but the install root %s is owned by %s: pass --user NAME, the user the gateway runs as (or set %s to the install root)",
			name, root, userName(int(st.Uid)), fociroot.EnvVar)
	}
	if configPath == "" {
		if configPath, err = config.DefaultConfigPath(); err != nil {
			return nil, "", err
		}
		if _, err := os.Stat(configPath); err != nil {
			return nil, "", fmt.Errorf("running as root for user %s: no foci config at %s: pass --user NAME, the user the gateway runs as, or --config PATH (with %s=<install root> when the install root is not that user's home)",
				name, configPath, fociroot.EnvVar)
		}
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return nil, "", err
	}
	if rel, err := filepath.Rel(root, abs); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("running as root for user %s: the config %s is not under the install root %s: pass --user NAME, the user the gateway runs as (or set %s to the install root)",
			name, abs, root, fociroot.EnvVar)
	}
	if dir := filepath.Dir(abs); filepath.Base(dir) == "config" && filepath.Dir(dir) != filepath.Clean(root) {
		return nil, "", fmt.Errorf("running as root for user %s: the config %s is in %s, but the install root is %s: set %s=%s",
			name, abs, filepath.Dir(dir), root, fociroot.EnvVar, filepath.Dir(dir))
	}
	return &Owner{User: name, UID: int(st.Uid), GID: int(st.Gid)}, abs, nil
}

// writeFile writes a small backup file (0600) in one step, owned like any
// other file the run creates.
func (w *ownership) writeFile(path string, data []byte) error {
	return w.writeFileAtomic(path, 0o600, time.Time{}, func(out io.Writer) error {
		_, err := out.Write(data)
		return err
	})
}
