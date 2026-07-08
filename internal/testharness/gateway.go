package testharness

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"

	"foci/internal/session"
	"foci/internal/testtemp"
)

// AgentSpec describes one agent to wire up in a test foci-gw.
// Tests typically declare 1-2 agents per scenario; the harness creates
// per-agent workspaces and platform bindings.
type AgentSpec struct {
	ID        string // e.g. "clutch", "fotini"
	BotToken  string // Telegram token to register; auto-generated if empty
	UserID    int64  // synthetic Telegram user_id allowed to message this bot
	BotUserID int64  // synthetic Telegram user_id for the bot itself
	// SkipStubRegister, if true, skips registering this agent's bot
	// token with the Telegram stub. Used to test misconfigurations where
	// the foci.toml names a token the gateway can't reach. The token is
	// still written into the generated config; only RegisterBot is
	// skipped.
	SkipStubRegister bool

	// Permission knobs — populate [agents.permissions] subkeys. Used by
	// the permission-test family (permissions_test.go) to verify foci's
	// per-agent auto-approve behaviour. Nil/empty = inherit defaults
	// (auto_approve_common_readonly defaults to true; the other two
	// default to off / empty).
	AutoApprove                []string
	AutoApproveCommonReadonly  *bool
	AutoApproveCommonSafeWrite *bool

	// ExtraEnv populates [agents.backend_config.env] so the per-agent
	// backend subprocess (cc-stub in L2 tests) receives the listed
	// environment variables. Use for lifecycle env vars CCSTUB_HANG,
	// CCSTUB_EXIT_CODE, CCSTUB_FAIL_ON_RESUME, etc. — these need to be
	// scoped to one agent without polluting peers.
	//
	// Values are emitted as TOML strings, so consumers should treat all
	// values as strings (foci's backendConfigEnv coerces back). Order
	// in the TOML output is sorted by key for stable test snapshots.
	ExtraEnv map[string]string

	// PreStartFiles maps workspace-relative paths to file contents that
	// are written into the agent's workspace AFTER writeWorkspaces but
	// BEFORE foci-gw spawns. Use for tests that need foci to discover
	// files at startup — memory indexing, character file additions,
	// skill seeds. Paths must be relative and may contain subdirectories
	// (parent dirs are created with 0o755).
	PreStartFiles map[string]string

	// OmitWorkspaceKey, if true, suppresses the `workspace = <path>` line
	// in the generated [[agents]] block so load.go's convention default
	// (`$HOME/<id>`) fires. Tests that flip this MUST also expect HOME
	// inside foci-gw to point at the tempDir/workspaces dir — the
	// harness automatically sets `HOME` on the gateway subprocess when
	// any agent has this flag, so the resolved default matches the
	// pre-seeded workspace path.
	OmitWorkspaceKey bool

	// OmitPlatformBotKey, if true, suppresses the `bot = <id>` line in
	// the per-agent `[[agents.platforms]]` block so ensureAgentPlatform's
	// convention default (bot name = agent ID) fires. Tests assert the
	// resolved value via downstream behaviour (e.g. the bot's long-poll
	// successfully registers).
	OmitPlatformBotKey bool

	// OmitPlatformAllowedUsersKey, if true, suppresses the
	// `allowed_users = [...]` line in the per-agent
	// `[agents.platforms.access]` block. Used together with a global
	// `allowed_users_only = false` to prove the "empty list + non-strict
	// mode accepts any user" branch of the access gate.
	OmitPlatformAllowedUsersKey bool

	// PlatformBotSecret, when non-empty, emits a `bot_secret = "<value>"`
	// line on the per-agent `[[agents.platforms]]` block. Foci resolves
	// the bot token via the named secret path (e.g. "custom.weird_token")
	// instead of the `<platform>.<bot>` convention. Used together with
	// ExtraSecretsTOML to register the actual token at the named path.
	PlatformBotSecret string

	// OmitDefaultPlatformSecret, if true, suppresses the default
	// `<agentID> = "<token>"` entry that writeTestSecrets writes into
	// the [telegram] section. Use to prove that an override path
	// (e.g. PlatformBotSecret pointing at a custom section) is being
	// preferred over the convention — without the convention secret
	// present, the bot can only authenticate via the override.
	OmitDefaultPlatformSecret bool

	// ClaudeBinary, when non-empty, emits
	// `claude_binary = "<path>"` on this agent's
	// `[agents.backend_config]` block. Per-agent override beats the
	// global `[cc_backend].claude_binary`. Used by override tests that
	// need to prove the per-agent path was picked up — empty falls back
	// to the global cc-stub binary the harness builds.
	ClaudeBinary string
}

// preStartFiles returns the AgentSpec's PreStartFiles map (nil-safe).
// Centralised so the harness loop can iterate without nil-checks.
func (a AgentSpec) preStartFiles() map[string]string {
	if a.PreStartFiles == nil {
		return nil
	}
	return a.PreStartFiles
}

