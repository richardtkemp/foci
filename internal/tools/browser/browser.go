package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/procx"
	"foci/internal/tempdir"
	"foci/internal/tools"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// BrowserManager manages a browser instance, page, and snapshot state.
type BrowserManager struct {
	mu         sync.Mutex
	browser    *rod.Browser
	page       *rod.Page
	config     *config.ResolvedBrowser
	logger     *log.ComponentLogger
	snapshot   *Snapshot
	generation int
	FileMode   os.FileMode // permission bits for saved files (screenshots, PDFs)
	incognito  bool        // runtime-toggleable; default true
	profileDir string      // owned temp user-data-dir; created on Start, removed on Stop
	// profile guards the configured persistent UserDataDir. A SessionPool
	// shares one lock across its managers; a standalone manager owns its own.
	profile *profileLock
	// launcher and exited track the chromium process that Start launched, so
	// Stop can wait for it to exit (and kill it if it does not). exited is nil
	// when the exit cannot be watched; Stop then does not wait.
	launcher *launcher.Launcher
	exited   <-chan struct{}
}

// NewBrowserManager creates a new browser manager with the given config.
func NewBrowserManager(cfg *config.ResolvedBrowser, fileMode os.FileMode) *BrowserManager {
	return &BrowserManager{
		config:    cfg,
		FileMode:  fileMode,
		logger:    log.NewComponentLogger("browser"),
		incognito: true,
		profile:   &profileLock{},
	}
}

// IsConnected reports whether the browser is running.
func (m *BrowserManager) IsConnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.browser != nil
}

// baseLauncher builds the go-rod launcher with every flag that does not
// depend on the profile directory. Start adds the user-data-dir.
func (m *BrowserManager) baseLauncher() *launcher.Launcher {
	l := launcher.New().Headless(m.config.Headless)
	if m.config.ExecutablePath != "" {
		l = l.Bin(m.config.ExecutablePath)
	}
	// Always force NoSandbox: go-rod's own default (lib/launcher/launcher.go)
	// only sets it `if inContainer`, a heuristic (checks for /.dockerenv /
	// a container cgroup) that's false on a bare-metal host, so Chrome's
	// setuid-root chrome-sandbox helper is left enabled there by default.
	// That helper requires being able to elevate privileges on execve, which
	// is exactly what prctl(PR_SET_NO_NEW_PRIVS) (set by process sandboxing
	// such as Landlock, foci_todo #1517) forbids — under NO_NEW_PRIVS the
	// setuid path AND Chrome's namespace fallback both fail fatally.
	// Deliberately always-on rather than conditional-on-inContainer/NO_NEW_PRIVS:
	// once the process tree is sealed under Landlock (#1517), that OS-level
	// restriction is the outer sandbox layer and Chrome's own sandbox is
	// redundant defence-in-depth, not the only line of defence — and it must
	// already be forced with no other change for this same launch to work
	// identically whether or not the calling process happens to be sealed.
	l = l.NoSandbox(true)
	// Append, not Set: go-rod's own disabled features share this one flag.
	return l.Append("disable-features", noScreenCaptureFeatures...)
}

// noScreenCaptureFeatures turns off Chromium's WarmScreenCapture
// (ui/base/cocoa/permissions_utils.mm). On macOS, where screen recording is
// allowed, that startup task takes a throwaway 16x10 ScreenCaptureKit
// screenshot. For a Chromium started over SSH, macOS answers each one with a
// "bypass the system private window picker" dialog that is never remembered
// (foci_todo #3222). foci never captures the screen: screenshots come from
// CDP. go-rod pins Chromium 1321438, where WarmScreenCaptureSonoma is the
// switch for every macOS >= 14; newer Chromium also gates macOS >= 15 on
// WarmScreenCaptureSequoia. The features exist only on macOS; other platforms
// ignore the names. Every Chromium foci starts must carry them, the binary
// check in pinnedChromium included.
var noScreenCaptureFeatures = []string{"WarmScreenCaptureSonoma", "WarmScreenCaptureSequoia"}

