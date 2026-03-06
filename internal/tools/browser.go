package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"foci/internal/log"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
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

func browserStart(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	_ = p // unused but required for function signature consistency
	if err := mgr.Start(); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}
	return ToolResult{Text: "Browser started"}, nil
}

func browserStop(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	_ = p // unused but required for function signature consistency
	if err := mgr.Stop(); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}
	return ToolResult{Text: "Browser stopped"}, nil
}

func browserNavigate(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	if p.TargetURL == "" {
		return ToolResult{Text: "Error: targetUrl required"}, nil
	}

	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	pt := mgr.withTimeout(page, p.TimeoutMs)
	if err := pt.MustNavigate(p.TargetURL); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: navigation failed: %v", err)}, nil
	}
	if err := pt.WaitLoad(); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: wait load failed: %v", err)}, nil
	}

	info, _ := page.Info()
	result := "ok"
	if info != nil {
		result = info.URL
	}
	return ToolResult{Text: result}, nil
}

func browserClick(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	el, err := resolveElement(mgr, p)
	if err != nil {
		return ToolResult{Text: err.Error()}, nil
	}

	if err := el.Click("left", 1); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: click failed: %v", err)}, nil
	}

	return ToolResult{Text: fmt.Sprintf("Clicked: %s", buildSelector(p))}, nil
}

func browserType(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	if p.Text == "" {
		return ToolResult{Text: "Error: text required"}, nil
	}

	el, err := resolveElement(mgr, p)
	if err != nil {
		return ToolResult{Text: err.Error()}, nil
	}

	if err := el.MustInput(p.Text); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: type failed: %v", err)}, nil
	}

	return ToolResult{Text: fmt.Sprintf("Typed: %s", p.Text)}, nil
}

func browserPress(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	if p.Key == "" {
		return ToolResult{Text: "Error: key required"}, nil
	}

	key, ok := keyMap[p.Key]
	if !ok {
		return ToolResult{Text: fmt.Sprintf("Error: unknown key: %q", p.Key)}, nil
	}

	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	page.Keyboard.MustType(key)
	return ToolResult{Text: fmt.Sprintf("Pressed: %s", p.Key)}, nil
}

func browserHover(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	el, err := resolveElement(mgr, p)
	if err != nil {
		return ToolResult{Text: err.Error()}, nil
	}

	if err := el.Hover(); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: hover failed: %v", err)}, nil
	}

	return ToolResult{Text: fmt.Sprintf("Hovered: %s", buildSelector(p))}, nil
}

func browserSelect(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	if len(p.Values) == 0 {
		return ToolResult{Text: "Error: values required"}, nil
	}

	el, err := resolveElement(mgr, p)
	if err != nil {
		return ToolResult{Text: err.Error()}, nil
	}

	if err := el.MustSelect(p.Values...); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: select failed: %v", err)}, nil
	}

	return ToolResult{Text: fmt.Sprintf("Selected: %v", p.Values)}, nil
}

func browserScroll(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	el, err := resolveElement(mgr, p)
	if err != nil {
		return ToolResult{Text: err.Error()}, nil
	}

	if err := el.ScrollIntoView(); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: scroll failed: %v", err)}, nil
	}

	return ToolResult{Text: fmt.Sprintf("Scrolled to: %s", buildSelector(p))}, nil
}

func browserEvaluate(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	if p.Script == "" {
		return ToolResult{Text: "Error: script required"}, nil
	}

	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	pt := mgr.withTimeout(page, p.TimeoutMs)
	result, err := pt.Eval(p.Script)
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: evaluate failed: %v", err)}, nil
	}

	return ToolResult{Text: fmt.Sprintf("Result: %v", result)}, nil
}

func browserScreenshot(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	var buf []byte
	if p.FullPage {
		buf, err = page.ScrollScreenshot(&rod.ScrollScreenshotOptions{})
		if err != nil {
			return ToolResult{Text: fmt.Sprintf("Error: scroll screenshot failed: %v", err)}, nil
		}
	} else {
		pt := mgr.withTimeout(page, p.TimeoutMs)
		buf, err = pt.Screenshot(false, &proto.PageCaptureScreenshot{
			Format: proto.PageCaptureScreenshotFormatPng,
		})
		if err != nil {
			return ToolResult{Text: fmt.Sprintf("Error: screenshot failed: %v", err)}, nil
		}
	}

	path := filepath.Join(os.TempDir(), fmt.Sprintf("browser-screenshot-%d.png", time.Now().Unix()))
	if err := os.WriteFile(path, buf, 0644); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: save screenshot failed: %v", err)}, nil
	}

	if p.ReturnPath {
		return ToolResult{Text: path}, nil
	}

	b64 := base64.StdEncoding.EncodeToString(buf)
	return ToolResult{Text: fmt.Sprintf("Screenshot saved to: %s\n\nBase64:\n%s", path, b64)}, nil
}

