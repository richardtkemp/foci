package tools

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

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

// resolveElement finds an element using the selector from browserParams.
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

// buildSelector returns the appropriate selector string from browserParams.
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