// CorrectnessWaitFloor is the minimum budget for a test wait whose assertion
// is about *eventual occurrence* (a message was sent, a socket appeared, a turn
// was processed) rather than speed. Load-induced latency (concurrent builds,
// gateway-boot contention, a busy CI box) must not trip such a wait, so every
// wait in this family is floored to at least this long. Waits that genuinely
// assert timing (poll frequency, retry rate) bypass it. 60s is generous over
// the largest previously-hand-tuned value (30s); with the /tmp/heavy flock
// preventing build-vs-test contention, waits normally return in well under a
// second and the floor only bites when something is genuinely slow.
const CorrectnessWaitFloor = 60 * time.Second

// HarnessOptions configures a test foci-gw subprocess.
type HarnessOptions struct {
	Agents []AgentSpec
	// LogTail, if true, tees foci-gw stderr to t.Log for debugging.
	LogTail bool
	// ReadyTimeout caps how long to wait for the gateway to log the
	// terminal startup line. Zero = 20s.
	ReadyTimeout time.Duration
	// ExtraConfigTOML, if non-empty, is appended verbatim to the
	// generated foci.toml. Use to inject sections the default config
	// writer doesn't emit ([keepalive], [reflection], [background],
	// [platforms.display], [defaults.behavior], [logging], etc.).
	ExtraConfigTOML string
	// ExtraSecretsTOML, if non-empty, is appended verbatim to the
	// generated secrets.toml. Use to inject custom secret sections
	// (e.g. [custom] my_key = "...") for tests exercising
	// {{secret:...}} template resolution from arbitrary sources.
	ExtraSecretsTOML string
	// SkipSecretsFile, if true, suppresses generation of secrets.toml
	// entirely. Used to test foci-gw's behaviour when the secrets file
	// is missing.
	SkipSecretsFile bool
	// ClaudeBinary, if non-empty, overrides the auto-built cc-stub
	// binary path. Used to point at a non-existent file (missing-binary
	// scenarios) or a custom stub variant. Empty value preserves the
	// default behaviour of building cc-stub from source.
	ClaudeBinary string
	// BackendReadyTimeout, if non-zero, sets the WaitReady budget that
	// foci-gw's delegated_manager gives a freshly-spawned coding-agent
	// backend to complete its init handshake. Propagated via the
	// FOCI_BACKEND_READY_TIMEOUT env var, read at backend-create time.
	// Use to keep init-timeout-then-recovery tests inside CI wall-clock
	// budgets — the production default is 60s, far too long for L2.
	BackendReadyTimeout time.Duration
	// PreStartDataFiles writes files into the gateway's data dir BEFORE
	// foci-gw spawns. Keys are paths relative to DataDir() (e.g.
	// "sessions/agentX/abc123.jsonl"); values are the file contents.
	// Use for tests that need to seed corrupted/legacy session JSONLs,
	// stale resume files, or other on-disk state foci should observe at
	// startup. Parent dirs are created with 0o755; files are written
	// with 0o600. Paths must be relative (no leading "/", no "..").
	PreStartDataFiles map[string]string
	// SeedAgentMetadata writes rows into the session index's agent_metadata
	// table BEFORE foci-gw spawns: outer key is the agent ID, inner map is
	// key→value. Use to set up persistent-state preconditions a fresh boot
	// would otherwise lack — e.g. an overdue "consolidation_last" so a
	// scheduler fires promptly despite a long interval. Written via the same
	// SessionIndex foci-gw reads at startup.
	SeedAgentMetadata map[string]map[string]string
}

// Harness drives one foci-gw subprocess against a Telegram stub. Tests
// access the stub via TelegramStub() and the cc-stub recorder file via
// RecorderPath(); they push synthetic updates and assert on what foci
// did to the recorder.
// MaxIntegTestNameLen is the sun_path budget for integration test names:
// under `make integration`, t.TempDir() is
// /tmp/foci/integration-<unix10>/<TestName><rand10>/001, and that path must
// itself stay within sockaddr_un.sun_path (108 incl NUL) — the necessary
// condition for ANY unix socket ever placed under a test's TempDir (real
// sockets need headroom on top, which is why harness sockets live in short
// /tmp/fcs*//tmp/fgw* dirs instead; TODO #804). Enforced at build time by
// TestIntegrationTestNamesFitSunPath.
const MaxIntegTestNameLen = 107 - len("/tmp/foci/integration-1234567890/") - 10 - len("/001")