// pinnedChromium returns go-rod's pinned Chromium, downloading it if absent
// or broken. It replaces go-rod's own lookup, which Launch runs when no binary
// is set: that lookup checks the binary by starting it with fixed flags, and
// a Chromium started without noScreenCaptureFeatures shows the macOS dialog.
// The check here is go-rod's (launcher.Browser.Validate) plus those features.
// When it fails, go-rod's Get repeats its own check, which fails before
// starting a Chromium for a missing binary and cannot reach the warm-up for
// a broken one, then downloads.
func pinnedChromium() (string, error) {
	b := launcher.NewBrowser()
	if err := validateChromium(b.BinPath()); err == nil {
		return b.BinPath(), nil
	}
	return b.Get()
}

// validateChromium returns nil if bin runs as a headless Chromium. It mirrors
// go-rod v0.116.2 launcher.Browser.Validate, with noScreenCaptureFeatures.
func validateChromium(bin string) error {
	if _, err := os.Stat(bin); err != nil {
		return err
	}
	out, err := procx.Spawn(context.Background(), procx.Trusted, bin, validateArgs()...).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "error while loading shared libraries") {
			// go-rod treats a binary missing only OS libraries as valid.
			return nil
		}
		return fmt.Errorf("failed to run the browser: %w\n%s", err, out)
	}
	if !bytes.Contains(out, []byte(`<html><head></head><body></body></html>`)) {
		return fmt.Errorf("the browser executable doesn't support headless mode")
	}
	return nil
}

// validateArgs is the argument list validateChromium starts Chromium with.
func validateArgs() []string {
	return []string{"--headless", "--no-sandbox", "--use-mock-keychain",
		"--disable-dev-shm-usage", "--disable-gpu",
		"--disable-features=" + strings.Join(noScreenCaptureFeatures, ","),
		"--dump-dom", "about:blank"}
}

// Start launches the browser process.
func (m *BrowserManager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.browser != nil {
		return nil
	}

	l := m.baseLauncher()
	if m.config.ExecutablePath == "" {
		bin, err := pinnedChromium()
		if err != nil {
			return fmt.Errorf("launch browser: %w", err)
		}
		l = l.Bin(bin)
	}
	// Always launch with an explicit, isolated user-data-dir. Relying on
	// go-rod's implicit default is unreliable on some hosts (notably the Linux
	// Mint chromium wrapper): the implicit flag fails to take and chromium falls
	// back to the host's real profile (~/.config/chromium). That both risks
	// corrupting the user's actual browser data and causes SingletonLock
	// collisions when two instances run concurrently. A configured persistent
	// dir is honored only for non-incognito sessions that must retain state.
	if m.config.UserDataDir != "" && !m.incognito {
		if err := m.profile.take(m); err != nil {
			return err
		}
		if err := os.MkdirAll(m.config.UserDataDir, 0o700); err != nil {
			m.profile.drop(m)
			return fmt.Errorf("create browser profile dir: %w", err)
		}
		l = l.UserDataDir(m.config.UserDataDir)
	} else {
		dir, err := tempdir.Mkdir("foci-browser-*")
		if err != nil {
			return fmt.Errorf("create browser profile dir: %w", err)
		}
		m.profileDir = dir
		l = l.UserDataDir(dir)
	}

	url, err := l.Launch()
	if err != nil {
		// Launch has already killed a chromium that started but gave no
		// debug URL. Wait for that exit (it is unwatchable if chromium is
		// already gone) so nothing writes into the dir after it is removed.
		_ = m.trackExit(l)
		m.abortStart(true)
		return fmt.Errorf("launch browser: %w", err)
	}
	if err := m.trackExit(l); err != nil {
		m.logger.Warnf("Cannot watch browser pid %d for exit; Stop will not wait for it: %v", l.PID(), err)
	}

	browser := rod.New().ControlURL(url)
	if err := browser.Connect(); err != nil {
		m.abortStart(false)
		return fmt.Errorf("connect to browser: %w", err)
	}
	m.browser = browser
	m.logger.Infof("Browser started (headless=%v, incognito=%v)", m.config.Headless, m.incognito)
	return nil
}

// trackExit records the launcher and opens a watch on its chromium process.
// The watch is opened right after Launch, while the pid is certainly
// chromium's; opened at Stop time (up to an idle TTL later) the pid could
// belong to another process if chromium had crashed in between.
func (m *BrowserManager) trackExit(l *launcher.Launcher) error {
	m.launcher = l
	if l.PID() == 0 {
		return nil // no process was started
	}
	exited, err := watchExit(l.PID())
	m.exited = exited
	return err
}

