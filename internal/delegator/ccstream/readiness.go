package ccstream

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"foci/internal/procx"
)

// authStatusTimeout bounds the `claude auth status` probe. The command reads
// local credential files (no network round-trip), so it returns near-instantly;
// the timeout is a backstop against a wedged binary, not a normal-path wait.
var authStatusTimeout = 15 * time.Second

// authStatus is the subset of `claude auth status` JSON output we consume.
// The CLI prints this object to stdout regardless of login state, with
// loggedIn=false when there is no usable credential.
type authStatus struct {
	LoggedIn   bool   `json:"loggedIn"`
	AuthMethod string `json:"authMethod"`
	Email      string `json:"email"`
}

// CheckReady verifies Claude Code is authenticated and triggers re-login if it
// is not. See delegator.Delegator.CheckReady for the contract. Safe to call
// before Start: it shells out to `claude auth status` and touches no subprocess
// state.
//
// On an indeterminate probe (the auth-status command failed to run or parse) it
// returns (false, err) WITHOUT triggering login — a transient hiccup must not
// spuriously launch an interactive login flow.
func (b *Backend) CheckReady(ctx context.Context) (bool, error) {

	st, err := b.queryAuthStatus(ctx)
	if err != nil {
		return false, err
	}
	if st.LoggedIn {
		b.logger().Infof("readiness check: authenticated (method=%s)", st.AuthMethod)
		return true, nil
	}

	b.logger().Warnf("readiness check: claude auth status reports NOT logged in — triggering re-login")
	b.fireAuthFailure("startup readiness check: claude auth status reports not logged in")
	return false, nil
}

// queryAuthStatus runs `claude auth status` and parses its JSON output. It uses
// the same binary resolution as Start (cfg["binary"], default "claude") and
// inherits the gateway process environment — foci-gw runs as the agent's
// user, so HOME points at the ~/.claude that holds the shared OAuth credential.
func (b *Backend) queryAuthStatus(ctx context.Context) (authStatus, error) {
	claudeBin := "claude"
	if v, ok := b.cfg["binary"].(string); ok && v != "" {
		claudeBin = v
	}

	cctx, cancel := context.WithTimeout(ctx, authStatusTimeout)
	defer cancel()

	cmd := procx.Spawn(cctx, claudeBin, "auth", "status")
	cmd.Env = os.Environ()
	if b.workDir != "" {
		cmd.Dir = b.workDir
	}

	// `claude auth status` signals login state via the EXIT CODE (0=logged in,
	// non-zero=not) but prints the same JSON body to stdout in both cases. So
	// the non-zero exit is expected for the not-authenticated case — parse
	// stdout first and only treat a missing/unparseable body as a genuine probe
	// failure (binary absent, crash, garbage output). runErr is captured but
	// subordinate to whether we got parseable JSON.
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
