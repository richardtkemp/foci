package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/singleflight"

	"foci/internal/procx"
	"foci/internal/tempdir"
)

// Mermaid rendering (#1980). The app renders markdown natively, and neither
// Compose target can run mermaid.js, so a ```mermaid block is rendered HERE by
// mermaid-cli (mmdc, headless Chromium) into a PNG the app fetches on demand:
// POST /app/render/mermaid with the diagram source as the body. Renders are
// slow (seconds: a browser launch each), so they are cached on disk by content
// hash and identical concurrent requests share one render.
const (
	mermaidDirName       = "app-mermaid"
	mermaidMaxSource     = 64 << 10 // larger bodies are refused: no chat diagram is this big
	mermaidRenderTimeout = 45 * time.Second
	mermaidMaxCached     = 256 // PNGs kept on disk; least recently used beyond this are pruned
	mermaidConcurrency   = 2   // simultaneous mmdc runs (each one is a Chromium)
	mermaidScale         = "2" // device-pixel scale: phones need more than 1x to stay crisp
	mermaidDefaultCmd    = "mmdc"
	mermaidDisabledCmd   = "off"
)

var (
	// errMermaidUnavailable: no renderer installed (or rendering turned off).
	errMermaidUnavailable = errors.New("mermaid rendering unavailable")
	// errMermaidTimeout: the render did not finish in mermaidRenderTimeout.
	errMermaidTimeout = errors.New("mermaid render timed out")
)

// mermaidSyntaxError is a render mmdc rejected: the diagram source is at fault,
// not the server. msg is mmdc's own error, trimmed of its JS stack.
type mermaidSyntaxError struct{ msg string }

func (e *mermaidSyntaxError) Error() string { return e.msg }

// mermaidBrowsers are the system browsers tried, in order, when no browser is
// configured. Puppeteer's own downloaded Chrome is used when none is found.
var mermaidBrowsers = []string{"chromium", "chromium-browser", "google-chrome-stable", "google-chrome"}

type mermaidRenderer struct {
	cmd     string // mmdc name or path; "off" disables
	browser string // configured browser path; "" = detect
	dir     string // PNG cache + puppeteer config
	timeout time.Duration

	group singleflight.Group
	sem   chan struct{}
}

func newMermaidRenderer(dataDir, cmd, browser string) *mermaidRenderer {
	if cmd == "" {
		cmd = mermaidDefaultCmd
	}
	dir := filepath.Join(tempdir.Dir(), mermaidDirName)
	if dataDir != "" {
		dir = filepath.Join(dataDir, mermaidDirName)
	}
	return &mermaidRenderer{
		cmd:     cmd,
		browser: browser,
		dir:     dir,
		timeout: mermaidRenderTimeout,
		sem:     make(chan struct{}, mermaidConcurrency),
	}
}

// mermaidTheme maps the client's theme request onto a mermaid theme name. Only
// two are offered: the app picks by whether its card background is dark.
func mermaidTheme(q string) (string, bool) {
	switch q {
	case "", "default", "light":
		return "default", true
	case "dark":
		return "dark", true
	}
	return "", false
}

// cachePath names the cached PNG for a source+theme pair.
func (m *mermaidRenderer) cachePath(src, theme string) string {
	sum := sha256.Sum256([]byte(theme + "\x00" + src))
	return filepath.Join(m.dir, hex.EncodeToString(sum[:])+".png")
}

