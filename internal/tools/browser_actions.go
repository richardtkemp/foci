package tools

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"foci/internal/tools/browserjs"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
	"gopkg.in/yaml.v3"
)

// withAutoSnapshot captures a fresh snapshot after an action and appends it
// to the result text. This is the key UX pattern: the agent always sees
// the page state after each interaction.
func withAutoSnapshot(mgr *BrowserManager, result string) ToolResult {
	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: result + "\n\n(auto-snapshot failed: " + err.Error() + ")"}
	}

	mgr.WaitDOMStable(page)

	snap, err := mgr.CaptureSnapshot()
	if err != nil {
		return ToolResult{Text: result + "\n\n(auto-snapshot failed: " + err.Error() + ")"}
	}

	return ToolResult{Text: result + "\n\n" + snap.String()}
}

// withScopedSnapshot captures a snapshot scoped to the form context around the
// given refs, rather than the full page. Falls back to a full snapshot if
// scoping fails. This saves tokens on large pages after fill actions.
func withScopedSnapshot(mgr *BrowserManager, result string, refs []string) ToolResult {
	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: result + "\n\n(auto-snapshot failed: " + err.Error() + ")"}
	}

	mgr.WaitDOMStable(page)

	snap := mgr.LatestSnapshot()
	if snap == nil || len(refs) == 0 {
		return withAutoSnapshot(mgr, result)
	}

	// Resolve the first filled element to find its form context
	el, err := snap.LocatorInFrame(refs[0])
	if err != nil {
		return withAutoSnapshot(mgr, result)
	}

	scopedText, err := buildScopedSnapshot(page, el, mgr)
	if err != nil {
		return withAutoSnapshot(mgr, result)
	}

	return ToolResult{Text: result + "\n\n" + scopedText}
}

// buildScopedSnapshot captures an ARIA snapshot rooted at the closest <form>
// ancestor of el. If no form is found, walks up 3 levels as a fallback.
// Returns the formatted snapshot text.
func buildScopedSnapshot(page *rod.Page, el *rod.Element, mgr *BrowserManager) (string, error) {
	// Inject snapshot JS and capture scoped snapshot
	if err := injectSnapshotJS(page); err != nil {
		return "", fmt.Errorf("inject snapshot JS: %w", err)
	}

	// Find the closest <form> ancestor (or walk up 3 levels) and store
	// it on window so AriaSnapshot can reference it.
	if _, err := el.Eval(storeScopeRootJS); err != nil {
		return "", fmt.Errorf("store scope root: %w", err)
	}

	mgr.mu.Lock()
	mgr.generation++
	gen := mgr.generation
	mgr.mu.Unlock()

	snapResult, err := page.Eval(browserjs.AriaSnapshot, "window.__fociScopeRoot", "({ref: true})")
	if err != nil {
		return "", fmt.Errorf("capture scoped snapshot: %w", err)
	}

	var snapNode yaml.Node
	if err := yaml.Unmarshal([]byte(snapResult.Value.String()), &snapNode); err != nil {
		return "", fmt.Errorf("unmarshal scoped snapshot: %w", err)
	}

	yamlBytes, err := yaml.Marshal(&snapNode)
	if err != nil {
		return "", fmt.Errorf("marshal scoped snapshot: %w", err)
	}

	info, err := page.Info()
	if err != nil {
		return "", fmt.Errorf("get page info: %w", err)
	}

	// Store as the latest snapshot so refs from it can be used
	snap := &Snapshot{
		frames:     []*rod.Page{page},
		generation: gen,
	}
	lang := snapshotLang(page, yamlBytes)

	var b strings.Builder
	fmt.Fprintf(&b, "- Page URL: %s\n", info.URL)
	fmt.Fprintf(&b, "- Page Title: %s\n", info.Title)
	b.WriteString("- Form Context Snapshot (scoped to form around filled fields)\n")
	fmt.Fprintf(&b, "```%s\n", lang)
	b.WriteString(strings.TrimSpace(string(yamlBytes)))
	b.WriteString("\n```\n")

	snap.text = b.String()

	mgr.mu.Lock()
	mgr.snapshot = snap
	mgr.mu.Unlock()

	return snap.text, nil
}

