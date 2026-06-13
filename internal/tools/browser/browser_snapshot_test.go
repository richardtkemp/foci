package browser

import (
	"strings"
	"testing"

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
