package agentrename

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// node is one path's identity and owner.
type node struct {
	ino      uint64
	uid, gid int
}

// inodes maps every path under root (files and directories) to its node,
// with the owners rec simulates.
func inodes(t *testing.T, root string, rec *chownRecorder) map[string]node {
	t.Helper()
	out := map[string]node{}
	must(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		uid, gid, _ := rec.ownerOf(fi)
		out[p] = node{ino: ino(fi), uid: uid, gid: gid}
		return nil
	}))
	return out
}

func ino(fi fs.FileInfo) uint64 { return uint64(fi.Sys().(*syscall.Stat_t).Ino) }

// chownRecorder simulates ownership for a root run in a test (ownerFS): the
// test process cannot give files away, so chown records the owner per inode
// (byIno, the simulated filesystem) and ownerOf reads it back, falling back
// to the real owner. asked holds the inodes chowned in the current phase.
type chownRecorder struct {
	byIno map[uint64][2]int
	asked map[uint64]bool
}

func newRecorder() *chownRecorder {
	return &chownRecorder{byIno: map[uint64][2]int{}, asked: map[uint64]bool{}}
}

func (r *chownRecorder) chown(p string, uid, gid int) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	r.byIno[ino(fi)] = [2]int{uid, gid}
	r.asked[ino(fi)] = true
	return nil
}

func (r *chownRecorder) ownerOf(fi fs.FileInfo) (int, int, bool) {
	if o, ok := r.byIno[ino(fi)]; ok {
		return o[0], o[1], true
	}
	st := fi.Sys().(*syscall.Stat_t)
	return int(st.Uid), int(st.Gid), true
}

// assertOwned checks the root-run property: every path that is a NEW inode
// after the run (a file the run created, or replaced by temp-and-rename)
// was given an owner — the owner of the file it replaced (found at the same
// path before, or at origOf(path)), or for a brand-new path the install
// owner with the group its directory hands out. A path whose inode existed
// before (untouched, renamed, or rewritten in place) keeps its owner by
// itself. As root, a new inode with no chown would be left root-owned.
func assertOwned(t *testing.T, before, after map[string]node, rec *chownRecorder, owner Owner, origOf func(string) string) {
	t.Helper()
	old := map[uint64]bool{}
	for _, n := range before {
		old[n.ino] = true
	}
	checked := 0
	for p, n := range after {
		if old[n.ino] {
			continue
		}
		checked++
		got := [2]int{n.uid, n.gid}
		if !rec.asked[n.ino] {
			t.Errorf("%s: written by the run but never given an owner (a root run leaves it root-owned)", p)
			continue
		}
		orig, existed := before[p]
		if !existed {
			orig, existed = before[origOf(p)]
		}
		want := [2]int{orig.uid, orig.gid}
		if !existed {
			want = [2]int{owner.UID, owner.GID}
			if pi, err := os.Stat(filepath.Dir(p)); err == nil && pi.Mode()&os.ModeSetgid != 0 {
				_, want[1], _ = rec.ownerOf(pi)
			}
		}
		if got != want {
			t.Errorf("%s: given owner %v, want %v (replaced an existing file: %v)", p, got, want, existed)
		}
	}
	if checked == 0 {
		t.Fatal("no new inode after the run: the check saw nothing (instrument broken)")
	}
}

// testOwner is the simulated install owner: this process's uid (so the data
// dir check passes) and a group no fixture file has, so a new file's
// "install owner" and a replaced file's "original owner" differ.
func testOwner() Owner {
	gid := os.Getgid() + 1000
	return Owner{User: "foci", UID: os.Getuid(), GID: gid}
}