// storeScopeRootJS finds the closest <form> ancestor of the element (or walks
// up 3 levels as fallback) and stores it on window for AriaSnapshot to use.
const storeScopeRootJS = `function() {
	let form = this.closest('form');
	if (form) { window.__fociScopeRoot = form; return; }
	let node = this;
	for (let i = 0; i < 3 && node.parentElement; i++) {
		node = node.parentElement;
	}
	window.__fociScopeRoot = node;
}`

// resolveRef validates and resolves a ref from the current snapshot to a rod.Element.
func resolveRef(mgr *BrowserManager, ref string) (*rod.Element, error) {
	if ref == "" {
		return nil, fmt.Errorf("ref required — use a [ref=...] value from the snapshot")
	}
	if err := ParseRef(ref); err != nil {
		return nil, err
	}
	snap := mgr.LatestSnapshot()
	if snap == nil {
		return nil, fmt.Errorf("no snapshot available — use 'snapshot' or 'navigate' first")
	}
	return snap.LocatorInFrame(ref)
}

func browserSnapshot(mgr *BrowserManager) (ToolResult, error) {
	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	mgr.WaitDOMStable(page)

	snap, err := mgr.CaptureSnapshot()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error capturing snapshot: %v", err)}, nil
	}

	return ToolResult{Text: snap.String()}, nil
}

func browserNavigate(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	if p.URL == "" {
		return ToolResult{Text: "Error: url required"}, nil
	}

	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	mgr.ResetSnapshot()

	pt := mgr.withTimeout(page, 0)
	if err := pt.Navigate(p.URL); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: navigation failed: %v", err)}, nil
	}
	if err := pt.WaitLoad(); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: wait load failed: %v", err)}, nil
	}

	return withAutoSnapshot(mgr, fmt.Sprintf("Navigated to %s", p.URL)), nil
}

func browserClick(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	el, err := resolveRef(mgr, p.Ref)
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	desc := p.Element
	if desc == "" {
		desc = p.Ref
	}

	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: click failed: %v", err)}, nil
	}

	return withAutoSnapshot(mgr, fmt.Sprintf("Clicked: %s", desc)), nil
}

func browserFill(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	// Build the list of fields to fill — either from "fields" array or
	// single ref+value for backward compatibility.
	fields := p.Fields
	if len(fields) == 0 {
		if p.Ref == "" {
			return ToolResult{Text: "Error: ref or fields required"}, nil
		}
		fields = []fillField{{Ref: p.Ref, Value: p.Value}}
	}

	var filledRefs []string
	var descriptions []string

	for _, f := range fields {
		el, err := resolveRef(mgr, f.Ref)
		if err != nil {
			return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
		}

		// Clear existing value then type new one
		if err := el.SelectAllText(); err != nil {
			return ToolResult{Text: fmt.Sprintf("Error: select text failed for %s: %v", f.Ref, err)}, nil
		}
		if err := el.Input(f.Value); err != nil {
			return ToolResult{Text: fmt.Sprintf("Error: fill failed for %s: %v", f.Ref, err)}, nil
		}

		filledRefs = append(filledRefs, f.Ref)
		descriptions = append(descriptions, fmt.Sprintf("%s=%q", f.Ref, f.Value))
	}

	desc := p.Element
	if desc == "" {
		desc = strings.Join(descriptions, ", ")
	}
	result := fmt.Sprintf("Filled %s", desc)

	if p.Submit {
		page, err := mgr.getPage()
		if err != nil {
			return ToolResult{Text: fmt.Sprintf("Error getting page for submit: %v", err)}, nil
		}
		if err := page.Keyboard.Press(input.Enter); err != nil {
			return ToolResult{Text: fmt.Sprintf("Error: submit (Enter) failed: %v", err)}, nil
		}
		result += " and submitted"
	}

	return withScopedSnapshot(mgr, result, filledRefs), nil
}

