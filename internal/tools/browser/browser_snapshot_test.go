package browser

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"testing"

	"foci/internal/tools"

	"github.com/go-rod/rod"
)

func TestParseRef(t *testing.T) {
	// Validates that ParseRef correctly accepts valid ref formats
	// and rejects invalid ones, covering plain refs, frame-prefixed refs,
	// and various malformed strings.
	t.Parallel()

	tests := []struct {
		name    string
		ref     string
		wantErr bool
	}{
		{name: "valid plain ref", ref: "s1e5", wantErr: false},
		{name: "valid high numbers", ref: "s123e456", wantErr: false},
		{name: "valid frame-prefixed ref", ref: "f1s1e5", wantErr: false},
		{name: "valid frame-prefixed high numbers", ref: "f12s99e100", wantErr: false},
		{name: "empty ref", ref: "", wantErr: true},
		{name: "missing s prefix", ref: "1e5", wantErr: true},
		{name: "missing e prefix", ref: "s1x5", wantErr: true},
		{name: "no numbers", ref: "se", wantErr: true},
		{name: "garbage", ref: "foobar", wantErr: true},
		{name: "css selector", ref: "#login-button", wantErr: true},
		{name: "frame prefix only", ref: "f1", wantErr: true},
		{name: "frame with bad ref", ref: "f1garbage", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ParseRef(tt.ref)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseRef(%q): err=%v, wantErr=%v", tt.ref, err, tt.wantErr)
			}
		})
	}
}

func TestLocatorInFrameNilSnapshot(t *testing.T) {
	// Verifies that LocatorInFrame returns a clear
	// error when called on a nil snapshot (no snapshot captured yet).
	t.Parallel()

	var snap *Snapshot
	_, err := snap.LocatorInFrame("s1e5")
	if err == nil {
		t.Fatal("expected error for nil snapshot")
	}
	if got := err.Error(); got != "no snapshot available — use 'snapshot' action first" {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestLocatorInFrameEmptyFrames(t *testing.T) {
	// Verifies that LocatorInFrame returns a clear
	// error when the snapshot has no frames registered.
	t.Parallel()

	snap := &Snapshot{}
	_, err := snap.LocatorInFrame("s1e5")
	if err == nil {
		t.Fatal("expected error for empty frames")
	}
}

func TestLocatorInFrameOutOfRange(t *testing.T) {
	// Verifies that a frame index beyond the
	// registered frames slice returns a clear out-of-range error.
	t.Parallel()

	snap := &Snapshot{
		frames:     nil, // no frames
		generation: 1,
	}
	_, err := snap.LocatorInFrame("s1e5")
	if err == nil {
		t.Fatal("expected error for no frames")
	}

	// With one frame but referencing frame index 5
	snap2 := &Snapshot{
		frames:     make([]*rod.Page, 1),
		generation: 1,
	}
	_, err = snap2.LocatorInFrame("f5s1e3")
	if err == nil {
		t.Fatal("expected error for out-of-range frame index")
	}
}

func TestSnapshotString(t *testing.T) {
	// Verifies that the Snapshot.String() method returns
	// the stored text.
	t.Parallel()

	snap := &Snapshot{text: "test snapshot content", generation: 42}
	if snap.String() != "test snapshot content" {
		t.Errorf("String() = %q, want %q", snap.String(), "test snapshot content")
	}
	if snap.generation != 42 {
		t.Errorf("generation = %d, want 42", snap.generation)
	}
}

func TestSnapshotJSONContentType(t *testing.T) {
	// Verifies that navigating to a JSON endpoint
	// produces a snapshot with ```json code block instead of ```yaml.
	t.Parallel()
	srv := testJSONServer(t, `{"status":"ok","count":42}`)
	mgr := sharedBrowserManager(t)
	tool := NewBrowserTool(mgr)

	params := marshalParams(t, map[string]any{"action": "navigate", "url": srv.URL})
	result, err := tool.Execute(t.Context(), params)
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}

	if !strings.Contains(result.Text, "```json") {
		t.Errorf("expected ```json code block for JSON content type, got:\n%s", result.Text)
	}
	if strings.Contains(result.Text, "```yaml") {
		t.Error("should not contain ```yaml for JSON content type")
	}
}

func TestSnapshotHTMLContentType(t *testing.T) {
	// Verifies that navigating to an HTML page
	// produces a snapshot with ```yaml code block (the default for accessibility trees).
	t.Parallel()
	srv := testHTMLServer(t, `<html><head><title>Test</title></head><body><h1>Hello</h1></body></html>`)
	mgr := sharedBrowserManager(t)
	tool := NewBrowserTool(mgr)

	params := marshalParams(t, map[string]any{"action": "navigate", "url": srv.URL})
	result, err := tool.Execute(t.Context(), params)
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}

	if !strings.Contains(result.Text, "```yaml") {
		t.Errorf("expected ```yaml code block for HTML content type, got:\n%s", result.Text)
	}
}

