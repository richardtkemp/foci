package browser

import (
	"encoding/base64"
	"errors"
	"fmt"
	"foci/internal/tools"
	"io"
	"os"
	"strings"

	"foci/internal/tempdir"
	"foci/internal/tools/browserjs"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
	"gopkg.in/yaml.v3"
)

// withAutoSnapshot captures a fresh snapshot after an action and appends it
// to the result text. This is the key UX pattern: the agent always sees
// the page state after each interaction.
func withAutoSnapshot(mgr *BrowserManager, result string) tools.ToolResult {
	page, err := mgr.getPage()
	if err != nil {
		return tools.ToolResult{Text: result + "\n\n(auto-snapshot failed: " + err.Error() + ")"}
	}

	mgr.WaitDOMStable(page)

	snap, err := mgr.CaptureSnapshot()
	if err != nil {
		return tools.ToolResult{Text: result + "\n\n(auto-snapshot failed: " + err.Error() + ")"}
	}

	return tools.ToolResult{Text: result + "\n\n" + snap.String()}
}

// withScopedSnapshot captures a snapshot scoped to the form context around the
// given refs, rather than the full page. Falls back to a full snapshot if
// scoping fails. This saves tokens on large pages after fill actions.
func withScopedSnapshot(mgr *BrowserManager, result string, refs []string) tools.ToolResult {
	page, err := mgr.getPage()
	if err != nil {
		return tools.ToolResult{Text: result + "\n\n(auto-snapshot failed: " + err.Error() + ")"}
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

	scopedText, err := buildScopedSnapshot(page, el)
	if err != nil {
		return withAutoSnapshot(mgr, result)
	}

	return tools.ToolResult{Text: result + "\n\n" + scopedText}
}

// buildScopedSnapshot captures an ARIA snapshot rooted at the closest <form>
// ancestor of el, reading DOM-stamped refs from the prior full snapshot instead
// of generating new positional IDs. This preserves ref stability — the agent
// can keep using refs from the full snapshot after a scoped fill.
// If no form is found, walks up 3 levels as a fallback.
func buildScopedSnapshot(page *rod.Page, el *rod.Element) (string, error) {
	if err := injectSnapshotJS(page); err != nil {
		return "", fmt.Errorf("inject snapshot JS: %w", err)
	}

	// Find the closest <form> ancestor (or walk up 3 levels) and store
	// it on window so ScopedAriaSnapshot can reference it.
	if _, err := el.Eval(storeScopeRootJS); err != nil {
		return "", fmt.Errorf("store scope root: %w", err)
	}

	snapResult, err := page.Eval(browserjs.ScopedAriaSnapshot, "window.__fociScopeRoot", "({ref: true})")
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

	lang := snapshotLang(page, yamlBytes)

	var b strings.Builder
	fmt.Fprintf(&b, "- Page URL: %s\n", info.URL)
	fmt.Fprintf(&b, "- Page Title: %s\n", info.Title)
	b.WriteString("- Form Context Snapshot (scoped to form around filled fields)\n")
	fmt.Fprintf(&b, "```%s\n", lang)
	b.WriteString(strings.TrimSpace(string(yamlBytes)))
	b.WriteString("\n```\n")

	return b.String(), nil
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

func browserSnapshot(mgr *BrowserManager) (tools.ToolResult, error) {
	page, err := mgr.getPage()
	if err != nil {
		return tools.ToolResult{}, err
	}

	mgr.WaitDOMStable(page)

	snap, err := mgr.CaptureSnapshot()
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("capturing snapshot: %w", err)
	}

	return tools.ToolResult{Text: snap.String()}, nil
}

func browserNavigate(mgr *BrowserManager, p browserParams) (tools.ToolResult, error) {
	if p.URL == "" {
		return tools.ToolResult{}, errors.New("url required")
	}

	page, err := mgr.getPage()
	if err != nil {
		return tools.ToolResult{}, err
	}

	mgr.ResetSnapshot()

	pt := mgr.withTimeout(page, 0)
	if err := pt.Navigate(p.URL); err != nil {
		return tools.ToolResult{}, fmt.Errorf("navigation failed: %w", err)
	}
	if err := pt.WaitLoad(); err != nil {
		return tools.ToolResult{}, fmt.Errorf("wait load failed: %w", err)
	}

	return withAutoSnapshot(mgr, fmt.Sprintf("Navigated to %s", p.URL)), nil
}

func browserClick(mgr *BrowserManager, p browserParams) (tools.ToolResult, error) {
	el, err := resolveRef(mgr, p.Ref)
	if err != nil {
		return tools.ToolResult{}, err
	}

	desc := p.Element
	if desc == "" {
		desc = p.Ref
	}

	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return tools.ToolResult{}, fmt.Errorf("click failed: %w", err)
	}

	return withAutoSnapshot(mgr, fmt.Sprintf("Clicked: %s", desc)), nil
}