type Harness struct {
	t            *testing.T
	tempDir      string
	dataDir      string
	logsDir      string
	configPath   string
	tgStub       *TelegramStub
	recorderPath string
	scriptDir    string
	workspaces   map[string]string // agent id → workspace path
	agents       []AgentSpec

	// controlSock is the absolute path to the unix-domain control
	// socket foci-gw listens on for L2 lifecycle-control commands. Set
	// at harness startup; foci-gw opens it when FOCI_TESTHARNESS_CONTROL_SOCK
	// is set in its env. Empty means no control socket was requested.
	controlSock string

	// gwSock is the gateway's same-user auth unix socket ([http]
	// socket_path). Allocated under a short /tmp dir — not DataDir — so
	// long test names can't overflow sun_path (TODO #804).
	gwSock string

	// Spawn-time invariants captured so Restart can re-run spawnGateway
	// without re-doing the build / config / stub-setup work.
	gwBin        string
	binDir       string // for lazy CLI build via FociCLI()
	ccStubBin    string // path to the shared cc-stub binary (used as the global ClaudeBinary)
	repoRoot     string // for lazy CLI build via FociCLI()
	fociCLIBin   string // set on first FociCLI() call; "" until built
	fociCLIOnce  sync.Once
	readyTimeout time.Duration
	opts         HarnessOptions

	cmd       *exec.Cmd
	stderrBuf *syncBuffer
	stoppedCh chan struct{}
}

// StartGateway builds foci-gw + cc-stub from source, generates a test
// config in t.TempDir(), spawns foci-gw, and waits for the terminal
// startup log line. The harness shuts down on t.Cleanup.
//
// Build artefacts are cached under t.TempDir/bin so a test process can
// reuse them across sub-tests via the harness; repeated test runs hit
// Go's build cache and rebuild only on source change.
//
// Use [TryStartGateway] if the test expects the gateway to fail to
// start (e.g. malformed config, duplicate bot token, expect-startup-
// error scenarios) — StartGateway calls t.Fatalf on any startup error.
func StartGateway(t *testing.T, opts HarnessOptions) *Harness {
	t.Helper()
	h, err := tryStartGateway(t, opts)
	if err != nil {
		t.Fatalf("StartGateway: %v", err)
	}
	return h
}

// TryStartGateway is the non-Fatal variant of [StartGateway]. It returns
// the startup error instead of calling t.Fatalf, letting tests assert on
// the failure mode (e.g. config parse error in stderr, exit code, etc.).
//
// The returned *Harness is non-nil on success only; on error it may be
// nil. Cleanup is still registered via t.Cleanup so any partial state
// (telegram stub, spawned process) is torn down at end of test.
func TryStartGateway(t *testing.T, opts HarnessOptions) (*Harness, error) {
	t.Helper()
	return tryStartGateway(t, opts)
}