// TestRootRunGivesEveryWrittenFileItsOwner: #2786. A rename and its restore,
// run as root, leave no file or directory they created or replaced without
// its owner: replaced files keep the original's owner and group, new ones go
// to the install owner. Fail-arm: drop any one chown site (e.g. the keep in
// sessionsStep, or like() in writeFileAtomic) and this test names the file.
func TestRootRunGivesEveryWrittenFileItsOwner(t *testing.T) {
	for _, move := range []bool{false, true} {
		name := "keep"
		if move {
			name = "move"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			owner := testOwner()
			oldWS, newWS := filepath.Join(f.root, "bob"), filepath.Join(f.root, "rob")
			origOf := func(p string) string {
				p = strings.Replace(p, filepath.Join(f.sessions, "rob")+"/", filepath.Join(f.sessions, "bob")+"/", 1)
				if move {
					p = strings.Replace(p, newWS+"/", oldWS+"/", 1)
				}
				return p
			}
			rec := newRecorder()
			before := inodes(t, f.root, rec)
			out, err := f.run(Options{Move: move, Owner: &owner, fakeOwners: rec})
			if err != nil {
				t.Fatalf("rename: %v\n%s", err, out)
			}
			if !strings.Contains(out, "running as root for user foci") {
				t.Errorf("plan does not say it runs as root:\n%s", out)
			}
			after := inodes(t, f.root, rec)
			assertOwned(t, before, after, rec, owner, origOf)

			// The restore runs on the same simulated filesystem.
			backup := filepath.Join(f.data, "backups", "rename-bob-rob-20261101T120000Z")
			rec.asked = map[uint64]bool{}
			var rout bytes.Buffer
			must(t, Restore(context.Background(), RestoreOptions{ConfigPath: f.cfgPath, BackupDir: backup, Yes: true,
				Out: &rout, Priv: f.priv, Owner: &owner, fakeOwners: rec}))
			if !strings.Contains(rout.String(), "sudo crontab -u foci ") {
				t.Errorf("restore does not name the install user's crontab:\n%s", rout.String())
			}
			// Restored paths were at their renamed path in the "after" snapshot.
			newOf := func(p string) string {
				p = strings.Replace(p, filepath.Join(f.sessions, "bob")+"/", filepath.Join(f.sessions, "rob")+"/", 1)
				if move {
					p = strings.Replace(p, oldWS+"/", newWS+"/", 1)
				}
				return p
			}
			assertOwned(t, after, inodes(t, f.root, rec), rec, owner, newOf)
		})
	}
}

// TestRootRunRefusesAnotherUsersInstall: a root run for a user who does not
// own the data dir changes nothing.
func TestRootRunRefusesAnotherUsersInstall(t *testing.T) {
	f := newFixture(t)
	skip := []string{"config/secrets.toml"}
	before := hashTree(t, f.root, skip...)
	owner := Owner{User: "someone", UID: os.Getuid() + 1, GID: os.Getgid()}
	rec := newRecorder()
	out, err := f.run(Options{Owner: &owner, fakeOwners: rec})
	if err == nil || !strings.Contains(err.Error(), "pass --user") {
		t.Fatalf("err = %v, want a refusal naming --user\n%s", err, out)
	}
	diffHashes(t, before, hashTree(t, f.root, skip...))
	if len(rec.byIno) != 0 {
		t.Errorf("chowned %d path(s) before refusing", len(rec.byIno))
	}
}