func browserPDF(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	_ = p // unused but required for function signature consistency
	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	reader, err := page.PDF(&proto.PagePrintToPDF{})
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: pdf failed: %v", err)}, nil
	}

	buf, err := io.ReadAll(reader)
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: read pdf failed: %v", err)}, nil
	}

	path := filepath.Join(os.TempDir(), fmt.Sprintf("browser-page-%d.pdf", time.Now().Unix()))
	if err := os.WriteFile(path, buf, 0644); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: save pdf failed: %v", err)}, nil
	}

	return ToolResult{Text: fmt.Sprintf("PDF saved to: %s", path)}, nil
}

func browserWait(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	switch p.WaitType {
	case "load":
		if err := page.WaitLoad(); err != nil {
			return ToolResult{Text: fmt.Sprintf("Error: wait load failed: %v", err)}, nil
		}
	case "idle":
		wait := page.MustWaitRequestIdle()
		wait()
	default:
		return ToolResult{Text: fmt.Sprintf("Error: unknown waitType: %q", p.WaitType)}, nil
	}

	return ToolResult{Text: fmt.Sprintf("Wait completed: %s", p.WaitType)}, nil
}

func browserGetText(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	el, err := resolveElement(mgr, p)
	if err != nil {
		return ToolResult{Text: err.Error()}, nil
	}

	text, err := el.Text()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: get text failed: %v", err)}, nil
	}

	return ToolResult{Text: text}, nil
}

func browserGetAttribute(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	if p.Attribute == "" {
		return ToolResult{Text: "Error: attribute required"}, nil
	}

	el, err := resolveElement(mgr, p)
	if err != nil {
		return ToolResult{Text: err.Error()}, nil
	}

	attr, err := el.Attribute(p.Attribute)
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: get attribute failed: %v", err)}, nil
	}

	if attr == nil {
		return ToolResult{Text: "(attribute not set)"}, nil
	}

	return ToolResult{Text: *attr}, nil
}

func browserExists(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	selector := buildSelector(p)
	if selector == "" {
		return ToolResult{Text: "Error: no selector provided"}, nil
	}

	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	has, _, err := mgr.withTimeout(page, p.TimeoutMs).Has(selector)
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	return ToolResult{Text: fmt.Sprintf("%v", has)}, nil
}

func browserListElements(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	selector := buildSelector(p)
	if selector == "" {
		selector = "body"
	}

	maxDepth := p.MaxDepth
	if maxDepth == 0 {
		maxDepth = 3
	}

	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	els, err := mgr.withTimeout(page, p.TimeoutMs).Elements(selector)
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	var lines []string
	for _, el := range els {
		lines = append(lines, formatElement(el, 0, maxDepth)...)
	}

	return ToolResult{Text: strings.Join(lines, "\n")}, nil
}

func resolveElement(mgr *BrowserManager, p browserParams) (*rod.Element, error) {
	selector := buildSelector(p)
	if selector == "" {
		return nil, fmt.Errorf("no selector provided")
	}

	page, err := mgr.getPage()
	if err != nil {
		return nil, err
	}

	el, err := mgr.withTimeout(page, p.TimeoutMs).Element(selector)
	if err != nil {
		return nil, fmt.Errorf("element not found: %v\n\nTry list_elements to see available elements", err)
	}

	return el, nil
}

func buildSelector(p browserParams) string {
	switch {
	case p.Selector != "":
		return p.Selector
	case p.XPath != "":
		return fmt.Sprintf("xpath:%s", p.XPath)
	case p.Regex != "":
		return fmt.Sprintf("regex:%s", p.Regex)
	default:
		return ""
	}
}

func formatElement(el *rod.Element, depth, maxDepth int) []string {
	var lines []string
	indent := strings.Repeat("  ", depth)

	tag, _ := el.Attribute("tagName")
	if tag == nil {
		return lines
	}

	var line string = fmt.Sprintf("%s<%s", indent, strings.ToLower(*tag))

	if id, _ := el.Attribute("id"); id != nil {
		line += fmt.Sprintf(" id=%q", *id)
	}
	if class, _ := el.Attribute("className"); class != nil {
		line += fmt.Sprintf(" class=%q", *class)
	}
	if typ, _ := el.Attribute("type"); typ != nil {
		line += fmt.Sprintf(" type=%q", *typ)
	}
	if name, _ := el.Attribute("name"); name != nil {
		line += fmt.Sprintf(" name=%q", *name)
	}

	line += ">"

	if text, _ := el.Text(); text != "" {
		if len(text) > 100 {
			text = text[:100] + "..."
		}
		line += fmt.Sprintf(" %s", text)
	}

	lines = append(lines, line)

	if depth < maxDepth-1 {
		children, _ := el.Elements("*")
		for _, child := range children {
			lines = append(lines, formatElement(child, depth+1, maxDepth)...)
		}
	}

	return lines
}

var keyMap = map[string]input.Key{
	"Enter":      input.Enter,
	"Tab":        input.Tab,
	"Escape":     input.Escape,
	"Backspace":  input.Backspace,
	"Delete":     input.Delete,
	"Home":       input.Home,
	"End":        input.End,
	"ArrowUp":    input.ArrowUp,
	"ArrowDown":  input.ArrowDown,
	"ArrowLeft":  input.ArrowLeft,
	"ArrowRight": input.ArrowRight,
	"Space":      input.Space,
	"PageUp":     input.PageUp,
	"PageDown":   input.PageDown,
}
