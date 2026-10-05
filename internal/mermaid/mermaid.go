// Package mermaid renders mermaid diagram source to PNG with mermaid-cli
// (mmdc, headless Chromium). It serves two callers: the app's
// POST /app/render/mermaid (#1980), and send_to_chat, which sends a rendered
// image alongside a mermaid file (#2195). Renders are slow (seconds: a browser
// launch each), so they are cached on disk by content hash and identical
// concurrent requests share one render.
package mermaid

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/singleflight"

	"foci/internal/procx"
	"foci/internal/tempdir"
)

const (
	DirName       = "app-mermaid" // cache dir under the data dir (name predates the second caller)
	MaxSource     = 64 << 10      // larger sources are refused: no chat diagram is this big
	RenderTimeout = 45 * time.Second
	MaxCached     = 256 // PNGs kept on disk; least recently used beyond this are pruned
	concurrency   = 2   // simultaneous mmdc runs (each one is a Chromium)
	scale         = "2" // device-pixel scale: phones need more than 1x to stay crisp
	DefaultCmd    = "mmdc"
	DisabledCmd   = "off"

	// Backgrounds. The app draws diagrams on its own card, so it wants
	// Transparent; a chat client may be in dark mode, where the default
	// theme's dark lines vanish on a transparent PNG, so a sent image is White.
	Transparent = "transparent"
	White       = "white"
)

var (
	// ErrUnavailable: no renderer installed (or rendering turned off).
	ErrUnavailable = errors.New("mermaid rendering unavailable")
	// ErrTimeout: the render did not finish in the renderer's Timeout.
	ErrTimeout = errors.New("mermaid render timed out")
)

// SyntaxError is a render mmdc rejected: the diagram source is at fault,
// not the server. Msg is mmdc's own error, trimmed of its JS stack.
type SyntaxError struct{ Msg string }

func (e *SyntaxError) Error() string { return e.Msg }

// browsers are the system browsers tried, in order, when no browser is
// configured. Puppeteer's own downloaded Chrome is used when none is found.
var browsers = []string{"chromium", "chromium-browser", "google-chrome-stable", "google-chrome"}

// Renderer runs mmdc with a disk cache. Safe for concurrent use.
type Renderer struct {
	cmd     string // mmdc name or path; "off" disables
	browser string // configured browser path; "" = detect
	dir     string // PNG cache
	Timeout time.Duration

	group singleflight.Group
	sem   chan struct{}
}

// New returns a renderer caching under <dataDir>/app-mermaid (the temp root
// when dataDir is ""). cmd "" means DefaultCmd.
func New(dataDir, cmd, browser string) *Renderer {
	if cmd == "" {
		cmd = DefaultCmd
	}
	dir := filepath.Join(tempdir.Dir(), DirName)
	if dataDir != "" {
		dir = filepath.Join(dataDir, DirName)
	}
	return &Renderer{
		cmd:     cmd,
		browser: browser,
		dir:     dir,
		Timeout: RenderTimeout,
		sem:     make(chan struct{}, concurrency),
	}
}

var (
	sharedMu sync.Mutex
	shared   = map[[3]string]*Renderer{}
)

// Shared returns the process-wide renderer for this configuration, creating
// it on first use. The app hub and send_to_chat both use it, so they share
// one concurrency limit and one in-flight render per diagram.
func Shared(dataDir, cmd, browser string) *Renderer {
	if cmd == "" {
		cmd = DefaultCmd
	}
	key := [3]string{dataDir, cmd, browser}
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if r := shared[key]; r != nil {
		return r
	}
	r := New(dataDir, cmd, browser)
	shared[key] = r
	return r
}

// IsSourceFile reports whether name has a mermaid source extension.
func IsSourceFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mmd", ".mermaid":
		return true
	}
	return false
}

// cachePath names the cached PNG for a source+theme+background triple.
func (m *Renderer) cachePath(src, theme, background string) string {
	sum := sha256.Sum256([]byte(theme + "\x00" + background + "\x00" + src))
	return filepath.Join(m.dir, hex.EncodeToString(sum[:])+".png")
}

// Render returns the path of a PNG of src in theme ("default" or "dark") on
// background (Transparent or White), rendering it if it is not already
// cached. The file belongs to the cache: read or copy it, never modify it;
// it may be pruned by a later render.
func (m *Renderer) Render(ctx context.Context, src, theme, background string) (string, error) {
	if m.cmd == DisabledCmd {
		return "", ErrUnavailable
	}
	out := m.cachePath(src, theme, background)
	if _, err := os.Stat(out); err == nil {
		now := time.Now()
		_ = os.Chtimes(out, now, now) // LRU: a hit counts as a use
		return out, nil
	}
	// The shared render must outlive any one requester: a client that gives up
	// must not cancel the render a second client is waiting on, nor waste the
	// work by discarding a nearly-done PNG.
	_, err, _ := m.group.Do(out, func() (any, error) {
		return nil, m.run(context.WithoutCancel(ctx), src, theme, background, out)
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// run executes mmdc once, writing the PNG atomically to out.
func (m *Renderer) run(ctx context.Context, src, theme, background, out string) error {
	bin, err := procx.LookPath(procx.Trusted, m.cmd)
	if err != nil {
		return ErrUnavailable
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
	args := []string{"-q", "-i", in, "-o", png, "-t", theme, "-b", background, "-s", scale}
	if browser := m.browserPath(); browser != "" {
		cfg := filepath.Join(work, "puppeteer.json")
		b, _ := json.Marshal(map[string]string{"executablePath": browser})
		if err := os.WriteFile(cfg, b, 0o600); err != nil {
			return fmt.Errorf("mermaid puppeteer config: %w", err)
		}
		args = append(args, "-p", cfg)
	}

	rctx, cancel := context.WithTimeout(ctx, m.Timeout)
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
			return ErrTimeout
		}
		if mmdcBrowserFailed(stderr.String()) {
			// The server's fault, not the diagram's: say so in the log, where
			// an operator will look, rather than blaming the source.
			return fmt.Errorf("mmdc could not start a browser: %s", mmdcErrorMessage(stderr.String()))
		}
		return &SyntaxError{Msg: mmdcErrorMessage(stderr.String())}
	}
	if fi, err := os.Stat(png); err != nil || fi.Size() == 0 {
		return &SyntaxError{Msg: mmdcErrorMessage(stderr.String())}
	}
	// The work dir is under the temp root, maybe another filesystem: copy
	// into the cache dir, then rename there so a reader never sees half a PNG.
	b, err := os.ReadFile(png)
	if err != nil {
		return fmt.Errorf("mermaid output: %w", err)
	}
	// A unique part name: another Renderer on the same cache dir may be
	// writing the same diagram.
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	part := out + "." + hex.EncodeToString(nonce[:]) + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("mermaid cache write: %w", err)
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(part)
		return fmt.Errorf("mermaid cache write: %w", werr)
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
func (m *Renderer) browserPath() string {
	if m.browser != "" {
		return m.browser
	}
	for _, name := range browsers {
		if p, err := procx.LookPath(procx.Trusted, name); err == nil {
			return p
		}
	}
	return ""
}

// prune deletes the least recently used PNGs beyond MaxCached.
func (m *Renderer) prune() {
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
	if len(pngs) <= MaxCached {
		return
	}
	sort.Slice(pngs, func(i, j int) bool { return pngs[i].mod.After(pngs[j].mod) })
	for _, p := range pngs[MaxCached:] {
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