func tryStartGateway(t *testing.T, opts HarnessOptions) (*Harness, error) {
	t.Helper()
	if len(opts.Agents) == 0 {
		return nil, fmt.Errorf("at least one agent required")
	}
	if opts.ReadyTimeout == 0 {
		opts.ReadyTimeout = 20 * time.Second
	}
	// Floor: gateway boot is a correctness wait (the gateway eventually comes
	// up), not a speed assertion — load-induced latency must not trip it.
	if opts.ReadyTimeout < CorrectnessWaitFloor {
		opts.ReadyTimeout = CorrectnessWaitFloor
	}

	tempDir := t.TempDir()
	binDir := filepath.Join(tempDir, "bin")
	dataDir := filepath.Join(tempDir, "data")
	logsDir := filepath.Join(tempDir, "logs")
	scriptDir := filepath.Join(tempDir, "cc-scripts")
	recorderPath := filepath.Join(tempDir, "cc-recorder.jsonl")
	for _, d := range []string{binDir, dataDir, logsDir, scriptDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// Build foci-gw + cc-stub ONCE per test process and share the binaries
	// across all tests. The binaries are identical for every test (same
	// source); per-test behaviour comes from the config and the cc-stub
	// script file, not the binary. Linking foci-gw is ~2.6s even with a warm
	// build cache, so re-linking it per test (×170 L2 tests) was the dominant
	// cost of the suite — see sharedBinary.
	repoRoot := findRepoRoot(t)
	gwBin := sharedBinary(t, repoRoot, "./cmd/foci-gw")
	stubBin := sharedBinary(t, repoRoot, "./cmd/cc-stub")
	claudeBinary := stubBin
	if opts.ClaudeBinary != "" {
		claudeBinary = opts.ClaudeBinary
	}

	// Telegram stub up before we spawn foci-gw so its getMe call lands.
	tgStub := NewTelegramStub()
	t.Cleanup(tgStub.Close)

	// Assign bot tokens and register them with the stub. Tokens are
	// always assigned (so the generated config is well-formed); the
	// per-agent SkipStubRegister flag suppresses RegisterBot only.
	for i := range opts.Agents {
		a := &opts.Agents[i]
		if a.BotToken == "" {
			a.BotToken = fmt.Sprintf("test-token-%s:%d", a.ID, time.Now().UnixNano())
		}
		if a.BotUserID == 0 {
			a.BotUserID = int64(1000 + i)
		}
		if a.UserID == 0 {
			a.UserID = int64(2000 + i)
		}
		if a.SkipStubRegister {
			continue
		}
		tgStub.RegisterBot(a.BotToken, gotgbot.User{
			Id:        a.BotUserID,
			IsBot:     true,
			FirstName: titleFirst(a.ID) + "Bot",
			Username:  a.ID + "_bot",
		})
	}

	configPath := filepath.Join(tempDir, "foci.toml")
	secretsPath := filepath.Join(tempDir, "secrets.toml")
	workspaces := writeWorkspaces(t, tempDir, opts.Agents)

	// Pre-start files: tests that need foci to discover content at
	// startup (memory indexing, custom character/* files, skill seeds)
	// write here. Files go to <workspace>/<rel-path>; parents are
	// created with 0o755. Validation: reject absolute or .. paths to
	// keep tests honest about scope.
	for _, a := range opts.Agents {
		ws := workspaces[a.ID]
		for rel, content := range a.preStartFiles() {
			if strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") {
				t.Fatalf("PreStartFiles path %q on agent %s must be relative and within the workspace",
					rel, a.ID)
			}
			full := filepath.Join(ws, rel)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatalf("mkdir for PreStartFile %s: %v", full, err)
			}
			if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
				t.Fatalf("write PreStartFile %s: %v", full, err)
			}
		}
	}

	// Pre-start data-dir files: tests that need foci to observe on-disk
	// state at startup (corrupted session JSONLs, stale resume files,
	// legacy formats) seed here. Paths are relative to dataDir; parents
	// are created with 0o755 so seed paths like "sessions/X/Y.jsonl"
	// don't require the test to create the subtree manually.
	for rel, content := range opts.PreStartDataFiles {
		if strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") {
			t.Fatalf("PreStartDataFiles path %q must be relative and within the data dir", rel)
		}
		full := filepath.Join(dataDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for PreStartDataFile %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write PreStartDataFile %s: %v", full, err)
		}
	}

	// Pick an ephemeral HTTP port so parallel tests don't all collide on the
	// default 18791. Brief race window between Close and foci-gw's bind, but
	// acceptable for test purposes — kernel rarely reuses an ephemeral port
	// that fast.
	httpPort, err := pickFreePort()
	if err != nil {
		return nil, fmt.Errorf("pick free port: %w", err)
	}

	// Gateway unix socket: same sun_path story as the control socket below
	// (TODO #804) — the default socket path lives under DataDir, which embeds
	// the test name and overflows 108 bytes for long-named tests, failing
	// bind with "invalid argument". Allocate a short /tmp dir instead.
	gwSockDir, err := testtemp.Mkdir("fgw")
	if err != nil {
		return nil, fmt.Errorf("alloc gateway-socket dir: %w", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(gwSockDir) })
	gwSock := filepath.Join(gwSockDir, "gw.sock")

	writeTestConfig(t, configPath, testConfigOpts{
		DataDir:         dataDir,
		LogsDir:         logsDir,
		ClaudeBinary:    claudeBinary,
		TelegramBase:    tgStub.URL(),
		SecretsPath:     secretsPath,
		Agents:          opts.Agents,
		Workspaces:      workspaces,
		RecorderPath:    recorderPath,
		HTTPPort:        httpPort,
		SocketPath:      gwSock,
		ExtraConfigTOML: opts.ExtraConfigTOML,
	})
	if !opts.SkipSecretsFile {
		writeTestSecrets(t, secretsPath, opts.Agents, opts.ExtraSecretsTOML)
	}

	// L2 lifecycle-control socket. Always allocate the path even if no
	// test in this run uses it — the cost is one extra env var on the
	// spawned subprocess and a small goroutine inside foci-gw. Tests
	// that need it call h.CloseAgentBackend.
	//
	// The socket path must NOT live under t.TempDir(): that path embeds the
	// (often long) test name, and combined with a long TMPDIR it overflows
	// the kernel's sockaddr_un.sun_path 108-byte limit, failing bind/connect
	// with "invalid argument" for the longest-named L2 tests (TODO #804).
	// Allocate a short, test-name-independent dir directly under /tmp so the
	// full path stays ~25 chars regardless of test name or TMPDIR.
	sockDir, err := testtemp.Mkdir("fcs")
	if err != nil {
		t.Fatalf("testharness: alloc control-socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	controlSock := filepath.Join(sockDir, "c.sock")

	h := &Harness{
		t:            t,
		tempDir:      tempDir,
		dataDir:      dataDir,
		logsDir:      logsDir,
		configPath:   configPath,
		tgStub:       tgStub,
		recorderPath: recorderPath,
		scriptDir:    scriptDir,
		workspaces:   workspaces,
		controlSock:  controlSock,
		gwSock:       gwSock,
		gwBin:        gwBin,
		binDir:       binDir,
		ccStubBin:    stubBin,
		repoRoot:     repoRoot,
		agents:       opts.Agents,
		readyTimeout: opts.ReadyTimeout,
		opts:         opts,
	}

	// Registered before spawnGateway so it runs LAST (cleanups are LIFO),
	// by which point t.Failed() reflects the test's final verdict.
	t.Cleanup(func() { auditWeight(t) })

	if len(opts.SeedAgentMetadata) > 0 {
		seedAgentMetadata(t, filepath.Join(dataDir, "state.db"), opts.SeedAgentMetadata)
	}

	if err := h.spawnGateway(); err != nil {
		return nil, err
	}
	return h, nil
}

// seedAgentMetadata writes the requested agent_metadata rows into the session
// index at dbPath before foci-gw opens it. Reuses the production SessionIndex
// so the schema and write path match exactly.
func seedAgentMetadata(t *testing.T, dbPath string, seed map[string]map[string]string) {
	t.Helper()
	idx, err := session.NewSessionIndex(dbPath)
	if err != nil {
		t.Fatalf("seed agent metadata: open session index: %v", err)
	}
	defer func() { _ = idx.Close() }()
	for agentID, kv := range seed {
		for k, v := range kv {
			if err := idx.SetAgentMetadata(agentID, k, v); err != nil {
				t.Fatalf("seed agent metadata %s/%s: %v", agentID, k, err)
			}
		}
	}
}

// spawnGateway forks the foci-gw subprocess, wires up streaming I/O
// into h.stderrBuf, and blocks until either the terminal "started N
// agent(s)" log line appears or readyTimeout elapses. The caller is
// responsible for assigning the harness fields (paths, opts) BEFORE
// invoking this — spawnGateway only sets h.cmd / h.stderrBuf /
// h.stoppedCh and registers cleanup.
//
// Reused by both initial StartGateway and Restart so the two paths
// share spawn-time invariants (env, ready-signal scan, cleanup
// registration).
func (h *Harness) spawnGateway() error {
	ctx, cancel := context.WithCancel(context.Background())
	// The harness launches the real foci-gw binary under test directly;
	// procx.Spawn is the production secrets-dropping wrapper and must not wrap
	// the test process.
	cmd := exec.CommandContext(ctx, h.gwBin, "-config", h.configPath) //nolint:forbidigo // integration harness launches the gw under test
	cmd.Env = append(os.Environ(),
		"CCSTUB_RECORDER="+h.recorderPath,
		"CCSTUB_SCRIPT_DIR="+h.scriptDir,
		// Pin the early log-init path to the test tempdir so foci-gw
		// doesn't open the host's production ~/logs/foci.log before
		// config load. See cmd/foci-gw/main.go:81 (FOCI_LOG_FILE).
		"FOCI_LOG_FILE="+filepath.Join(h.logsDir, "foci.log"),
		// L2 lifecycle-control socket. foci-gw's testharness_control.go
		// opens this path when set; harness commands like
		// CloseAgentBackend dial it.
		"FOCI_TESTHARNESS_CONTROL_SOCK="+h.controlSock,
	)
	if h.opts.BackendReadyTimeout > 0 {
		cmd.Env = append(cmd.Env, "FOCI_BACKEND_READY_TIMEOUT="+h.opts.BackendReadyTimeout.String())
	}
	// If any agent has OmitWorkspaceKey set, override HOME so foci's
	// load.go convention default ($HOME/<id>) resolves to the tempDir
	// workspaces subdir that writeWorkspaces already populated with the
	// per-agent character/CRAFT.md. Without this override, the host
	// user's actual home would be the resolved root — workspaces don't
	// exist there, foci's startup file loader complains, and the test
	// is observing host state instead of the test artefact.
	for _, a := range h.agents {
		if a.OmitWorkspaceKey {
			cmd.Env = append(cmd.Env, "HOME="+filepath.Join(h.tempDir, "workspaces"))
			break
		}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stderr pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start foci-gw: %w", err)
	}

	stderrBuf := newSyncBuffer()
	stoppedCh := make(chan struct{})

	// Stream stderr/stdout into the buffer so we can both wait-for-ready
	// and dump-on-failure.
	go func() {
		_, _ = io.Copy(stderrBuf, stderr)
	}()
	go func() {
		_, _ = io.Copy(stderrBuf, stdout)
	}()
	go func() {
		_ = cmd.Wait()
		close(stoppedCh)
	}()

	h.cmd = cmd
	h.stderrBuf = stderrBuf
	h.stoppedCh = stoppedCh

	// Register cleanup for THIS spawn. Multiple cleanups stack in
	// reverse order; on Restart we register again so both the original
	// (long-dead) and current cmd get a graceful-stop attempt — the
	// dead-cmd attempt is a fast no-op.
	logTail := h.opts.LogTail
	h.t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-stoppedCh:
		case <-time.After(3 * time.Second):
			cancel()
			<-stoppedCh
		}
		if logTail || h.t.Failed() {
			h.t.Logf("foci-gw stderr:\n%s", stderrBuf.String())
		}
	})

	timeout := h.readyTimeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	if err := waitForReady(stderrBuf, stoppedCh, timeout); err != nil {
		// On not-ready, surface both the error and the captured
		// stderr so callers can debug without re-grabbing it.
		return fmt.Errorf("foci-gw not ready: %w\n--- stderr ---\n%s", err, stderrBuf.String())
	}

	// Tripwire for the sun_path class of failure (TODO #804): if the gateway
	// failed to bind its same-user unix socket, every socket-dependent
	// operation in this test will time out later with a far less diagnostic
	// error — and only under `make integration` (short paths in isolation).
	// Fail loudly at the source instead. Bind failure is never legitimate in
	// the harness: the socket path is allocated short (see gwSock).
	if strings.Contains(stderrBuf.String(), "same-user auth unavailable") {
		return fmt.Errorf("foci-gw failed to bind its unix socket (sun_path overflow? see TODO #804; socket=%s)\n--- stderr ---\n%s", h.gwSock, stderrBuf.String())
	}
	return nil
}

