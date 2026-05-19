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
}

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
}

// Harness drives one foci-gw subprocess against a Telegram stub. Tests
// access the stub via TelegramStub() and the cc-stub recorder file via
// RecorderPath(); they push synthetic updates and assert on what foci
// did to the recorder.
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

	// Build foci-gw + cc-stub. go build is cheap when sources are
	// unchanged thanks to Go's build cache; expensive only on the first
	// test of a fresh checkout.
	repoRoot := findRepoRoot(t)
	gwBin := filepath.Join(binDir, "foci-gw")
	stubBin := filepath.Join(binDir, "cc-stub")
	buildBinary(t, repoRoot, "./cmd/foci-gw", gwBin)
	// cc-stub is built unconditionally so the cached binary is available
	// even when ClaudeBinary overrides where foci-gw looks for it; tests
	// that swap the binary mid-flight rely on having a known-good stub
	// in opts (via h.ScriptDir()/etc.).
	buildBinary(t, repoRoot, "./cmd/cc-stub", stubBin)
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
			FirstName: strings.Title(a.ID) + "Bot",
			Username:  a.ID + "_bot",
		})
	}

	configPath := filepath.Join(tempDir, "foci.toml")
	secretsPath := filepath.Join(tempDir, "secrets.toml")
	workspaces := writeWorkspaces(t, tempDir, opts.Agents)

	// Pick an ephemeral HTTP port so parallel tests don't all collide on the
	// default 18791. Brief race window between Close and foci-gw's bind, but
	// acceptable for test purposes — kernel rarely reuses an ephemeral port
	// that fast.
	httpPort, err := pickFreePort()
	if err != nil {
		return nil, fmt.Errorf("pick free port: %w", err)
	}

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
		ExtraConfigTOML: opts.ExtraConfigTOML,
	})
	if !opts.SkipSecretsFile {
		writeTestSecrets(t, secretsPath, opts.Agents, opts.ExtraSecretsTOML)
	}

	// Spawn foci-gw.
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, gwBin, "-config", configPath)
	cmd.Env = append(os.Environ(),
		"CCSTUB_RECORDER="+recorderPath,
		"CCSTUB_SCRIPT_DIR="+scriptDir,
		// Pin the early log-init path to the test tempdir so foci-gw
		// doesn't open the host's production ~/logs/foci.log before
		// config load. See cmd/foci-gw/main.go:81 (FOCI_LOG_FILE).
		"FOCI_LOG_FILE="+filepath.Join(logsDir, "foci.log"),
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start foci-gw: %w", err)
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
		agents:       opts.Agents,
		cmd:          cmd,
		stderrBuf:    stderrBuf,
		stoppedCh:    stoppedCh,
	}

	t.Cleanup(func() {
		// Try graceful, then force.
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-stoppedCh:
		case <-time.After(3 * time.Second):
			cancel()
			<-stoppedCh
		}
		if opts.LogTail || t.Failed() {
			t.Logf("foci-gw stderr:\n%s", stderrBuf.String())
		}
	})

	if err := waitForReady(stderrBuf, stoppedCh, opts.ReadyTimeout); err != nil {
		// On not-ready, return both the error and the captured stderr
		// so callers (including StartGateway's t.Fatalf wrapper) can
		// surface useful debug info without re-grabbing it.
		return nil, fmt.Errorf("foci-gw not ready: %w\n--- stderr ---\n%s", err, stderrBuf.String())
	}
	return h, nil
}

// TelegramStub returns the underlying Bot API stub so tests can push
// updates and inspect outbound calls.
func (h *Harness) TelegramStub() *TelegramStub { return h.tgStub }

// RecorderPath returns the path to the cc-stub invocation recorder file.
// Tests read this file (JSONL) to assert on what foci handed CC.
func (h *Harness) RecorderPath() string { return h.recorderPath }

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

func buildBinary(t *testing.T, repoRoot, pkg, outPath string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", outPath, pkg)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, string(out))
	}
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

// ----- Internal: stderr scanner helper -------------------------------
// (Not currently used directly; kept here for tests that want to wait
// on arbitrary log lines beyond the ready signal.)
func scanLines(r io.Reader, onLine func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		onLine(sc.Text())
	}
}
