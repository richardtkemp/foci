package tempdir

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// holderEnv, when set in a re-exec of this test binary, turns the process
// into a stand-in gateway: it claims the root named by the variable, reports
// "ready" on stdout, and holds the lock until stdin closes or it is killed.
// holderChildEnv additionally makes it start a long-lived child process first
// (standing in for a delegated backend that outlives a crashed gateway).
const (
	holderEnv      = "FOCI_TEMPDIR_TEST_HOLDER_ROOT"
	holderChildEnv = "FOCI_TEMPDIR_TEST_HOLDER_CHILD"
)

func TestMain(m *testing.M) {
	if root := os.Getenv(holderEnv); root != "" {
		os.Exit(runHolder(root))
	}
	os.Exit(m.Run())
}

// runHolder is the body of the stand-in gateway process (see holderEnv).
func runHolder(root string) int {
	lock, r := claimRoot(root)
	if lock == nil {
		fmt.Printf("claim failed: %+v\n", r)
		return 1
	}
	childPID := 0
	if os.Getenv(holderChildEnv) != "" {
		child := exec.Command("sleep", "300")
		if err := child.Start(); err != nil {
			fmt.Printf("start child: %v\n", err)
			return 1
		}
		childPID = child.Process.Pid
	}
	fmt.Printf("ready %d\n", childPID)
	_, _ = io.Copy(io.Discard, os.Stdin) // hold until the parent closes stdin
	_ = lock.Close()
	return 0
}

// holderProc is a stand-in gateway running in a separate OS process.
type holderProc struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	childPID int
}

// startHolder starts a separate process that claims root and holds its lock,
// and returns once the claim is done. withChild makes it also start a
// long-lived child before reporting ready.
func startHolder(t *testing.T, root string, withChild bool) *holderProc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), holderEnv+"="+root)
	if withChild {
		cmd.Env = append(cmd.Env, holderChildEnv+"=1")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &holderProc{cmd: cmd, stdin: stdin}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if h.childPID > 0 {
			_ = syscall.Kill(h.childPID, syscall.SIGKILL)
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("holder did not report ready: %v (got %q)", err, line)
	}
	fields := strings.Fields(line)
	if len(fields) != 2 || fields[0] != "ready" {
		t.Fatalf("holder: %q", line)
	}
	h.childPID, _ = strconv.Atoi(fields[1])
	return h
}

// kill SIGKILLs the holder (a crashed gateway) and waits for it to be gone.
func (h *holderProc) kill(t *testing.T) {
	t.Helper()
	if err := h.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = h.cmd.Wait()
}

// plantBridge plants a pid-named exec-bridge socket/funcs pair (plain files
// stand in for the socket) and a file with no pid in its name, and returns
// their paths.
func plantBridge(t *testing.T, root string) []string {
	t.Helper()
	paths := []string{
		filepath.Join(root, "exec-x-99999-1.sock"),
		filepath.Join(root, "exec-x-99999-1-funcs.sh"),
		filepath.Join(root, "session-env-probe"),
	}
	for _, p := range paths {
		mustWriteFile(t, p, "live state")
	}
	return paths
}

func assertExist(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s should survive: %v", filepath.Base(p), err)
		}
	}
}

func assertGone(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s should have been wiped, stat err = %v", filepath.Base(p), err)
		}
	}
}

// claimOrFail claims root in this process, failing the test when no lock was
// taken, and closes the lock (a gateway exit) at cleanup.
func claimOrFail(t *testing.T, root string) (*os.File, CleanStaleResult) {
	t.Helper()
	lock, r := claimRoot(root)
	if lock == nil {
		t.Fatalf("claimRoot(%s) took no lock: %+v", root, r)
	}
	t.Cleanup(func() { _ = lock.Close() })
	return lock, r
}

