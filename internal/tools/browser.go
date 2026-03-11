package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"foci/internal/log"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

type BrowserConfig struct {
	Enabled        bool   `toml:"enabled"`
	Headless       bool   `toml:"headless" default:"true"`
	TimeoutSec     int    `toml:"timeout_sec" default:"30"`
	UserDataDir    string `toml:"user_data_dir"`
	ExecutablePath string `toml:"executable_path"`
	Incognito      bool   `toml:"incognito" default:"true"`
}

type BrowserManager struct {
	mu      sync.Mutex
	browser *rod.Browser
	page    *rod.Page
	config  *BrowserConfig
	logger  *log.ComponentLogger
}

func NewBrowserManager(cfg *BrowserConfig) *BrowserManager {
	return &BrowserManager{
		config: cfg,
		logger: log.NewComponentLogger("browser"),
	}
}

func (m *BrowserManager) IsConnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.browser != nil
}

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
		return fmt.Errorf("failed to launch browser: %w", err)
	}

	m.browser = rod.New().ControlURL(url).MustConnect()
	m.logger.Infof("Browser started (headless=%v, incognito=%v)", m.config.Headless, m.config.Incognito)
	return nil
}

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
			return fmt.Errorf("failed to create page: %w", err)
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

func (m *BrowserManager) withTimeout(page *rod.Page, timeoutMs int) *rod.Page {
	d := m.Timeout()
	if timeoutMs > 0 {
		d = time.Duration(timeoutMs) * time.Millisecond
	}
	return page.Timeout(d)
}

func (m *BrowserManager) Timeout() time.Duration {
	if m.config.TimeoutSec <= 0 {
		return 30 * time.Second
	}
	return time.Duration(m.config.TimeoutSec) * time.Second
}

func NewBrowserTool(mgr *BrowserManager) *Tool {
	description := `Control a browser for web automation using native selectors.

Element selection (use ONE of):
- selector: CSS selector (e.g., "button.submit", "#email", "input[name='user']")
- xpath: XPath expression (e.g., "//button[@type='submit']")
- regex: Match element text content (e.g., "Sign.*In")

Actions:
- start: Start browser
- stop: Stop browser
- navigate: Go to URL (requires targetUrl)
- click: Click element (requires selector/xpath/regex)
- type: Type text into element (requires text + selector/xpath/regex)
- press: Press a key (requires key: Enter, Tab, Escape, Backspace, Delete, Home, End, ArrowUp, ArrowDown, ArrowLeft, ArrowRight, Space, PageUp, PageDown)
- hover: Hover over element
- select: Select option(s) in dropdown (requires values array + selector)
- scroll: Scroll element into view
- evaluate: Run JavaScript (requires script)
- screenshot: Capture screenshot (optional fullPage=true, returnPath=true for file path only)
- pdf: Save page as PDF
- wait: Wait for load/idle (requires waitType: load or idle)
- get_text: Get element text content
- get_attribute: Get element attribute value (requires attribute)
- exists: Check if element exists
- list_elements: List elements on page (optional maxDepth, default 3)

Common patterns:
1. Navigate to page → list_elements to discover structure
2. Interact using selectors → verify with get_text/exists
3. Screenshot for debugging

Timeout: Use timeoutMs parameter (default 30s). Implicit waits are enabled by default.`

	return &Tool{
		Name:        "browser",
		Description: description,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {
					"type": "string",
					"description": "Action to perform",
					"enum": ["start", "stop", "navigate", "click", "type", "press", "hover", "select", "scroll", "evaluate", "screenshot", "pdf", "wait", "get_text", "get_attribute", "exists", "list_elements"]
				},
				"selector": {"type": "string", "description": "CSS selector"},
				"xpath": {"type": "string", "description": "XPath expression"},
				"regex": {"type": "string", "description": "Regex to match element text"},
				"targetUrl": {"type": "string", "description": "URL to navigate to"},
				"text": {"type": "string", "description": "Text to type"},
				"key": {"type": "string", "description": "Key to press"},
				"values": {"type": "array", "items": {"type": "string"}, "description": "Values for select"},
				"attribute": {"type": "string", "description": "Attribute name to get"},
				"script": {"type": "string", "description": "JavaScript to evaluate"},
				"waitType": {"type": "string", "description": "Type of wait (load or idle)"},
				"timeoutMs": {"type": "integer", "description": "Timeout in milliseconds"},
				"fullPage": {"type": "boolean", "description": "Capture full page screenshot"},
				"returnPath": {"type": "boolean", "description": "Return file path instead of base64"},
				"maxDepth": {"type": "integer", "description": "Max depth for list_elements (default 3)"}
			},
			"required": ["action"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return executeBrowserTool(ctx, params, mgr)
		},
	}
}

type browserParams struct {
	Action     string   `json:"action"`
	Selector   string   `json:"selector"`
	XPath      string   `json:"xpath"`
	Regex      string   `json:"regex"`
	TargetURL  string   `json:"targetUrl"`
	Text       string   `json:"text"`
	Key        string   `json:"key"`
	Values     []string `json:"values"`
	Attribute  string   `json:"attribute"`
	Script     string   `json:"script"`
	WaitType   string   `json:"waitType"`
	TimeoutMs  int      `json:"timeoutMs"`
	FullPage   bool     `json:"fullPage"`
	ReturnPath bool     `json:"returnPath"`
	MaxDepth   int      `json:"maxDepth"`
}

func executeBrowserTool(ctx context.Context, params json.RawMessage, mgr *BrowserManager) (ToolResult, error) {
	var p browserParams
	if err := json.Unmarshal(params, &p); err != nil {
		return ToolResult{}, fmt.Errorf("parse params: %w", err)
	}

	switch p.Action {
	case "start":
		return browserStart(mgr, p)
	case "stop":
		return browserStop(mgr, p)
	case "navigate":
		return browserNavigate(mgr, p)
	case "click":
		return browserClick(mgr, p)
	case "type":
		return browserType(mgr, p)
	case "press":
		return browserPress(mgr, p)
	case "hover":
		return browserHover(mgr, p)
	case "select":
		return browserSelect(mgr, p)
	case "scroll":
		return browserScroll(mgr, p)
	case "evaluate":
		return browserEvaluate(mgr, p)
	case "screenshot":
		return browserScreenshot(mgr, p)
	case "pdf":
		return browserPDF(mgr, p)
	case "wait":
		return browserWait(mgr, p)
	case "get_text":
		return browserGetText(mgr, p)
	case "get_attribute":
		return browserGetAttribute(mgr, p)
	case "exists":
		return browserExists(mgr, p)
	case "list_elements":
		return browserListElements(mgr, p)
	default:
		return ToolResult{Text: fmt.Sprintf("Unknown action: %q", p.Action)}, nil
	}
}