// abortStart undoes a Start that failed after the launch attempt: it waits
// for chromium to exit (killing it unless shutdown was already requested),
// then removes the owned profile dir and releases the persistent profile.
// Stop cannot do this, because it returns early when m.browser is nil (#2090).
func (m *BrowserManager) abortStart(shutdownRequested bool) {
	m.awaitExit(shutdownRequested)
	m.releaseProfile()
}

// releaseProfile removes the owned temp profile dir, if any, and releases the
// persistent profile lock. Chromium must have exited first (see awaitExit).
func (m *BrowserManager) releaseProfile() {
	if m.profileDir != "" {
		if err := os.RemoveAll(m.profileDir); err != nil {
			m.logger.Warnf("Error removing browser profile dir %s: %v", m.profileDir, err)
		}
		m.profileDir = ""
	}
	m.profile.drop(m)
}

// Stop shuts down the browser.
func (m *BrowserManager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.browser == nil {
		return nil
	}

	closeErr := m.browser.Close()
	if closeErr != nil {
		m.logger.Warnf("Error closing browser: %v", closeErr)
	}
	m.awaitExit(closeErr == nil)
	m.browser = nil
	m.page = nil
	m.snapshot = nil
	m.releaseProfile()
	m.logger.Infof("Browser stopped")
	return nil
}

// awaitExit blocks until the chromium process has exited. CDP Browser.close
// only starts chromium's shutdown, and chromium keeps writing its profile dir
// until it exits, so removing an owned dir earlier fails with "directory not
// empty", and releasing the persistent profile earlier lets another session
// launch into a dir that is still locked (#1518). If the close was not
// acknowledged, or chromium outlives the configured browser operation timeout
// (the bound this manager already puts on one CDP operation), it is killed;
// SIGKILL makes the second wait finite. closed means chromium was already
// asked to shut down (a CDP close, or Launch's own kill on failure).
func (m *BrowserManager) awaitExit(closed bool) {
	exited := m.exited
	l := m.launcher
	m.exited, m.launcher = nil, nil
	if exited == nil {
		return
	}
	if closed {
		timeout := m.Timeout()
		select {
		case <-exited:
			return
		case <-time.After(timeout):
			m.logger.Warnf("Browser did not exit within %s of close; killing it", timeout)
		}
	}
	l.Kill()
	<-exited
}

func (m *BrowserManager) ensureStarted() error {
	if !m.IsConnected() {
		return m.Start()
	}
	return nil
}

func (m *BrowserManager) ensurePage() error {
	if err := m.ensureStarted(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.page == nil {
		page, err := m.browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
		if err != nil {
			return fmt.Errorf("create page: %w", err)
		}
		m.page = page
	}
	return nil
}

func (m *BrowserManager) getPage() (*rod.Page, error) {
	if err := m.ensurePage(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.page, nil
}

// Timeout returns the configured browser timeout.
func (m *BrowserManager) Timeout() time.Duration {
	return tools.ResolveTimeout(m.config.TimeoutSec, tools.TimeoutConfig{DefaultSec: 30})
}

func (m *BrowserManager) withTimeout(page *rod.Page, timeoutMs int) *rod.Page {
	d := m.Timeout()
	if timeoutMs > 0 {
		d = time.Duration(timeoutMs) * time.Millisecond
	}
	return page.Timeout(d)
}

// CaptureSnapshot takes a fresh accessibility tree snapshot of the current page.
func (m *BrowserManager) CaptureSnapshot() (*Snapshot, error) {
	page, err := m.getPage()
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.generation++
	gen := m.generation
	m.mu.Unlock()

	snap, err := BuildSnapshot(page, gen)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.snapshot = snap
	m.mu.Unlock()

	return snap, nil
}

// LatestSnapshot returns the most recent snapshot, or nil.
func (m *BrowserManager) LatestSnapshot() *Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot
}

// WaitDOMStable waits for the DOM to stabilize by comparing page content
// hashes at intervals. Uses config dom_stable_sec and dom_stable_diff.
func (m *BrowserManager) WaitDOMStable(page *rod.Page) {
	interval := m.config.DOMStableSec
	diff := m.config.DOMStableDiff
	_ = page.WaitDOMStable(time.Duration(interval*float64(time.Second)), diff)
}

// ResetSnapshot clears the stored snapshot (e.g., on navigation).
func (m *BrowserManager) ResetSnapshot() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshot = nil
}