// TestClaimRoot_HeldByAnotherProcessIsNotWiped proves requirement 1 across
// real processes: while a separate OS process holds the root, a claim removes
// nothing (a pid-named bridge pair, a file with no pid, and the holder's lock
// file all survive), reports why it skipped, and still takes its own lock.
func TestClaimRoot_HeldByAnotherProcessIsNotWiped(t *testing.T) {
	root := t.TempDir()
	startHolder(t, root, false)
	planted := plantBridge(t, root)

	_, r := claimOrFail(t, root)

	if r.SkipReason == "" || r.LockErr != nil {
		t.Errorf("want a skip with no lock error, got %+v", r)
	}
	if r.Removed != 0 || r.Failed != 0 {
		t.Errorf("a skipped claim touched entries: %+v", r)
	}
	assertExist(t, planted...)
	assertExist(t, filepath.Join(root, lockFileName))
}

// TestClaimRoot_DeadHolderDoesNotBlockWipe proves requirement 3: a holder
// killed with SIGKILL leaves its lock file behind, yet the next claim wipes
// the orphaned state, keeps tool-results/, and reuses the lock file.
func TestClaimRoot_DeadHolderDoesNotBlockWipe(t *testing.T) {
	root := t.TempDir()
	h := startHolder(t, root, false)
	h.kill(t)
	planted := plantBridge(t, root)
	kept := filepath.Join(root, toolResultsDir, "tool-result-x.txt")
	mustWriteFile(t, kept, "keep")

	_, r := claimOrFail(t, root)

	if r.SkipReason != "" || r.LockErr != nil {
		t.Fatalf("a dead holder blocked the wipe: %+v", r)
	}
	if r.Removed < len(planted) {
		t.Errorf("Removed = %d, want >= %d", r.Removed, len(planted))
	}
	assertGone(t, planted...)
	assertExist(t, kept, filepath.Join(root, lockFileName))
}

// TestClaimRoot_DeadHolderWithLiveChildDoesNotBlockWipe proves requirement 3
// when the dead gateway left a child running (a delegated backend): the lock
// descriptor is close-on-exec, so the child does not keep the lock alive.
func TestClaimRoot_DeadHolderWithLiveChildDoesNotBlockWipe(t *testing.T) {
	root := t.TempDir()
	h := startHolder(t, root, true)
	if h.childPID == 0 {
		t.Fatal("holder reported no child")
	}
	h.kill(t)
	if err := syscall.Kill(h.childPID, 0); err != nil {
		t.Fatalf("the holder's child should still be alive: %v", err)
	}
	planted := plantBridge(t, root)

	_, r := claimOrFail(t, root)

	if r.SkipReason != "" {
		t.Fatalf("a dead holder's surviving child blocked the wipe: %+v", r)
	}
	assertGone(t, planted...)
}

// TestClaimRoot_EveryLivingHolderCounts proves requirement 1's "every living
// gateway": A claims, B claims (skips), A exits, and C still skips because B
// lives.
func TestClaimRoot_EveryLivingHolderCounts(t *testing.T) {
	root := t.TempDir()
	a, _ := claimOrFail(t, root)
	_, rb := claimOrFail(t, root)
	if rb.SkipReason == "" {
		t.Fatalf("B should skip while A lives: %+v", rb)
	}
	_ = a.Close() // A exits
	planted := filepath.Join(root, "exec-y-99998-1.sock")
	mustWriteFile(t, planted, "B's state")

	_, rc := claimOrFail(t, root)

	if rc.SkipReason == "" {
		t.Errorf("C wiped while B lives: %+v", rc)
	}
	assertExist(t, planted)
}

// TestClaimRoot_RestartWipesPredecessor proves requirement 4: a gateway that
// exits cleanly releases the root, and its successor wipes the leftovers.
// Characterisation: the root is unlocked by the time of the second claim.
func TestClaimRoot_RestartWipesPredecessor(t *testing.T) {
	root := t.TempDir()
	first, _ := claimOrFail(t, root)
	planted := plantBridge(t, root)
	_ = first.Close()

	_, r := claimOrFail(t, root)

	if r.SkipReason != "" {
		t.Fatalf("restart skipped the wipe: %+v", r)
	}
	assertGone(t, planted...)
}

