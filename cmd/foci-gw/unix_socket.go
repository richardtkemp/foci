package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"foci/internal/config"
	"foci/internal/log"
)

// peerUIDKey is the context key for the peer UID extracted from Unix socket connections.
type peerUIDKey struct{}

// resolveSocketPath returns the Unix socket path for same-user authentication.
// Uses the configured path if set, otherwise defaults to data dir.
func resolveSocketPath(cfg *config.Config) string {
	if cfg.HTTP.SocketPath != "" {
		return config.ResolvePath(cfg.HTTP.SocketPath)
	}
	return cfg.DataPath("foci-gw.sock")
}

// startUnixSocket creates a Unix domain socket listener and starts serving HTTP.
// The socket file is created with mode 0600 (owner-only) and a peer credential
// middleware verifies that connecting processes belong to the same user.
// Returns the server for shutdown/cleanup.
func startUnixSocket(sockPath string, handler http.Handler) (*http.Server, error) {
	// Remove stale socket if it exists (e.g. from a crash).
	if err := removeStaleSocket(sockPath); err != nil {
		return nil, err
	}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, err
	}

	// Restrict to owner only (defense in depth — peer creds also checked).
	if err := os.Chmod(sockPath, 0600); err != nil {
		_ = ln.Close()
		_ = os.Remove(sockPath)
		return nil, err
	}

	srv := &http.Server{
		Handler:           peerCredMiddleware(handler),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		ConnContext:        injectPeerUID,
	}

	go func() {
		if err := srv.Serve(ln); err != http.ErrServerClosed {
			log.Errorf("http", "unix socket server error: %v", err)
		}
	}()

	return srv, nil
}

// removeStaleSocket removes a leftover socket file from a previous run.
// Returns an error only if the path exists and is not a socket (refuse to
// delete regular files).
func removeStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return &os.PathError{Op: "remove", Path: path, Err: os.ErrExist}
	}
	return os.Remove(path)
}

// injectPeerUID is a ConnContext function for http.Server that extracts the
// peer UID from Unix socket connections and stores it in the request context.
func injectPeerUID(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	uid, err := getPeerUID(uc)
	if err != nil {
		log.Debugf("http", "unix socket peer cred error: %v", err)
		return ctx
	}
	return context.WithValue(ctx, peerUIDKey{}, uid)
}

// peerCredMiddleware verifies that the connecting process belongs to the same
// user as the gateway. This is defense in depth — the socket file permissions
// (0600) already restrict access to the owner.
func peerCredMiddleware(next http.Handler) http.Handler {
	myUID := uint32(os.Getuid()) //nolint:gosec // UID fits in uint32 on all supported platforms
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, ok := r.Context().Value(peerUIDKey{}).(uint32)
		if !ok {
			http.Error(w, "peer credential check failed", http.StatusForbidden)
			return
		}
		if uid != myUID {
			http.Error(w, "peer UID mismatch", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// getPeerUID extracts the UID of the process on the other end of a Unix socket
// connection using SO_PEERCRED.
func getPeerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var credErr error
	controlErr := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if controlErr != nil {
		return 0, controlErr
	}
	if credErr != nil {
		return 0, credErr
	}
	return cred.Uid, nil
}

// cleanupSocket removes the socket file and closes the listener.
func cleanupSocket(srv *http.Server, sockPath string) {
	if srv != nil {
		_ = srv.Close()
	}
	_ = os.Remove(sockPath)
}