// render returns the path of a PNG of src in theme, rendering it if it is not
// already cached.
func (m *mermaidRenderer) render(ctx context.Context, src, theme string) (string, error) {
	if m.cmd == mermaidDisabledCmd {
		return "", errMermaidUnavailable
	}
	out := m.cachePath(src, theme)
	if _, err := os.Stat(out); err == nil {
		now := time.Now()
		_ = os.Chtimes(out, now, now) // LRU: a hit counts as a use
		return out, nil
	}
	// The shared render must outlive any one requester: a client that gives up
	// must not cancel the render a second client is waiting on, nor waste the
	// work by discarding a nearly-done PNG.
	_, err, _ := m.group.Do(out, func() (any, error) {
		return nil, m.run(context.WithoutCancel(ctx), src, theme, out)
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// run executes mmdc once, writing the PNG atomically to out.
func (m *mermaidRenderer) run(ctx context.Context, src, theme, out string) error {
	bin, err := procx.LookPath(procx.Trusted, m.cmd)
	if err != nil {
		return errMermaidUnavailable
	}
	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return fmt.Errorf("mermaid cache dir: %w", err)
	}
	work, err := tempdir.SpawnMkdir("mermaid-")
	if err != nil {
		return fmt.Errorf("mermaid work dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	in := filepath.Join(work, "in.mmd")
	png := filepath.Join(work, "out.png")
	if err := os.WriteFile(in, []byte(src), 0o600); err != nil {
		return fmt.Errorf("mermaid input: %w", err)
	}
	args := []string{"-q", "-i", in, "-o", png, "-t", theme, "-b", "transparent", "-s", mermaidScale}
	if browser := m.browserPath(); browser != "" {
		cfg := filepath.Join(work, "puppeteer.json")
		b, _ := json.Marshal(map[string]string{"executablePath": browser})
		if err := os.WriteFile(cfg, b, 0o600); err != nil {
			return fmt.Errorf("mermaid puppeteer config: %w", err)
		}
		args = append(args, "-p", cfg)
	}

	rctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	// Trusted: foci's own machinery, like pandoc — not an agent tool.
	cmd := procx.Spawn(rctx, procx.Trusted, bin, args...)
	cmd.Dir = work
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	// mmdc runs Chromium as its own children. Killing only node on timeout
	// would leave them holding stderr open (Wait would block on it) and alive.
	// Spawn put the tree in its own process group, so kill the group, and
	// sweep it after a normal exit too so no stray browser outlives a render.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	runErr := cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if err := runErr; err != nil {
		if rctx.Err() != nil {
			return errMermaidTimeout
		}
		if mmdcBrowserFailed(stderr.String()) {
			// The server's fault, not the diagram's: say so in the log, where
			// an operator will look, rather than blaming the source.
			return fmt.Errorf("mmdc could not start a browser: %s", mmdcErrorMessage(stderr.String()))
		}
		return &mermaidSyntaxError{msg: mmdcErrorMessage(stderr.String())}
	}
	if fi, err := os.Stat(png); err != nil || fi.Size() == 0 {
		return &mermaidSyntaxError{msg: mmdcErrorMessage(stderr.String())}
	}
	// The work dir is under the temp root, maybe another filesystem: copy
	// into the cache dir, then rename there so a reader never sees half a PNG.
	// singleflight makes out (and so part) unique to this render.
	b, err := os.ReadFile(png)
	if err != nil {
		return fmt.Errorf("mermaid output: %w", err)
	}
	part := out + ".part"
	if err := os.WriteFile(part, b, 0o600); err != nil {
		return fmt.Errorf("mermaid cache write: %w", err)
	}
	if err := os.Rename(part, out); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("mermaid cache write: %w", err)
	}
	m.prune()
	return nil
}

// browserPath is the configured browser, else the first system browser found
// on the trusted PATH, else "" (let puppeteer use its own download).
func (m *mermaidRenderer) browserPath() string {
	if m.browser != "" {
		return m.browser
	}
	for _, name := range mermaidBrowsers {
		if p, err := procx.LookPath(procx.Trusted, name); err == nil {
			return p
		}
	}
	return ""
}

// prune deletes the least recently used PNGs beyond mermaidMaxCached.
func (m *mermaidRenderer) prune() {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	type png struct {
		path string
		mod  time.Time
	}
	var pngs []png
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".png") {
			continue
		}
		if fi, err := e.Info(); err == nil {
			pngs = append(pngs, png{filepath.Join(m.dir, e.Name()), fi.ModTime()})
		}
	}
	if len(pngs) <= mermaidMaxCached {
		return
	}
	sort.Slice(pngs, func(i, j int) bool { return pngs[i].mod.After(pngs[j].mod) })
	for _, p := range pngs[mermaidMaxCached:] {
		_ = os.Remove(p.path)
	}
}

