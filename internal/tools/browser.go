package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/log"

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
}

// NewBrowserManager creates a new browser manager with the given config.
func NewBrowserManager(cfg *config.ResolvedBrowser, fileMode os.FileMode) *BrowserManager {
	return &BrowserManager{
		config:   cfg,
		FileMode: fileMode,
		logger: log.NewComponentLogger("browser"),
	}
}

// IsConnected reports whether the browser is running.
func (m *BrowserManager) IsConnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.browser != nil
}

// Start launches the browser process.
func (m *BrowserManager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.browser != nil {
		return nil
	}

	l := launcher.New().Headless(m.config.Headless)
	if m.config.ExecutablePath != "" {
		l = l.Bin(m.config.ExecutablePath)
	}
	if m.config.UserDataDir != "" && !m.config.Incognito {
		l = l.UserDataDir(m.config.UserDataDir)
	}

	url, err := l.Launch()
	if err != nil {
		return fmt.Errorf("launch browser: %w", err)
	}

	m.browser = rod.New().ControlURL(url).MustConnect()
	m.logger.Infof("Browser started (headless=%v, incognito=%v)", m.config.Headless, m.config.Incognito)
	return nil
}

// Stop shuts down the browser.
func (m *BrowserManager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.browser == nil {
		return nil
	}

	if err := m.browser.Close(); err != nil {
		m.logger.Warnf("Error closing browser: %v", err)
	}
	m.browser = nil
	m.page = nil
	m.snapshot = nil
	m.logger.Infof("Browser stopped")
	return nil
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
	return ResolveTimeout(m.config.TimeoutSec, TimeoutConfig{DefaultSec: 30})
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

// NewBrowserTool creates the browser tool definition using snapshot/ref paradigm.
func NewBrowserTool(mgr *BrowserManager) *Tool {
	description := `Control a headless browser via accessibility snapshots and element refs. Navigate to URLs, read the snapshot YAML to find [ref=...] locators, then use click/fill/select/press with refs to interact. Each action auto-returns a fresh snapshot. Read the browser skill (SKILL.md) for the full action and parameter reference.`

	return &Tool{
		Name:        "browser",
		Description: description,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {
					"type": "string",
					"description": "Action to perform",
					"enum": ["snapshot", "navigate", "click", "fill", "select", "press", "go_back", "go_forward", "reload", "screenshot", "pdf", "evaluate", "wait", "close"]
				},
				"url": {"type": "string", "description": "URL to navigate to"},
				"ref": {"type": "string", "description": "Element ref from snapshot (e.g., s1e5)"},
				"element": {"type": "string", "description": "Human-readable element description (for logging)"},
				"value": {"type": "string", "description": "Value to fill into input"},
				"fields": {"type": "array", "items": {"type": "object", "properties": {"ref": {"type": "string"}, "value": {"type": "string"}}, "required": ["ref", "value"]}, "description": "Multiple fields to fill at once: [{ref, value}, ...]. Takes one snapshot at the end."},
				"values": {"type": "array", "items": {"type": "string"}, "description": "Values for select"},
				"submit": {"type": "boolean", "description": "Press Enter after filling"},
				"key": {"type": "string", "description": "Key to press (Enter, Tab, Escape, etc.)"},
				"script": {"type": "string", "description": "JavaScript to evaluate"},
				"waitType": {"type": "string", "description": "Wait type: load or idle"},
				"fullPage": {"type": "boolean", "description": "Capture full page screenshot"},
				"returnPath": {"type": "boolean", "description": "Return file path instead of base64"}
			},
			"required": ["action"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return executeBrowserTool(ctx, params, mgr)
		},
	}
}

// fillField is a single ref+value pair for multi-fill operations.
type fillField struct {
	Ref   string `json:"ref"`
	Value string `json:"value"`
}

type browserParams struct {
	Action   string      `json:"action"`
	URL      string      `json:"url"`
	Ref      string      `json:"ref"`
	Element  string      `json:"element"`
	Value    string      `json:"value"`
	Values   []string    `json:"values"`
	Fields   []fillField `json:"fields"`
	Submit   bool        `json:"submit"`
	Key      string      `json:"key"`
	Script   string      `json:"script"`
	WaitType string      `json:"waitType"`
	FullPage bool        `json:"fullPage"`
	RetPath  bool        `json:"returnPath"`
}

func executeBrowserTool(ctx context.Context, params json.RawMessage, mgr *BrowserManager) (ToolResult, error) {
	var p browserParams
	if err := json.Unmarshal(params, &p); err != nil {
		return ToolResult{}, fmt.Errorf("parse params: %w", err)
	}

	switch p.Action {
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
		return ToolResult{Text: fmt.Sprintf("Unknown action: %q", p.Action)}, nil
	}
}
