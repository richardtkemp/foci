package ccstream

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"foci/internal/procx"
)

// `claude auth status` usually reads local credential files and returns at once,
// but it can also refresh the OAuth token over the network (CC 2.1.317 changelog),
// and killing it during a slow refresh could sign the user out. So the two limits
// below are separate (#2837):
var (
	// authStatusTimeout is how long CheckReady waits. When it passes, the result
	// is 'indeterminate' and the probe carries on detached.
	authStatusTimeout = 15 * time.Second
	// authStatusHardCap is how long the probe may run before it is killed, as a
	// backstop against a wedged binary. It is long enough for a slow refresh.
	authStatusHardCap = 5 * time.Minute
)

// authStatusWaitDelay bounds Output() after the probe exits or is killed, in case
// a descendant still holds stdout. Without it the detached goroutine could outlive
// the hard cap.
const authStatusWaitDelay = 10 * time.Second

// authStatus is the subset of `claude auth status` JSON output we consume.
// The CLI prints this object to stdout regardless of login state, with
// loggedIn=false when there is no usable credential.
type authStatus struct {
	LoggedIn   bool   `json:"loggedIn"`
	AuthMethod string `json:"authMethod"`
	Email      string `json:"email"`
	// LoginExpiresAt is when a claude.ai sign-in ends (CC 2.1.318+).
	LoginExpiresAt credentialExpiry `json:"loginExpiresAt"`
}

// CheckReady verifies Claude Code is authenticated and triggers re-login if it
// is not. See delegator.Delegator.CheckReady for the contract. Safe to call
// before Start: it shells out to `claude auth status` and touches no subprocess
// state.
//
// On an indeterminate probe (the auth-status command failed to run or parse, or
// has not finished within authStatusTimeout) it returns (false, err) WITHOUT triggering login — a transient hiccup must not
// spuriously launch an interactive login flow.
func (b *Backend) CheckReady(ctx context.Context) (bool, error) {

	st, err := b.queryAuthStatus(ctx)
	if err != nil {
		return false, err
	}
	if st.LoggedIn {
		b.logger().Infof("readiness check: authenticated (method=%s)", st.AuthMethod)
		b.reportLoginExpiry(st.LoginExpiresAt)
		return true, nil
	}

	b.logger().Warnf("readiness check: claude auth status reports NOT logged in — triggering re-login")
	b.fireAuthFailure("startup readiness check: claude auth status reports not logged in")
	return false, nil
}

// queryAuthStatus runs `claude auth status` and returns its parsed output, or an
// error if the result is not known within authStatusTimeout or before ctx ends.
// Neither of those ends the probe: it can be saving a refreshed token, so it runs
// on detached until it exits or reaches authStatusHardCap. A late result is only
// logged, because a timed-out check stays indeterminate.
//
// It uses the same binary resolution as Start (resolveBinary) and the same
// OPERATOR population — foci-gw runs as the agent's user, so HOME points at the
// ~/.claude that holds the shared OAuth credential. procx.Spawn installs the
// population's environment, so there is nothing to assign here.
func (b *Backend) queryAuthStatus(ctx context.Context) (authStatus, error) {
	if err := ctx.Err(); err != nil {
		return authStatus{}, fmt.Errorf("ccstream: auth status not started: %w", err)
	}
	claudeBin := b.resolveBinary()

	// WithoutCancel: the probe's lifetime is the hard cap, not the caller's ctx.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), authStatusHardCap)

	// Operator: same binary and credentials as the session spawn.
	cmd := procx.Spawn(pctx, procx.Operator, claudeBin, "auth", "status")
	cmd.WaitDelay = authStatusWaitDelay
	if b.workDir != "" {
		cmd.Dir = b.workDir
	}

	type probeResult struct {
		st  authStatus
		err error
	}
	done := make(chan probeResult, 1)
	go func() {
		defer cancel()
		st, err := runAuthStatus(cmd, claudeBin)
		done <- probeResult{st, err}
	}()

	timer := time.NewTimer(authStatusTimeout)
	defer timer.Stop()
	var stillRunning error
	select {
	case r := <-done:
		return r.st, r.err
	case <-timer.C:
		stillRunning = fmt.Errorf("ccstream: %q auth status still running after %s", claudeBin, authStatusTimeout)
	case <-ctx.Done():
		stillRunning = fmt.Errorf("ccstream: %q auth status still running when the wait ended: %w", claudeBin, ctx.Err())
	}

	// Stop waiting. The probe carries on, and this goroutine reports how it ended.
	start := time.Now()
	lg := b.logger()
	go func() {
		r := <-done
		if r.err != nil {
			lg.Warnf("detached auth status probe failed after a further %s: %v", time.Since(start).Round(time.Second), r.err)
			return
		}
		lg.Infof("detached auth status probe finished after a further %s: loggedIn=%v (not acted on)", time.Since(start).Round(time.Second), r.st.LoggedIn)
	}()
	return authStatus{}, stillRunning
}

// runAuthStatus runs cmd to its end and parses stdout.
//
// `claude auth status` signals login state via the EXIT CODE (0=logged in,
// non-zero=not) but prints the same JSON body to stdout in both cases. So
// the non-zero exit is expected for the not-authenticated case — parse
// stdout first and only treat a missing/unparseable body as a genuine probe
// failure (binary absent, crash, garbage output). runErr is captured but
// subordinate to whether we got parseable JSON.
func runAuthStatus(cmd *exec.Cmd, claudeBin string) (authStatus, error) {
	out, runErr := cmd.Output()
	var st authStatus
	if err := json.Unmarshal(out, &st); err != nil {
		if runErr != nil {
			return authStatus{}, fmt.Errorf("ccstream: run %q auth status: %w", claudeBin, runErr)
		}
		return authStatus{}, fmt.Errorf("ccstream: parse auth status output %q: %w", strings.TrimSpace(string(out)), err)
	}
	return st, nil
}
