package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"foci/internal/delegator/hookbin"
	"foci/internal/delegator/sessionenv"
	"foci/internal/procx"
)

// ---------------------------------------------------------------------------
// Codex PreToolUse hook — per-thread exec-bridge env routing
//
// An app-server process has ONE environment. Where foci runs one app-server
// per session that environment is right by construction; where it shares one
// across an agent's sessions (batch threads today, session facades next) the
// FOCI_SOCK / FOCI_SESSION_KEY / BASH_ENV baked in at spawn belong to whoever
// started the process, and every other thread's bash calls quietly route to
// that thread's exec bridge — wrong chat, no error. Codex offers no per-thread
// env lever, so foci installs a PreToolUse hook that rewrites the command to
// carry the right environment (see cmd/foci-codex-hook and
// internal/delegator/sessionenv).
//
// WHERE THE CONFIG GOES. Everything below is passed as `-c` session flags on
// the app-server's own argv — the hook entry AND its trust state. Nothing is
// written to ~/.codex/config.toml. That is the whole point: the codex config
// is the USER's, shared with their own codex CLI and with every other codex
// agent on the host, and foci already learned once (#1327) that writing its
// own settings there leaks between them and races concurrent writers. Session
// flags are per-process, need no locking, and disappear with the process.
//
// TRUST — the operational trap. An untrusted hook on `codex app-server` is
// SILENTLY SKIPPED: no error, no warning, no log line, and the misrouting the
// hook exists to prevent simply continues. `--dangerously-bypass-hook-trust`
// does not exist on app-server (it is a codex exec / TUI flag). Trust is keyed
// by a hash codex computes over the hook's config ENTRY, which foci cannot
// compute itself and which changes whenever the entry changes (verified: it
// varies with the command string and with the timeout). So foci asks codex:
// a short-lived probe app-server started with the same hook flags answers
// `hooks/list` with the key and currentHash, and the real launch passes them
// back as `hooks.state`. The answer is memoised per hook-config for the life
// of the process, so this costs one extra codex spawn per foci-gw, not one per
// session. The hash covers the config entry and not the script body, so foci
// can ship a new foci-codex-hook binary without re-probing.
// ---------------------------------------------------------------------------

// hookCommandName is the helper binary foci installs as the PreToolUse hook.
const hookCommandName = "foci-codex-hook"

// envDirFlag must match cmd/foci-codex-hook/main.go's envDirFlag. Held as a
// constant in both places so a rename surfaces as a build failure.
const envDirFlag = "--env-dir"

// bashToolMatcher scopes the hook to codex's Bash tool. Codex matches it as a
// regex against the tool name, so it is anchored: no other tool call should
// pay for a hook process, and no other tool's input should be rewritten.
const bashToolMatcher = "^Bash$"

// hookTimeoutSeconds bounds one hook invocation. The helper does a JSON decode
// and one small file read — milliseconds — but it sits in front of every bash
// call, so the timeout is a hang guard, not a budget.
const hookTimeoutSeconds = 10

// hookProbeTimeout bounds the trust probe end to end. Failure is non-fatal
// (the launch proceeds with an untrusted, therefore inert, hook) so this only
// needs to be long enough for a cold `codex app-server` start.
const hookProbeTimeout = 30 * time.Second

// hookTrust is codex's identity for one configured hook entry.
type hookTrust struct {
	Key  string
	Hash string
}

var (
	hookTrustMu   sync.Mutex
	hookTrustMemo = map[string]hookTrust{}
)

// hookConfigArgs renders the `-c` flags that install the PreToolUse hook.
// Returns nil when the helper binary can't be found, in which case the caller
// launches without the hook — exactly today's behaviour.
func hookConfigArgs(hookCmd string) []string {
	if hookCmd == "" {
		return nil
	}
	value := fmt.Sprintf(
		"hooks.PreToolUse=[{matcher=%s,hooks=[{type=\"command\",command=%s,timeout=%d}]}]",
		tomlBasicString(bashToolMatcher), tomlBasicString(hookCmd), hookTimeoutSeconds)
	return []string{"-c", value}
}