// TestClaimRoot_UnlockedRootWipedAsBefore is the characterisation case: with
// no other gateway, a claim wipes exactly what cleanStaleRoot always did.
func TestClaimRoot_UnlockedRootWipedAsBefore(t *testing.T) {
	root := t.TempDir()
	planted := plantBridge(t, root)
	kept := filepath.Join(root, toolResultsDir, "tool-result-x.txt")
	mustWriteFile(t, kept, "keep")

	_, r := claimOrFail(t, root)

	if r.SkipReason != "" || r.Removed != len(planted) || r.Failed != 0 {
		t.Errorf("got %+v, want a wipe of %d entries", r, len(planted))
	}
	assertGone(t, planted...)
	assertExist(t, kept)
}

// TestClaimRoot_ConcurrentStartsOneWipeNoLostState proves requirement 5:
// several gateways claiming one root at the same moment produce exactly one
// wipe, and no gateway loses the state it creates after its claim returns.
func TestClaimRoot_ConcurrentStartsOneWipeNoLostState(t *testing.T) {
	const n = 8
	for round := 0; round < 20; round++ {
		root := t.TempDir()
		plantBridge(t, root)
		var (
			wg     sync.WaitGroup
			start  = make(chan struct{})
			mu     sync.Mutex
			wipes  int
			states []string
		)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				lock, r := claimRoot(root)
				if lock == nil {
					t.Errorf("claim %d took no lock: %+v", i, r)
					return
				}
				t.Cleanup(func() { _ = lock.Close() })
				state := filepath.Join(root, fmt.Sprintf("exec-g%d-%d-1.sock", i, round))
				if err := os.WriteFile(state, []byte("mine"), 0o600); err != nil {
					t.Errorf("write state: %v", err)
				}
				mu.Lock()
				defer mu.Unlock()
				if r.SkipReason == "" {
					wipes++
				}
				states = append(states, state)
			}(i)
		}
		close(start)
		wg.Wait()
		if wipes != 1 {
			t.Fatalf("round %d: %d wipes, want exactly 1", round, wipes)
		}
		assertExist(t, states...)
	}
}

// TestClaimRoot_SeparateRootsAreIndependent proves requirement 6: a holder on
// one root neither blocks nor is touched by a claim on another root.
func TestClaimRoot_SeparateRootsAreIndependent(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	startHolder(t, rootA, false)
	plantedA := plantBridge(t, rootA)
	plantedB := plantBridge(t, rootB)

	_, r := claimOrFail(t, rootB)

	if r.SkipReason != "" {
		t.Errorf("a holder on another root blocked the wipe: %+v", r)
	}
	assertGone(t, plantedB...)
	assertExist(t, plantedA...)
}

// TestClaimRoot_UnopenableLockFileSkips proves requirement 7: a lock file
// this process cannot open (mode 000, like another uid's unreadable file)
// means "cannot tell", so the wipe is skipped with the reason and error, and
// claimRoot returns normally.
func TestClaimRoot_UnopenableLockFileSkips(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a mode-000 file")
	}
	root := t.TempDir()
	lockPath := filepath.Join(root, lockFileName)
	mustWriteFile(t, lockPath, "")
	if err := os.Chmod(lockPath, 0); err != nil {
		t.Fatal(err)
	}
	planted := plantBridge(t, root)

	lock, r := claimRoot(root)

	if lock != nil {
		_ = lock.Close()
		t.Error("claimRoot returned a lock it could not have taken")
	}
	if r.SkipReason == "" || r.LockErr == nil || !os.IsPermission(r.LockErr) {
		t.Errorf("want a fail-safe skip with a permission error, got %+v", r)
	}
	if !strings.Contains(r.SkipReason, lockPath) {
		t.Errorf("skip reason %q should name the lock file", r.SkipReason)
	}
	assertExist(t, planted...)
}