// Shutdown gracefully stops the running foci-gw subprocess. Sends
// SIGTERM, waits for the process to exit (capped at the supplied
// timeout, then SIGKILL if it overstays its welcome), and blocks
// until the goroutines streaming stderr/stdout have drained. Safe to
// call from a test goroutine — does not race with the t.Cleanup that
// registered during spawn.
func (h *Harness) Shutdown(timeout time.Duration) error {
	if h.cmd == nil || h.cmd.Process == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-h.stoppedCh:
		return nil
	case <-time.After(timeout):
		_ = h.cmd.Process.Kill()
		<-h.stoppedCh
		return fmt.Errorf("foci-gw did not exit cleanly within %s; killed", timeout)
	}
}

// Restart shuts down the running foci-gw and spawns a fresh subprocess
// against the SAME data_dir / configPath / tokens / workspaces. Used
// for cross-process tests (wake-reminder survival, durable session
// state, etc.) where the on-disk state must outlive the supervising
// process. The stderr buffer resets — Stderr() after Restart returns
// only the new process's output.
//
// Shutdown is given 15s of grace before SIGKILL because foci-gw's
// backend Close() path can take up to ~10s when a cc-stub subprocess
// is mid-tool-use; a 5s budget would kill mid-restart far too often.
// A SIGKILL escalation does NOT fail the restart — the on-disk state
// is the only thing the next spawn relies on, and the test still
// proves the durability contract.
func (h *Harness) Restart() error {
	// Discard the "did not exit cleanly; killed" error — SIGKILL is
	// still a successful shutdown for restart purposes.
	_ = h.Shutdown(15 * time.Second)
	return h.spawnGateway()
}

