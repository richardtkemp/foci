package tempdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The root lock (#2482) answers one question at startup: "is any other living
// foci-gw using this temp root?". Several gateways can resolve the same root
// (every gateway without FOCI_TMPDIR lands on the shared /tmp/foci), and the
// startup wipe in CleanStale would otherwise delete a live gateway's exec
// bridge sockets, funcs files and other temp state.
//
// Protocol, on one file <root>/.gateway.lock with flock(2):
//   - every gateway holds a SHARED lock on it for its whole life;
//   - a gateway wipes the root only while it holds the EXCLUSIVE lock, which
//     it can get only when no other open file description holds any lock.
//
// claimRoot tries LOCK_EX|LOCK_NB. Granted means no other gateway is alive on
// the root: it wipes, then downgrades to LOCK_SH (on Linux the downgrade is
// atomic, so no third gateway can slip an exclusive lock in between). Refused
// means another gateway lives (or is mid-wipe): it wipes nothing and waits for
// LOCK_SH, which it can only get after a concurrent wiper has finished and
// downgraded, so a gateway never creates temp state that a concurrent start
// could still wipe.
//
// The kernel drops a flock when the last descriptor of the open file
// description closes, i.e. when the gateway exits, however it exits (SIGKILL
// included). Go opens files O_CLOEXEC, so backends and other children the
// gateway starts never inherit the descriptor and cannot keep a dead
// gateway's lock alive. flock, not fcntl/OFD record locks, because those need
// a writable descriptor for a write lock: on the group-shared /tmp/foci the
// lock file can belong to another uid with no group write bit, and flock works
// on a read-only descriptor.
//
// The wipe never removes the lock file itself (see cleanStaleRoot): removing
// and re-creating it would give later gateways a different inode to lock than
// the living ones hold. A dead gateway's lock file is simply reused.

// lockFileName is the root lock file, a direct child of the temp root.
const lockFileName = ".gateway.lock"

// lockFileMode lets other members of the root's group open the lock file
// read-only, which is all flock needs.
const lockFileMode = 0o664

// sharedLockWait bounds how long a skipping gateway waits for its shared
// lock. A concurrent wiper holds the exclusive lock only for the length of
// one wipe; the bound exists so that a stuck holder can never stop this
// gateway from starting.
var sharedLockWait = 30 * time.Second

// sharedLockPoll is the retry interval while waiting for the shared lock.
const sharedLockPoll = 50 * time.Millisecond

// processLocks keeps every root lock CleanStale took, for the life of the
// process. Holding the *os.File also stops its finalizer from closing the
// descriptor, which would silently release the lock.
var (
	processLocksMu sync.Mutex
	processLocks   []*os.File
)

func holdForProcessLife(f *os.File) {
	processLocksMu.Lock()
	defer processLocksMu.Unlock()
	processLocks = append(processLocks, f)
}

// claimRoot takes this process's lock on root and wipes the root when, and
// only when, no other living gateway holds it. It returns the lock file (nil
// when no lock could be taken), which the caller must keep open for as long
// as it uses the root: closing it is "this gateway has exited".
//
// The result's SkipReason is set whenever the wipe did not run; LockErr is set
// when the outcome came from a lock failure rather than from another
// gateway's presence (the fail-safe arm: unsure means do not wipe). A lock
// problem never returns an error to the caller: the gateway must start.
func claimRoot(root string) (*os.File, CleanStaleResult) {
	path := filepath.Join(root, lockFileName)
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE|syscall.O_NOFOLLOW, lockFileMode)
	if err != nil {
		return nil, lockFailure(path, "cannot open the lock file", err)
	}

	err = flock(f, unix.LOCK_EX|unix.LOCK_NB)
	switch {
	case err == nil:
		result := cleanStaleRoot(root)
		if dErr := flock(f, unix.LOCK_SH); dErr != nil {
			// Still holding the exclusive lock, which protects this gateway's
			// state just as well; other gateways will time out waiting for a
			// shared lock and start without one.
			result.LockErr = fmt.Errorf("downgrade %s to a shared lock: %w", path, dErr)
		}
		return f, result
	case errors.Is(err, unix.EWOULDBLOCK):
		if wErr := waitShared(f); wErr != nil {
			_ = f.Close()
			return nil, lockFailure(path, "another gateway holds the lock, and taking a shared lock failed", wErr)
		}
		return f, CleanStaleResult{SkipReason: fmt.Sprintf("another foci gateway is using %s (it holds %s)", root, path)}
	default:
		_ = f.Close()
		return nil, lockFailure(path, "cannot lock the lock file", err)
	}
}

// lockFailure is the fail-safe result: the wipe is skipped because this
// process cannot tell whether another gateway uses the root.
func lockFailure(path, what string, err error) CleanStaleResult {
	return CleanStaleResult{
		SkipReason: fmt.Sprintf("%s %s, so cannot tell whether another gateway uses this temp root: %v", what, path, err),
		LockErr:    err,
	}
}

// waitShared takes a shared lock on f, retrying while a concurrent claimant
// holds the exclusive lock for its wipe, up to sharedLockWait.
func waitShared(f *os.File) error {
	deadline := time.Now().Add(sharedLockWait)
	for {
		err := flock(f, unix.LOCK_SH|unix.LOCK_NB)
		if err == nil || !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still held exclusively after %s: %w", sharedLockWait, err)
		}
		time.Sleep(sharedLockPoll)
	}
}

// flock applies how to f, retrying a call interrupted by a signal.
func flock(f *os.File, how int) error {
	for {
		err := unix.Flock(int(f.Fd()), how)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