// navigateNoPanic runs a navigate through the tool and turns a panic into a
// test failure, so a regression reports here instead of killing the binary.
func navigateNoPanic(t *testing.T, tool *tools.Tool, url string) string {
	t.Helper()
	var text string
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("navigate panicked: %v", r)
			}
		}()
		result, err := tool.Execute(t.Context(), marshalParams(t, map[string]any{"action": "navigate", "url": url}))
		if err != nil {
			t.Fatalf("navigate: %v", err)
		}
		text = result.Text
	}()
	return text
}

func TestSnapshotDocumentlessIframeDoesNotFailNavigate(t *testing.T) {
	// #3164: an iframe the snapshot lists, but that has no content document by
	// the time its frame is captured, made rod's getJSCtxID dereference a nil
	// ContentDocument and failed the whole navigate. The page below moves the
	// iframe into a document with no browsing context as soon as the snapshot
	// stamps it: it stays isConnected (so the ref still resolves) but has no
	// document. The rest of the page must be returned and the frame marked.
	t.Parallel()
	srv := testHTMLServer(t, `<html><head><title>Parent</title></head><body>
		<h1>Outer Heading</h1>
		<iframe id="f" srcdoc="<button>Inside Frame</button>"></iframe>
		<button>Outer Button</button>
		<script>
		const f = document.getElementById("f");
		new MutationObserver(() => {
			if (f.ownerDocument === document) {
				document.implementation.createHTMLDocument("").body.appendChild(f);
			}
		}).observe(f, {attributes: true, attributeFilter: ["data-foci-ref"]});
		</script></body></html>`)

	text := navigateNoPanic(t, NewBrowserTool(sharedBrowserManager(t)), srv.URL)

	for _, want := range []string{"Outer Heading", "Outer Button", unreadableFrameMarker} {
		if !strings.Contains(text, want) {
			t.Errorf("snapshot missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Inside Frame") {
		t.Errorf("documentless iframe content captured, want it marked unreadable:\n%s", text)
	}
}

func TestSnapshotSameOriginIframeIsCaptured(t *testing.T) {
	// Positive control for #3164: a same-origin iframe's content is still inlined.
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/child", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><button>Inside Frame</button></body></html>`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><h1>Outer Heading</h1><iframe src="/child" title="same"></iframe></body></html>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	tool := NewBrowserTool(sharedBrowserManager(t))
	text := navigateNoPanic(t, tool, srv.URL)

	if !strings.Contains(text, "Inside Frame") {
		t.Errorf("same-origin iframe content not captured:\n%s", text)
	}
	if strings.Contains(text, unreadableFrameMarker) {
		t.Errorf("same-origin iframe marked unreadable:\n%s", text)
	}
}

func TestCaptureDetachedChildFrameReturnsError(t *testing.T) {
	// #3164 reproduction: an iframe element with no content document (here, one
	// removed from the DOM after its frame page was taken, as when a page drops
	// an iframe during load). rod's getJSCtxID dereferences the nil
	// ContentDocument; capturing that frame must return an error, not panic.
	t.Parallel()
	srv := testHTMLServer(t, `<html><body><iframe id="f" srcdoc="<button>Inside</button>"></iframe></body></html>`)
	mgr := sharedBrowserManager(t)
	navigateNoPanic(t, NewBrowserTool(mgr), srv.URL)
	page, err := mgr.getPage()
	if err != nil {
		t.Fatalf("getPage: %v", err)
	}
	iframe, err := page.Element("#f")
	if err != nil {
		t.Fatalf("find iframe: %v", err)
	}
	child, err := iframe.Frame()
	if err != nil {
		t.Fatalf("iframe.Frame: %v", err)
	}
	if _, err := page.Eval(`() => document.getElementById("f").remove()`); err != nil {
		t.Fatalf("remove iframe: %v", err)
	}

	snap := &Snapshot{frames: []*rod.Page{page}}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("capturing a detached child frame panicked: %v\n%s", r, debug.Stack())
			}
		}()
		if _, err := snap.captureWithFrames(child); err == nil {
			t.Fatal("capturing a detached child frame succeeded, want an error")
		}
	}()
}