// TelegramStub returns the underlying Bot API stub so tests can push
// updates and inspect outbound calls.
func (h *Harness) TelegramStub() *TelegramStub { return h.tgStub }

// RecorderPath returns the path to the cc-stub invocation recorder file.
// Tests read this file (JSONL) to assert on what foci handed CC.
func (h *Harness) RecorderPath() string { return h.recorderPath }

// FociCLI lazily builds the foci CLI binary (cmd/foci) into the
// harness binDir on first call and returns its path. Subsequent calls
// reuse the cached binary path. Tests use this to drive CLI-side
// command paths (e.g. `foci branch -mf ...`) that aren't exercised by
// the foci-gw HTTP surface alone.
//
// The binary is built without --addr/--socket wiring; tests must pass
// those flags (or env vars) themselves when the CLI needs to reach the
// running harness gateway. Tests that only exercise CLI-local code
// paths (flag parsing, file reads, malformed input) don't need any
// gateway wiring.
func (h *Harness) FociCLI(t *testing.T) string {
	t.Helper()
	h.fociCLIOnce.Do(func() {
		bin := filepath.Join(h.binDir, "foci")
		buildBinary(t, h.repoRoot, "./cmd/foci", bin)
		h.fociCLIBin = bin
	})
	return h.fociCLIBin
}

// AgentBotToken returns the bot token assigned to an agent. Tests use
// this to PushUpdate / DrainSent on the right bot.
func (h *Harness) AgentBotToken(agentID string) string {
	for _, a := range h.agents {
		if a.ID == agentID {
			return a.BotToken
		}
	}
	h.t.Fatalf("unknown agent %q in harness", agentID)
	return ""
}

// Stderr returns the captured foci-gw stderr/stdout (combined) so far.
// Useful for debugging or asserting on log lines.
func (h *Harness) Stderr() string {
	return h.stderrBuf.String()
}

// WriteCCStubScript writes a script file consumed by cc-stub when it
// runs in the named agent's workdir. The file content is a JSON object:
//
//	{"text": "<assistant text>", "tool_uses": [{"name":"Bash","input":{...}}]}
//
// cc-stub reads the file from $CCSTUB_SCRIPT_DIR/<workdir-basename>.json
// on its NEXT user message after spawn, emits the scripted assistant
// response, and clears its in-memory copy (one-shot). Tests can re-write
// the file between turns if multi-turn scripting is needed.
func (h *Harness) WriteCCStubScript(t *testing.T, agentID string, body []byte) {
	t.Helper()
	path := filepath.Join(h.scriptDir, agentID+".json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write cc-stub script for %s: %v", agentID, err)
	}
}