func browserFill(mgr *BrowserManager, p browserParams) (tools.ToolResult, error) {
	// Build the list of fields to fill — either from "fields" array or
	// single ref+value for backward compatibility.
	fields := p.Fields
	if len(fields) == 0 {
		if p.Ref == "" {
			return tools.ToolResult{}, errors.New("ref or fields required")
		}
		fields = []fillField{{Ref: p.Ref, Value: p.Value}}
	}

	var filledRefs []string
	var descriptions []string

	for _, f := range fields {
		el, err := resolveRef(mgr, f.Ref)
		if err != nil {
			return tools.ToolResult{}, err
		}

		// Clear existing value then type new one
		if err := el.SelectAllText(); err != nil {
			return tools.ToolResult{}, fmt.Errorf("select text failed for %s: %w", f.Ref, err)
		}
		if err := el.Input(f.Value); err != nil {
			return tools.ToolResult{}, fmt.Errorf("fill failed for %s: %w", f.Ref, err)
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
			return tools.ToolResult{}, fmt.Errorf("getting page for submit: %w", err)
		}
		if err := page.Keyboard.Press(input.Enter); err != nil {
			return tools.ToolResult{}, fmt.Errorf("submit (Enter) failed: %w", err)
		}
		result += " and submitted"
	}

	return withScopedSnapshot(mgr, result, filledRefs), nil
}

func browserSelect(mgr *BrowserManager, p browserParams) (tools.ToolResult, error) {
	if len(p.Values) == 0 {
		return tools.ToolResult{}, errors.New("values required")
	}

	el, err := resolveRef(mgr, p.Ref)
	if err != nil {
		return tools.ToolResult{}, err
	}

	desc := p.Element
	if desc == "" {
		desc = p.Ref
	}

	if err := el.Select(p.Values, true, rod.SelectorTypeText); err != nil {
		return tools.ToolResult{}, fmt.Errorf("select failed: %w", err)
	}

	return withAutoSnapshot(mgr, fmt.Sprintf("Selected %v in %s", p.Values, desc)), nil
}

func browserPress(mgr *BrowserManager, p browserParams) (tools.ToolResult, error) {
	if p.Key == "" {
		return tools.ToolResult{}, errors.New("key required")
	}

	key, ok := keyMap[p.Key]
	if !ok {
		return tools.ToolResult{}, fmt.Errorf("unknown key %q — valid keys: Enter, Tab, Escape, Backspace, Delete, Home, End, ArrowUp, ArrowDown, ArrowLeft, ArrowRight, Space, PageUp, PageDown", p.Key)
	}

	page, err := mgr.getPage()
	if err != nil {
		return tools.ToolResult{}, err
	}

	if err := page.Keyboard.Press(key); err != nil {
		return tools.ToolResult{}, fmt.Errorf("press failed: %w", err)
	}

	return tools.ToolResult{Text: fmt.Sprintf("Pressed: %s", p.Key)}, nil
}

// browserNav runs a history/reload navigation with the shared sequence:
// get page → reset snapshot → run the action → wait for load → auto-snapshot.
// errVerb names the action for the failure message; successMsg labels the result.
func browserNav(mgr *BrowserManager, errVerb, successMsg string, action func(*rod.Page) error) (tools.ToolResult, error) {
	page, err := mgr.getPage()
	if err != nil {
		return tools.ToolResult{}, err
	}

	mgr.ResetSnapshot()
	if err := action(page); err != nil {
		return tools.ToolResult{}, fmt.Errorf("%s failed: %w", errVerb, err)
	}
	if err := page.WaitLoad(); err != nil {
		return tools.ToolResult{}, fmt.Errorf("wait load failed: %w", err)
	}

	return withAutoSnapshot(mgr, successMsg), nil
}

func browserGoBack(mgr *BrowserManager) (tools.ToolResult, error) {
	return browserNav(mgr, "go back", "Navigated back", (*rod.Page).NavigateBack)
}

func browserGoForward(mgr *BrowserManager) (tools.ToolResult, error) {
	return browserNav(mgr, "go forward", "Navigated forward", (*rod.Page).NavigateForward)
}

func browserReload(mgr *BrowserManager) (tools.ToolResult, error) {
	return browserNav(mgr, "reload", "Reloaded page", (*rod.Page).Reload)
}