// TestClaimRoot_SymlinkedLockFileSkips proves the fail-safe for a lock path
// that is a symlink (planted by another group member): it is not followed,
// and the wipe is skipped.
func TestClaimRoot_SymlinkedLockFileSkips(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(target, filepath.Join(root, lockFileName)); err != nil {
		t.Fatal(err)
	}
	planted := plantBridge(t, root)

	lock, r := claimRoot(root)

	if lock != nil {
		_ = lock.Close()
	}
	if r.SkipReason == "" || r.LockErr == nil {
		t.Errorf("want a fail-safe skip, got %+v", r)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Errorf("the symlink was followed and created %s", target)
	}
	assertExist(t, planted...)
}

// TestClaimRoot_SharedWaitTimesOutAndSkips proves the bounded wait: when
// another descriptor keeps the exclusive lock (a stuck wiper), a claim gives
// up after sharedLockWait, skips the wipe, reports a lock error and returns.
func TestClaimRoot_SharedWaitTimesOutAndSkips(t *testing.T) {
	root := t.TempDir()
	stuck, err := os.OpenFile(filepath.Join(root, lockFileName), os.O_RDONLY|os.O_CREATE, lockFileMode)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stuck.Close() })
	if err := flock(stuck, syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	planted := plantBridge(t, root)
	restore := setSharedLockWaitForTest(10 * time.Millisecond)
	t.Cleanup(restore)

	lock, r := claimRoot(root)

	if lock != nil {
		_ = lock.Close()
		t.Error("claimRoot returned a lock after the wait failed")
	}
	if r.SkipReason == "" || r.LockErr == nil {
		t.Errorf("want a fail-safe skip, got %+v", r)
	}
	assertExist(t, planted...)
}

// TestCleanStaleRoot_KeepsLockFile proves the wipe never removes the lock
// file, so a living gateway's lock inode is never replaced under it.
func TestCleanStaleRoot_KeepsLockFile(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, lockFileName)
	mustWriteFile(t, lockPath, "")
	mustWriteFile(t, filepath.Join(root, "exec-1-funcs.sh"), "stale")

	r := cleanStaleRoot(root)

	if r.Removed != 1 {
		t.Errorf("Removed = %d, want 1", r.Removed)
	}
	assertExist(t, lockPath)
}

// TestCleanStale_HoldsRootLockForProcessLife proves CleanStale keeps its lock
// after returning: a later claim on the same resolved root skips the wipe.
func TestCleanStale_HoldsRootLockForProcessLife(t *testing.T) {
	origOverride, hadOverride := os.LookupEnv(EnvOverride)
	t.Cleanup(func() {
		if hadOverride {
			_ = os.Setenv(EnvOverride, origOverride)
		} else {
			_ = os.Unsetenv(EnvOverride)
		}
		resetResolveForTest()
	})
	scratch := t.TempDir()
	if err := os.Setenv(EnvOverride, scratch); err != nil {
		t.Fatal(err)
	}
	resetResolveForTest()

	if r := CleanStale(); r.SkipReason != "" {
		t.Fatalf("first CleanStale skipped: %+v", r)
	}
	planted := plantBridge(t, scratch)

	_, r := claimOrFail(t, scratch)

	if r.SkipReason == "" {
		t.Errorf("CleanStale's lock was not held: a later claim wiped (%+v)", r)
	}
	assertExist(t, planted...)
}

// setSharedLockWaitForTest shortens the shared-lock wait and returns a func
// restoring it.
func setSharedLockWaitForTest(d time.Duration) func() {
	orig := sharedLockWait
	sharedLockWait = d
	return func() { sharedLockWait = orig }
}