func browserSelect(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	if len(p.Values) == 0 {
		return ToolResult{Text: "Error: values required"}, nil
	}

	el, err := resolveRef(mgr, p.Ref)
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	desc := p.Element
	if desc == "" {
		desc = p.Ref
	}

	if err := el.Select(p.Values, true, rod.SelectorTypeText); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: select failed: %v", err)}, nil
	}

	return withAutoSnapshot(mgr, fmt.Sprintf("Selected %v in %s", p.Values, desc)), nil
}

func browserPress(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	if p.Key == "" {
		return ToolResult{Text: "Error: key required"}, nil
	}

	key, ok := keyMap[p.Key]
	if !ok {
		return ToolResult{Text: fmt.Sprintf("Error: unknown key %q — valid keys: Enter, Tab, Escape, Backspace, Delete, Home, End, ArrowUp, ArrowDown, ArrowLeft, ArrowRight, Space, PageUp, PageDown", p.Key)}, nil
	}

	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	if err := page.Keyboard.Press(key); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: press failed: %v", err)}, nil
	}

	return ToolResult{Text: fmt.Sprintf("Pressed: %s", p.Key)}, nil
}

func browserGoBack(mgr *BrowserManager) (ToolResult, error) {
	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	mgr.ResetSnapshot()
	if err := page.NavigateBack(); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: go back failed: %v", err)}, nil
	}
	if err := page.WaitLoad(); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: wait load failed: %v", err)}, nil
	}

	return withAutoSnapshot(mgr, "Navigated back"), nil
}

func browserGoForward(mgr *BrowserManager) (ToolResult, error) {
	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	mgr.ResetSnapshot()
	if err := page.NavigateForward(); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: go forward failed: %v", err)}, nil
	}
	if err := page.WaitLoad(); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: wait load failed: %v", err)}, nil
	}

	return withAutoSnapshot(mgr, "Navigated forward"), nil
}

func browserReload(mgr *BrowserManager) (ToolResult, error) {
	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	mgr.ResetSnapshot()
	if err := page.Reload(); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: reload failed: %v", err)}, nil
	}
	if err := page.WaitLoad(); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: wait load failed: %v", err)}, nil
	}

	return withAutoSnapshot(mgr, "Reloaded page"), nil
}

func browserScreenshot(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	var buf []byte
	if p.FullPage {
		buf, err = page.ScrollScreenshot(nil)
		if err != nil {
			return ToolResult{Text: fmt.Sprintf("Error: scroll screenshot failed: %v", err)}, nil
		}
	} else {
		buf, err = page.Screenshot(false, &proto.PageCaptureScreenshot{
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

	if p.RetPath {
		return ToolResult{Text: path}, nil
	}

	b64 := base64.StdEncoding.EncodeToString(buf)
	return ToolResult{Text: fmt.Sprintf("Screenshot saved to: %s\n\nBase64:\n%s", path, b64)}, nil
}

func browserPDF(mgr *BrowserManager) (ToolResult, error) {
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

func browserEvaluate(mgr *BrowserManager, p browserParams) (ToolResult, error) {
	if p.Script == "" {
		return ToolResult{Text: "Error: script required"}, nil
	}

	page, err := mgr.getPage()
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}

	result, err := page.Eval(p.Script)
	if err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: evaluate failed: %v", err)}, nil
	}

	return ToolResult{Text: fmt.Sprintf("Result: %v", result.Value)}, nil
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
		return ToolResult{Text: fmt.Sprintf("Error: unknown waitType %q — use load or idle", p.WaitType)}, nil
	}

	return ToolResult{Text: fmt.Sprintf("Wait completed: %s", p.WaitType)}, nil
}

func browserClose(mgr *BrowserManager) (ToolResult, error) {
	if err := mgr.Stop(); err != nil {
		return ToolResult{Text: fmt.Sprintf("Error: %v", err)}, nil
	}
	return ToolResult{Text: "Browser closed"}, nil
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

// Ensure rod import is used (resolveRef returns *rod.Element).
var _ *rod.Element