// TestRootRunResolvesTheInstall: RootRun finds the install root and config
// of the named user, and refuses — naming what to pass — when it cannot.
func TestRootRunResolvesTheInstall(t *testing.T) {
	me, err := user.Current()
	must(t, err)
	// RootRun sets these: t.Setenv restores them after the test.
	for _, k := range []string{"HOME", "USER", "LOGNAME", "SUDO_USER"} {
		t.Setenv(k, os.Getenv(k))
	}

	t.Run("no user", func(t *testing.T) {
		newFixture(t)
		t.Setenv("SUDO_USER", "")
		if _, _, err := RootRun("", ""); err == nil || !strings.Contains(err.Error(), "pass --user NAME") {
			t.Fatalf("err = %v, want a refusal naming --user", err)
		}
		t.Setenv("SUDO_USER", "root")
		if _, _, err := RootRun("", ""); err == nil || !strings.Contains(err.Error(), "pass --user NAME") {
			t.Fatalf("SUDO_USER=root: err = %v, want a refusal naming --user", err)
		}
	})
	t.Run("sudo user", func(t *testing.T) {
		f := newFixture(t) // FOCI_HOME = the fixture root, owned by this user
		t.Setenv("SUDO_USER", me.Username)
		o, cfg, err := RootRun("", "")
		if err != nil {
			t.Fatal(err)
		}
		if o.User != me.Username || o.UID != os.Getuid() {
			t.Errorf("owner = %+v, want %s uid %d", o, me.Username, os.Getuid())
		}
		if cfg != f.cfgPath {
			t.Errorf("config = %s, want %s", cfg, f.cfgPath)
		}
		if os.Getenv("HOME") != me.HomeDir || os.Getenv("USER") != me.Username {
			t.Errorf("HOME=%s USER=%s, want %s %s", os.Getenv("HOME"), os.Getenv("USER"), me.HomeDir, me.Username)
		}
	})
	t.Run("config outside the install", func(t *testing.T) {
		newFixture(t)
		other := filepath.Join(t.TempDir(), "foci.toml")
		writeFile(t, other, []byte(""))
		if _, _, err := RootRun(me.Username, other); err == nil || !strings.Contains(err.Error(), "not under the install root") {
			t.Fatalf("err = %v, want a refusal: config not under the install root", err)
		}
	})
	t.Run("config of another install root", func(t *testing.T) {
		f := newFixture(t)
		other := filepath.Join(f.root, "elsewhere", "config", "foci.toml")
		writeFile(t, other, []byte(""))
		if _, _, err := RootRun(me.Username, other); err == nil || !strings.Contains(err.Error(), "set FOCI_HOME="+filepath.Join(f.root, "elsewhere")) {
			t.Fatalf("err = %v, want a refusal naming FOCI_HOME", err)
		}
	})
	t.Run("no config", func(t *testing.T) {
		f := newFixture(t)
		must(t, os.Rename(f.cfgPath, f.cfgPath+".gone"))
		if _, _, err := RootRun(me.Username, ""); err == nil || !strings.Contains(err.Error(), "--config PATH") {
			t.Fatalf("err = %v, want a refusal naming --user / --config", err)
		}
	})
	t.Run("unknown user", func(t *testing.T) {
		newFixture(t)
		if _, _, err := RootRun("no-such-user-2786", ""); err == nil || !strings.Contains(err.Error(), "--user no-such-user-2786") {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestRootRunSidecarsFollowTheDatabase: a -wal/-shm/-journal left beside a
// database gets the database's owner, and absent ones are skipped.
func TestRootRunSidecarsFollowTheDatabase(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	writeFile(t, db, []byte("db"))
	writeFile(t, db+"-wal", []byte("wal"))
	rec := newRecorder()
	dbInfo, err := os.Lstat(db)
	must(t, err)
	rec.byIno[ino(dbInfo)] = [2]int{4242, 4343} // the database's (simulated) owner
	owner := testOwner()
	own := newOwnership(&owner, rec)
	must(t, own.sidecars(db))
	walInfo, err := os.Lstat(db + "-wal")
	must(t, err)
	if got := rec.byIno[ino(walInfo)]; got != [2]int{4242, 4343} {
		t.Errorf("-wal owner = %v, want the database's [4242 4343]", got)
	}
	if len(rec.asked) != 1 {
		t.Errorf("chowned %d path(s), want only the -wal", len(rec.asked))
	}
	// Not a root run: nothing is chowned.
	rec2 := newRecorder()
	must(t, newOwnership(nil, rec2).sidecars(db))
	if len(rec2.asked) != 0 {
		t.Errorf("a run as the install user chowned %d path(s)", len(rec2.asked))
	}
}

// TestDirectRunnerRunsWithoutSudo: a root run's privileged command runs
// itself, stdin fed and stdout returned, and a failure is an error.
func TestDirectRunnerRunsWithoutSudo(t *testing.T) {
	out, err := DirectRunner{}.Run([]string{"cat"}, []byte("hello"))
	if err != nil || string(out) != "hello" {
		t.Fatalf("cat: out %q, err %v", out, err)
	}
	if _, err := (DirectRunner{}).Run([]string{"cat", filepath.Join(t.TempDir(), "missing")}, nil); err == nil {
		t.Fatal("a failing command returned no error")
	}
}