// hookTrustArgs renders the `-c` flag that marks the hook trusted. Without it
// codex loads the hook and never runs it.
func hookTrustArgs(t hookTrust) []string {
	if t.Key == "" || t.Hash == "" {
		return nil
	}
	value := fmt.Sprintf("hooks.state={%s={enabled=true,trusted_hash=%s}}",
		tomlBasicString(t.Key), tomlBasicString(t.Hash))
	return []string{"-c", value}
}

// buildHookCommand composes the command string codex executes. The binary path
// is shell-quoted so a path containing spaces survives codex's `SHELL -lc`
// invocation of it.
func buildHookCommand(hookPath, envDir string) string {
	return sessionenv.ShellQuote(hookPath) + " " + envDirFlag + " " + sessionenv.ShellQuote(envDir)
}

// prepareHookArgs returns the full set of `-c` flags installing the session-env
// hook in a trusted state, or nil to launch without it. Every failure is a
// logged warning rather than an error: a codex session without the hook is the
// pre-existing behaviour (correct for the process's own session, misrouted for
// the rest), and that is strictly better than refusing to start.
func (b *Backend) prepareHookArgs(ctx context.Context) []string {
	hookPath, err := hookbin.Resolve(hookCommandName)
	if err != nil {
		b.lg.Warnf("session-env hook skipped: %v (bash tool calls will use the app-server's own bridge env)", err)
		return nil
	}
	cfg := hookConfigArgs(buildHookCommand(hookPath, sessionenv.Dir()))
	if cfg == nil {
		return nil
	}
	trust, err := b.resolveHookTrust(ctx, cfg)
	if err != nil {
		b.lg.Warnf("session-env hook skipped: trust probe failed: %v", err)
		return nil
	}
	b.lg.Infof("session-env hook installed (key=%s)", trust.Key)
	return append(cfg, hookTrustArgs(trust)...)
}

// resolveHookTrust returns the key and hash codex assigns to our hook entry,
// asking a throwaway app-server the first time it sees a given config.
func (b *Backend) resolveHookTrust(ctx context.Context, cfgArgs []string) (hookTrust, error) {
	memoKey := strings.Join(cfgArgs, "\x00")
	hookTrustMu.Lock()
	if t, ok := hookTrustMemo[memoKey]; ok {
		hookTrustMu.Unlock()
		return t, nil
	}
	hookTrustMu.Unlock()

	t, err := probeHookTrust(ctx, b.codexBinary(), cfgArgs, b.workDir)
	if err != nil {
		return hookTrust{}, err
	}
	hookTrustMu.Lock()
	hookTrustMemo[memoKey] = t
	hookTrustMu.Unlock()
	return t, nil
}

// hooksListResponse is the `hooks/list` result. Only the fields identifying a
// hook entry are modelled.
type hooksListResponse struct {
	Data []struct {
		Hooks []struct {
			Key         string `json:"key"`
			EventName   string `json:"eventName"`
			Command     string `json:"command"`
			CurrentHash string `json:"currentHash"`
			TrustStatus string `json:"trustStatus"`
		} `json:"hooks"`
	} `json:"data"`
}