// mmdcBrowserFailed reports whether mmdc failed before rendering anything,
// because puppeteer found or launched no browser.
func mmdcBrowserFailed(stderr string) bool {
	return strings.Contains(stderr, "Failed to launch the browser process") ||
		strings.Contains(stderr, "Could not find Chrome") ||
		strings.Contains(stderr, "Browser was not found")
}

// mmdcErrorMessage extracts the human part of mmdc's stderr: from the
// "Error:" line up to where the JS stack trace starts. Falls back to the
// first non-empty line.
func mmdcErrorMessage(stderr string) string {
	lines := strings.Split(stderr, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "Error:") {
			start = i
			break
		}
	}
	var keep []string
	if start >= 0 {
		for _, l := range lines[start:] {
			t := strings.TrimSpace(l)
			if strings.HasPrefix(t, "at ") || strings.Contains(t, "(https://") || strings.Contains(t, "(file://") {
				break
			}
			if t != "" {
				keep = append(keep, strings.TrimRight(l, " \t\r"))
			}
		}
	}
	if len(keep) == 0 {
		for _, l := range lines {
			if t := strings.TrimSpace(l); t != "" {
				keep = []string{t}
				break
			}
		}
	}
	msg := strings.TrimPrefix(strings.Join(keep, "\n"), "Error: ")
	if msg == "" {
		msg = "diagram could not be rendered"
	}
	const maxMsg = 600
	if len(msg) > maxMsg {
		msg = msg[:maxMsg] + "…"
	}
	return msg
}

// ServeMermaid handles POST /app/render/mermaid?theme=dark|default: the body
// is the diagram source; the response is image/png. Errors: 413 source too
// large, 422 the diagram did not render (body: mmdc's message), 501 no
// renderer on this server (the app then shows the source), 504 timeout.
func (h *Hub) ServeMermaid(w http.ResponseWriter, r *http.Request) {
	dev, ok := h.authBlob(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	theme, ok := mermaidTheme(r.URL.Query().Get("theme"))
	if !ok {
		http.Error(w, "bad theme", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, mermaidMaxSource+1))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	if len(body) > mermaidMaxSource {
		http.Error(w, "diagram too large", http.StatusRequestEntityTooLarge)
		return
	}
	src := strings.TrimSpace(string(body))
	if src == "" {
		http.Error(w, "empty diagram", http.StatusBadRequest)
		return
	}
	if h.mermaid == nil {
		http.Error(w, errMermaidUnavailable.Error(), http.StatusNotImplemented)
		return
	}
	appLog.Debugf("mermaid render: device=%s bytes=%d theme=%s", dev.DeviceID, len(src), theme)
	path, err := h.mermaid.render(r.Context(), src, theme)
	var syn *mermaidSyntaxError
	switch {
	case err == nil:
	case errors.Is(err, errMermaidUnavailable):
		http.Error(w, err.Error(), http.StatusNotImplemented)
		return
	case errors.Is(err, errMermaidTimeout):
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	case errors.As(err, &syn):
		http.Error(w, syn.msg, http.StatusUnprocessableEntity)
		return
	default:
		appLog.Errorf("mermaid render for device=%s: %v", dev.DeviceID, err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		// Pruned between render and open by a concurrent render: rare, retryable.
		http.Error(w, "render evicted, retry", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "image/png")
	http.ServeContent(w, r, "diagram.png", time.Time{}, f)
}