// newBrowserTool builds the tool definition using the snapshot/ref paradigm;
// run executes one parsed call against whichever manager the caller picks.
func newBrowserTool(run func(context.Context, browserParams) (tools.ToolResult, error)) *tools.Tool {
	description := `Control a headless browser via accessibility snapshots and element refs. Navigate to URLs, read the snapshot YAML to find [ref=...] locators, then use click/fill/select/press with refs to interact. Each action auto-returns a fresh snapshot. Read the browser skill (SKILL.md) for the full action and parameter reference.`

	return &tools.Tool{
		Name:        "browser",
		ExecExport:  true,
		Positional:  []string{"action"},
		Description: description,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {
					"type": "string",
					"description": "Action to perform",
					"enum": ["start", "snapshot", "navigate", "click", "fill", "select", "press", "go_back", "go_forward", "reload", "screenshot", "pdf", "evaluate", "wait", "close"]
				},
				"url": {"type": "string", "description": "URL to navigate to"},
				"ref": {"type": "string", "description": "Element ref from snapshot (e.g., s1e5)"},
				"element": {"type": "string", "description": "Human-readable element description (for logging)"},
				"value": {"type": "string", "description": "Value to fill into input"},
				"fields": {"type": "array", "items": {"type": "object", "properties": {"ref": {"type": "string"}, "value": {"type": "string"}}, "required": ["ref", "value"]}, "description": "Multiple fields to fill at once: [{ref, value}, ...]. Takes one snapshot at the end."},
				"values": {"type": "array", "items": {"type": "string"}, "description": "Values for select"},
				"submit": {"type": "boolean", "description": "Press Enter after filling"},
				"key": {"type": "string", "description": "Key to press (Enter, Tab, Escape, etc.)"},
				"script": {"type": "string", "description": "JavaScript to evaluate: one expression (document.title, JSON.stringify(x), an IIFE) or a function expression (() => …, async () => …). A function is called and its result returned; promises are awaited. For statements, wrap them: (() => { …; return x })()"},
				"waitType": {"type": "string", "description": "Wait type: load or idle"},
				"fullPage": {"type": "boolean", "description": "Capture full page screenshot"},
				"returnPath": {"type": "boolean", "description": "Return file path instead of base64"},
				"incognito": {"type": "boolean", "description": "Use incognito mode (default true). Only applies to start action."}
			},
			"required": ["action"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			var p browserParams
			if err := json.Unmarshal(params, &p); err != nil {
				return tools.ToolResult{}, fmt.Errorf("parse params: %w", err)
			}
			return run(ctx, p)
		},
	}
}

// fillField is a single ref+value pair for multi-fill operations.
type fillField struct {
	Ref   string `json:"ref"`
	Value string `json:"value"`
}

type browserParams struct {
	Action    string      `json:"action"`
	URL       string      `json:"url"`
	Ref       string      `json:"ref"`
	Element   string      `json:"element"`
	Value     string      `json:"value"`
	Values    []string    `json:"values"`
	Fields    []fillField `json:"fields"`
	Submit    bool        `json:"submit"`
	Key       string      `json:"key"`
	Script    string      `json:"script"`
	WaitType  string      `json:"waitType"`
	FullPage  bool        `json:"fullPage"`
	RetPath   bool        `json:"returnPath"`
	Incognito *bool       `json:"incognito"`
}

func dispatchBrowserAction(mgr *BrowserManager, p browserParams) (tools.ToolResult, error) {
	switch p.Action {
	case "start":
		return browserStart(mgr, p)
	case "snapshot":
		return browserSnapshot(mgr)
	case "navigate":
		return browserNavigate(mgr, p)
	case "click":
		return browserClick(mgr, p)
	case "fill":
		return browserFill(mgr, p)
	case "select":
		return browserSelect(mgr, p)
	case "press":
		return browserPress(mgr, p)
	case "go_back":
		return browserGoBack(mgr)
	case "go_forward":
		return browserGoForward(mgr)
	case "reload":
		return browserReload(mgr)
	case "screenshot":
		return browserScreenshot(mgr, p)
	case "pdf":
		return browserPDF(mgr)
	case "evaluate":
		return browserEvaluate(mgr, p)
	case "wait":
		return browserWait(mgr, p)
	case "close":
		return browserClose(mgr)
	default:
		return tools.ToolResult{}, fmt.Errorf("unknown action: %q", p.Action)
	}
}
