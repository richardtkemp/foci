package peercred

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// dialedPair returns the server side of a loopback Unix socket connection made
// within this process. Both ends therefore belong to the current user, which is
// what lets us exercise the SO_PEERCRED plumbing without a second account.
func dialedPair(t *testing.T) *net.UnixConn {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "p.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan *net.UnixConn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c.(*net.UnixConn)
	}()

	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	srv := <-accepted
	if srv == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// Proves UID() reads the real peer UID over a Unix socket and that MatchesSelf
// accepts a same-user peer — i.e. the SO_PEERCRED syscall works on this platform
// and legitimate same-user connections are not falsely rejected.
func TestUIDAndMatchesSelf(t *testing.T) {
	srv := dialedPair(t)

	uid, err := UID(srv)
	if err != nil {
		t.Fatalf("UID: %v", err)
	}
	if want := uint32(os.Getuid()); uid != want { //nolint:gosec // UID fits in uint32
		t.Fatalf("UID = %d, want %d", uid, want)
	}

	ok, err := MatchesSelf(srv)
	if err != nil {
		t.Fatalf("MatchesSelf: %v", err)
	}
	if !ok {
		t.Fatal("MatchesSelf = false for a same-user peer, want true")
	}
}

// Proves the rejection path: Matches against any UID other than the peer's
// returns false, which is the check the exec bridge relies on to refuse a
// different-user process.
func TestMatchesRejectsOtherUID(t *testing.T) {
	srv := dialedPair(t)

	ok, err := Matches(srv, uint32(os.Getuid())+1) //nolint:gosec // UID fits in uint32
	if err != nil {
		t.Fatalf("Matches: %v", err)
	}
	if ok {
		t.Fatal("Matches = true for a mismatched UID, want false")
	}
}