func browserScreenshot(mgr *BrowserManager, p browserParams) (tools.ToolResult, error) {
	page, err := mgr.getPage()
	if err != nil {
		return tools.ToolResult{}, err
	}

	var buf []byte
	if p.FullPage {
		buf, err = page.ScrollScreenshot(nil)
		if err != nil {
			return tools.ToolResult{}, fmt.Errorf("scroll screenshot failed: %w", err)
		}
	} else {
		buf, err = page.Screenshot(false, &proto.PageCaptureScreenshot{
			Format: proto.PageCaptureScreenshotFormatPng,
		})
		if err != nil {
			return tools.ToolResult{}, fmt.Errorf("screenshot failed: %w", err)
		}
	}

	path, err := writeTempFileExcl("browser-screenshot-*.png", buf, mgr.FileMode)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("save screenshot failed: %w", err)
	}

	if p.RetPath {
		return tools.ToolResult{Text: path}, nil
	}

	b64 := base64.StdEncoding.EncodeToString(buf)
	return tools.ToolResult{Text: fmt.Sprintf("Screenshot saved to: %s\n\nBase64:\n%s", path, b64)}, nil
}

func browserPDF(mgr *BrowserManager) (tools.ToolResult, error) {
	page, err := mgr.getPage()
	if err != nil {
		return tools.ToolResult{}, err
	}

	reader, err := page.PDF(&proto.PagePrintToPDF{})
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("pdf failed: %w", err)
	}

	buf, err := io.ReadAll(reader)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("read pdf failed: %w", err)
	}

	path, err := writeTempFileExcl("browser-page-*.pdf", buf, mgr.FileMode)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("save pdf failed: %w", err)
	}

	return tools.ToolResult{Text: fmt.Sprintf("PDF saved to: %s", path)}, nil
}

// writeTempFileExcl saves data under the foci temp root using pattern (a
// tempdir.Create glob pattern, e.g. "browser-screenshot-*.png") and returns
// the resulting path. Unlike the previous
// fmt.Sprintf("...-%d", time.Now().Unix()) + os.WriteFile approach, this
// can't be symlink-planted: tempdir.Create opens with O_EXCL under a random
// suffix, so a pre-existing file/symlink at any guessable name is simply
// not the path picked — there's nothing for the write to follow (#1501: the
// exec bridge's funcs-file used the same unsafe pattern into the same temp
// root; this shares it and gets the same fix). mode restores the caller's
// desired permission bits, since CreateTemp always creates 0600 regardless
// of what's requested.
func writeTempFileExcl(pattern string, data []byte, mode os.FileMode) (string, error) {
	f, err := tempdir.Create(pattern)
	if err != nil {
		return "", err
	}
	path := f.Name()
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(path)
		return "", writeErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", closeErr
	}
	if err := os.Chmod(path, mode); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func browserEvaluate(mgr *BrowserManager, p browserParams) (tools.ToolResult, error) {
	if p.Script == "" {
		return tools.ToolResult{}, errors.New("script required")
	}

	page, err := mgr.getPage()
	if err != nil {
		return tools.ToolResult{}, err
	}

	result, err := page.Eval(p.Script)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("evaluate failed: %w", err)
	}

	return tools.ToolResult{Text: fmt.Sprintf("Result: %v", result.Value)}, nil
}

func browserWait(mgr *BrowserManager, p browserParams) (tools.ToolResult, error) {
	page, err := mgr.getPage()
	if err != nil {
		return tools.ToolResult{}, err
	}

	switch p.WaitType {
	case "load":
		if err := page.WaitLoad(); err != nil {
			return tools.ToolResult{}, fmt.Errorf("wait load failed: %w", err)
		}
	case "idle":
		wait := page.MustWaitRequestIdle()
		wait()
	default:
		return tools.ToolResult{}, fmt.Errorf("unknown waitType %q — use load or idle", p.WaitType)
	}

	return tools.ToolResult{Text: fmt.Sprintf("Wait completed: %s", p.WaitType)}, nil
}

func browserStart(mgr *BrowserManager, p browserParams) (tools.ToolResult, error) {
	if mgr.IsConnected() {
		return tools.ToolResult{}, errors.New("browser is already running: use close first to restart with different settings")
	}
	if p.Incognito != nil {
		mgr.incognito = *p.Incognito
	}
	if err := mgr.Start(); err != nil {
		return tools.ToolResult{}, err
	}
	mode := "on"
	if !mgr.incognito {
		mode = "off"
	}
	return tools.ToolResult{Text: fmt.Sprintf("Browser started (incognito: %s)", mode)}, nil
}

func browserClose(mgr *BrowserManager) (tools.ToolResult, error) {
	if err := mgr.Stop(); err != nil {
		return tools.ToolResult{}, err
	}
	return tools.ToolResult{Text: "Browser closed"}, nil
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
