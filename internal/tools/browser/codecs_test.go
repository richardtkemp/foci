package browser

import (
	"context"
	"strings"
	"testing"
)

// stubCanPlay makes the page's canPlayType answer verdict for every type, so a
// test controls the codec probe whichever Chromium build runs it.
func stubCanPlay(verdict string) string {
	return `<script>HTMLMediaElement.prototype.canPlayType = () => '` + verdict + `';</script>`
}

// TestSnapshotNotesMissingCodecsOnMediaPages checks the snapshot of a page
// with a media element states that the browser cannot play H.264/AAC, and
// that the note appears only then (foci_todo #3263).
func TestSnapshotNotesMissingCodecsOnMediaPages(t *testing.T) {
	t.Parallel()
	const note = "cannot play H.264 or AAC"
	cases := []struct {
		name    string
		verdict string // what the stubbed canPlayType answers
		body    string
		want    bool
	}{
		{"video, codecs missing", "", `<h1>Watch</h1><video></video>`, true},
		{"audio, codecs missing", "", `<h1>Listen</h1><audio></audio>`, true},
		{"video in iframe, codecs missing", "", `<h1>Embed</h1><iframe srcdoc="<video></video>"></iframe>`, true},
		{"video, codecs present", "probably", `<h1>Watch</h1><video></video>`, false},
		{"no media, codecs missing", "", `<h1>Text only</h1>`, false},
	}
	mgr := sharedBrowserManager(t)
	tool := NewBrowserTool(mgr)
	for _, tc := range cases {
		srv := testHTMLServer(t, `<html><head>`+stubCanPlay(tc.verdict)+`</head><body>`+tc.body+`</body></html>`)
		res, err := tool.Execute(context.Background(), marshalParams(t, map[string]any{"action": "navigate", "url": srv.URL}))
		if err != nil {
			t.Fatalf("%s: navigate: %v", tc.name, err)
		}
		if got := strings.Contains(res.Text, note); got != tc.want {
			t.Errorf("%s: snapshot has codec note = %v, want %v\n%s", tc.name, got, tc.want, res.Text)
		}
	}
}

// TestBrowserStartReportsCodecGap checks the start result names the missing
// codecs exactly when the running browser lacks them (foci_todo #3263).
func TestBrowserStartReportsCodecGap(t *testing.T) {
	t.Parallel()
	mgr := testBrowserManager(t)
	tool := NewBrowserTool(mgr)
	res, err := tool.Execute(context.Background(), marshalParams(t, map[string]any{"action": "start"}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	page, err := mgr.getPage()
	if err != nil {
		t.Fatal(err)
	}
	missing, err := missingCodecs(page)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("browser lacks codecs: %v", missing)
	if got, want := strings.Contains(res.Text, "cannot play"), len(missing) > 0; got != want {
		t.Errorf("start result has codec note = %v, want %v (missing %v)\n%s", got, want, missing, res.Text)
	}
	for _, c := range missing {
		if !strings.Contains(res.Text, c) {
			t.Errorf("start result does not name %s:\n%s", c, res.Text)
		}
	}
}