// probeHookTrust starts a disposable app-server with the same hook flags the
// real launch will use, asks it to enumerate the hooks it loaded, and returns
// the identity of ours. The probe never starts a thread and never touches the
// model, so it needs no credentials and costs one process start.
func probeHookTrust(ctx context.Context, bin string, cfgArgs []string, workDir string) (hookTrust, error) {
	ctx, cancel := context.WithTimeout(ctx, hookProbeTimeout)
	defer cancel()

	args := append([]string{"app-server", "-c", "sandbox_policy.mode=danger-full-access"}, cfgArgs...)
	cmd := procx.Spawn(ctx, bin, args...)
	cmd.Dir = workDir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return hookTrust{}, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return hookTrust{}, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return hookTrust{}, fmt.Errorf("start: %w", err)
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()

	w := NewWriter(stdin)
	if err := w.sendRequest("initialize", initializeParams{
		ClientInfo: clientInfo{Name: "foci", Title: "Foci", Version: "hook-probe"},
	}, 1); err != nil {
		return hookTrust{}, fmt.Errorf("initialize: %w", err)
	}
	if err := w.sendNotification("initialized", struct{}{}); err != nil {
		return hookTrust{}, fmt.Errorf("initialized: %w", err)
	}
	if err := w.sendRequest("hooks/list", map[string]any{"cwds": []string{workDir}}, 2); err != nil {
		return hookTrust{}, fmt.Errorf("hooks/list: %w", err)
	}

	raw, err := readRPCResult(ctx, stdout, 2)
	if err != nil {
		return hookTrust{}, err
	}
	var resp hooksListResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return hookTrust{}, fmt.Errorf("parse hooks/list: %w", err)
	}
	for _, entry := range resp.Data {
		for _, h := range entry.Hooks {
			if h.EventName == "preToolUse" && strings.Contains(h.Command, hookCommandName) {
				if h.CurrentHash == "" {
					return hookTrust{}, errors.New("hooks/list reported our hook without a hash")
				}
				return hookTrust{Key: h.Key, Hash: h.CurrentHash}, nil
			}
		}
	}
	return hookTrust{}, errors.New("hooks/list did not report the foci hook (config rejected?)")
}

// readRPCResult scans JSON-RPC lines until the reply to wantID arrives.
// Notifications and other traffic on the way are discarded — the probe has no
// session and cares about nothing else.
func readRPCResult(ctx context.Context, r interface{ Read([]byte) (int, error) }, wantID int64) (json.RawMessage, error) {
	type reply struct {
		ID     *int64          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	type outcome struct {
		raw json.RawMessage
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var m reply
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil || m.ID == nil || *m.ID != wantID {
				continue
			}
			if m.Error != nil {
				done <- outcome{err: fmt.Errorf("rpc error: %s", m.Error.Message)}
				return
			}
			done <- outcome{raw: m.Result}
			return
		}
		err := sc.Err()
		if err == nil {
			err = errors.New("app-server closed before replying")
		}
		done <- outcome{err: err}
	}()
	select {
	case o := <-done:
		return o.raw, o.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// Per-thread binding
// ---------------------------------------------------------------------------

// bindThreadEnv publishes this session's exec-bridge environment under the
// codex thread id, which is what the hook sees as session_id. Call it from
// registerThread and nowhere else: that IS the point at which a thread becomes
// bound to a foci session, and hand-placing this next to some of
// registerThread's call sites is exactly how batch and notification-discovered
// threads ended up with no env of their own.
func (b *Backend) bindThreadEnv(threadID string) {
	if err := sessionenv.Write(threadID, b.startOpts.Env); err != nil {
		b.lg.Warnf("session-env: %v", err)
		return
	}
	b.lg.Debugf("session-env: bound thread %s to session %s", threadID, b.startOpts.SessionKey)
}

// unbindThreadEnv drops the mapping when the thread goes away. Best-effort:
// a leftover file is harmless (tempdir is wiped at gateway startup) and its
// thread id is never reused.
func (b *Backend) unbindThreadEnv(threadID string) {
	// Exactly the condition bindThreadEnv writes under (sessionenv.Write
	// returns early on a zero entry, before it resolves the temp root). Without
	// this, a session with no exec-bridge env still resolves the session-env
	// directory on every thread teardown just to unlink a file it never wrote.
	if threadID == "" || sessionenv.EntryFrom(b.startOpts.Env).IsZero() {
		return
	}
	if err := sessionenv.Remove(threadID); err != nil {
		b.lg.Debugf("session-env: %v", err)
	}
}