// ScriptDir returns the on-disk directory cc-stub reads scripts from.
// Useful for tests that want fine control over scripting state.
func (h *Harness) ScriptDir() string { return h.scriptDir }

// TempDir returns the root temp directory the harness allocated for
// this test. All other harness-owned paths (data, logs, workspaces,
// config, recorder, cc-scripts) live under it.
func (h *Harness) TempDir() string { return h.tempDir }

// CCStubBinary returns the path to the cc-stub binary this harness uses as the
// global ClaudeBinary. It is shared across all tests in the process (built
// once), so tests that need the stub's path — e.g. to symlink it as a
// per-agent override — must read it here rather than reconstructing
// TempDir()/bin/cc-stub.
func (h *Harness) CCStubBinary() string { return h.ccStubBin }

// SocketPath returns the gateway's same-user auth unix socket path.
func (h *Harness) SocketPath() string { return h.gwSock }

// DataDir returns the on-disk data directory foci-gw was configured
// with. Tests use it to seed JSONL session files, mutate permissions,
// or read foci's persisted state.
func (h *Harness) DataDir() string { return h.dataDir }

// LogsDir returns the on-disk logs directory foci-gw was configured
// with. Tests use it to assert on log files (foci.log, etc.) when the
// relevant config knob is also set.
func (h *Harness) LogsDir() string { return h.logsDir }

// SessionsDir returns the on-disk path where foci-gw persists per-
// session state (JSONL files keyed by agent/session). It's a fixed
// subdirectory of DataDir.
func (h *Harness) SessionsDir() string {
	return filepath.Join(h.dataDir, "sessions")
}

// AgentWorkspace returns the on-disk workspace path the harness
// allocated for an agent. Each workspace has a pre-seeded
// character/CRAFT.md and an empty memory/ directory.
func (h *Harness) AgentWorkspace(agentID string) string {
	if ws, ok := h.workspaces[agentID]; ok {
		return ws
	}
	h.t.Fatalf("unknown agent %q in harness (no workspace)", agentID)
	return ""
}

// ConfigPath returns the on-disk path of the generated foci.toml.
// Used by tests that want to inspect or mutate the config file after
// startup.
func (h *Harness) ConfigPath() string { return h.configPath }

// CloseAgentBackend forces the running foci-gw to close every
// DelegatedManager backend for the named agent — equivalent to a
// mid-turn SIGTERM/SIGKILL escalation against the agent's cc-stub
// subprocess(es), but scoped to one agent. Subsequent inbound messages
// on that agent's bot lazy-spawn a fresh backend. Used by lifecycle
// tests that need to assert OnTurnComplete fires exactly once on the
// in-flight turn before the respawn fires.
//
// The command travels over the env-gated control socket
// (FOCI_TESTHARNESS_CONTROL_SOCK). Errors include connection failures
// (foci-gw not listening / control socket cleanup race) and protocol
// errors ("unknown agent", "no delegated manager").
func (h *Harness) CloseAgentBackend(agentID string) error {
	return h.sendControl("close_backend " + agentID)
}

// SetActiveWork pins the agent's HasActiveWorkFn return value to the
// given count for subsequent periodic ticks. Counts ≥ 0 are returned
// verbatim by the closure; pass a negative count to clear the override
// (NOT YET WIRED — current production gateway treats any value ≥ 0 as
// the override, so tests typically Set once at the start of the
// scenario and let the harness tear down on cleanup).
//
// For delegated agents this is the only way to drive the background-
// scheduler gate that defers when tmux watches are in flight, because
// the production tmuxWatchCount closure is nil for delegated agents.
func (h *Harness) SetActiveWork(agentID string, count int) error {
	return h.sendControl(fmt.Sprintf("set_active_work %s %d", agentID, count))
}

// StopAgent flags the agent as stopped so the cross-agent
// session_notify resolver returns nil for it — exercises the
// "unknown target agent — message dropped" log path without tearing
// down the bot or backend. The agent's own bot keeps serving inbound
// messages (only the cross-agent resolver is affected).
func (h *Harness) StopAgent(agentID string) error {
	return h.sendControl("stop_agent " + agentID)
}

// SetCanFire pins the agent's CanFireFunc return value to (allowed,
// reason) for subsequent periodic ticks. The shared rate-limit / can_run_background
// gate runs at the top of every scheduler (background, reflection,
// consolidation); pinning allowed=false with a non-empty reason
// confirms all three schedulers consult the same gate.
//
// The reason string is passed verbatim over the line protocol after
// the boolean — newlines are not supported (commands are line-framed).
func (h *Harness) SetCanFire(agentID string, allowed bool, reason string) error {
	allowedStr := "0"
	if allowed {
		allowedStr = "1"
	}
	cmd := fmt.Sprintf("set_canfire %s %s", agentID, allowedStr)
	if reason != "" {
		cmd += " " + reason
	}
	return h.sendControl(cmd)
}

// sendControl dials the gateway's control socket, writes one command
// line, reads the one-line reply, and returns nil on "ok" or an error
// shaped from the reply text.
func (h *Harness) sendControl(cmd string) error {
	if h.controlSock == "" {
		return fmt.Errorf("testharness control socket not allocated")
	}
	conn, err := net.DialTimeout("unix", h.controlSock, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s: %w", h.controlSock, err)
	}
	defer func() { _ = conn.Close() }()
	// 20s budget: DelegatedManager.Close → ccstream.Close worst-case is
	// closeGracefulWait (5s) + closeSigtermWait (2s) + closeSigkillWait
	// (2s) = 9s per managed backend, and the harness may close multiple
	// backends in one call. 20s leaves comfortable headroom.
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return fmt.Errorf("set deadline: %w", err)
	}
	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		return fmt.Errorf("write command: %w", err)
	}
	br := bufio.NewReader(conn)
	reply, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read reply: %w", err)
	}
	reply = strings.TrimSpace(reply)
	if reply == "ok" {
		return nil
	}
	return fmt.Errorf("control reply: %s", reply)
}

// ----- Internal: ready-signal polling --------------------------------

var readyPattern = regexp.MustCompile(`started \d+ agent\(s\):`)

func waitForReady(buf *syncBuffer, stoppedCh chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if readyPattern.MatchString(buf.String()) {
			return nil
		}
		select {
		case <-stoppedCh:
			return fmt.Errorf("foci-gw exited before signalling ready")
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for 'started N agent(s)' log line", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ----- Internal: subprocess build & config --------------------------

// pickFreePort asks the kernel for an ephemeral TCP port and returns it.
// Listener is closed before return — there is a brief race window where
// another process could grab the port, but it's small enough in practice
// for parallel test isolation.
func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	// Look upward from the test's working directory for go.mod with
	// module path "foci".
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for dir := wd; dir != "/"; dir = filepath.Dir(dir) {
		gomod := filepath.Join(dir, "go.mod")
		b, err := os.ReadFile(gomod)
		if err != nil {
			continue
		}
		if firstLine(string(b)) == "module foci" {
			return dir
		}
	}
	t.Fatalf("could not locate foci repo root from %s", wd)
	return ""
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// titleFirst upper-cases the first ASCII letter of s. Used only for synthetic
// bot display names in the stub; agent IDs are single tokens, so this matches
// the old strings.Title behaviour without the deprecated call.
func titleFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func buildBinary(t *testing.T, repoRoot, pkg, outPath string) {
	t.Helper()
	// Compiling a test binary with the go toolchain — not a foci subprocess, so
	// procx.Spawn does not apply.
	cmd := exec.Command("go", "build", "-o", outPath, pkg) //nolint:forbidigo // builds the gw test binary with the go toolchain
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, string(out))
	}
}

// Shared, build-once-per-process binary cache. Every StartGateway used to
// re-link foci-gw + cc-stub into its own t.TempDir; foci-gw links in ~2.6s
// even with a warm cache, so across the ~170 L2 tests that was the suite's
// single biggest cost. The binaries are byte-identical for every test, so we
// build each package exactly once (keyed by package path) into a shared temp
// dir and hand every test the same path.
var (
	sharedBinMu   sync.Mutex
	sharedBinDir  string
	sharedBins    = map[string]string{}
	sharedBinErr  = map[string]error{}
	sharedBinOnce = map[string]*sync.Once{}
)

func sharedBinary(t *testing.T, repoRoot, pkg string) string {
	t.Helper()
	sharedBinMu.Lock()
	if sharedBinDir == "" {
		d, err := testtemp.Mkdir("foci-l2-bin")
		if err != nil {
			sharedBinMu.Unlock()
			t.Fatalf("shared bin dir: %v", err)
		}
		sharedBinDir = d
	}
	once := sharedBinOnce[pkg]
	if once == nil {
		once = &sync.Once{}
		sharedBinOnce[pkg] = once
	}
	sharedBinMu.Unlock()

	// once.Do blocks all concurrent callers until the single build finishes.
	once.Do(func() {
		out := filepath.Join(sharedBinDir, filepath.Base(pkg))
		cmd := exec.Command("go", "build", "-o", out, pkg) //nolint:forbidigo // builds a test binary with the go toolchain
		cmd.Dir = repoRoot
		b, err := cmd.CombinedOutput()
		sharedBinMu.Lock()
		if err != nil {
			sharedBinErr[pkg] = fmt.Errorf("go build %s: %v\n%s", pkg, err, string(b))
		} else {
			sharedBins[pkg] = out
		}
		sharedBinMu.Unlock()
	})

	sharedBinMu.Lock()
	defer sharedBinMu.Unlock()
	if err := sharedBinErr[pkg]; err != nil {
		t.Fatalf("%v", err)
	}
	return sharedBins[pkg]
}

// ----- Internal: synthetic buffer with mutex ------------------------

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func newSyncBuffer() *syncBuffer { return &syncBuffer{} }

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
