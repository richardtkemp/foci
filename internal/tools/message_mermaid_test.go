package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/mermaid"
	"foci/internal/mermaid/mermaidtest"
	"foci/internal/platform"
)

// photoReadingSender records each photo's display name and bytes at send time:
// the tool sends the render through a temp symlink it deletes on return.
type photoReadingSender struct {
	*mockSender
	photoNames  []string
	photoBodies []string
}

func (s *photoReadingSender) SendPhoto(filePath, caption string) error {
	b, _ := os.ReadFile(filePath)
	s.photoNames = append(s.photoNames, filepath.Base(filePath))
	s.photoBodies = append(s.photoBodies, string(b))
	return s.mockSender.SendPhoto(filePath, caption)
}

func writeDiagram(t *testing.T, name, src string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runMermaidSend(t *testing.T, r *mermaid.Renderer, params map[string]any) (*photoReadingSender, string) {
	t.Helper()
	s := &photoReadingSender{mockSender: &mockSender{}}
	tool := NewSendToChatTool(func(string) platform.Sender { return s }, nil, nil, r)
	raw, _ := json.Marshal(params)
	res, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return s, res.Text
}

func TestSendToChat_MermaidFileSendsSourceAndRender(t *testing.T) {
	t.Parallel()
	cmd, runs := mermaidtest.FakeCmd(t)
	r := mermaid.New(t.TempDir(), cmd, "/no/browser")
	src := writeDiagram(t, "flow.mmd", "graph TD\n A-->B\n")

	s, res := runMermaidSend(t, r, map[string]any{"file": src, "text": "the flow"})

	if len(s.documentCalls) != 1 || s.documentCalls[0] != src || s.documentCaptions[0] != "the flow" {
		t.Errorf("source not sent as captioned document: calls=%v captions=%v", s.documentCalls, s.documentCaptions)
	}
	if len(s.photoNames) != 1 || s.photoNames[0] != "flow.png" {
		t.Fatalf("render not sent as flow.png photo: %v", s.photoNames)
	}
	// Opaque white, light theme: a transparent PNG vanishes in a dark-mode chat.
	if !strings.HasPrefix(s.photoBodies[0], "\x89PNG") || !strings.HasSuffix(s.photoBodies[0], "bg=white") {
		t.Errorf("photo body = %q, want a PNG rendered on white", s.photoBodies[0])
	}
	if b, _ := os.ReadFile(runs); string(b) != "default\n" {
		t.Errorf("themes passed to mmdc = %q, want default", b)
	}
	if res != "Sent: flow.mmd+caption + flow.png" {
		t.Errorf("result = %q", res)
	}
}

func TestSendToChat_MermaidDetectedByDisplayName(t *testing.T) {
	t.Parallel()
	cmd, _ := mermaidtest.FakeCmd(t)
	r := mermaid.New(t.TempDir(), cmd, "/no/browser")
	src := writeDiagram(t, "tmp123", "graph TD\n A-->B\n")

	s, _ := runMermaidSend(t, r, map[string]any{"file": src, "filename": "Arch.MERMAID"})
	if len(s.photoNames) != 1 || s.photoNames[0] != "Arch.png" {
		t.Errorf("photos = %v, want Arch.png", s.photoNames)
	}
}

func TestSendToChat_MermaidNotRenderedSendsSourceWithNote(t *testing.T) {
	t.Parallel()
	cmd, _ := mermaidtest.FakeCmd(t)
	cases := []struct {
		name, cmd, src, want string
	}{
		{"no mmdc", "/nonexistent/mmdc", "graph TD\n A-->B\n", "install mmdc"},
		{"rendering off", mermaid.DisabledCmd, "graph TD\n A-->B\n", "install mmdc"},
		{"bad diagram", cmd, "graph TD\n BAD-->\n", "Parse error on line 2"},
		{"empty", cmd, "  \n", "empty"},
		{"too large", cmd, "graph TD\n" + strings.Repeat("A-->B\n", mermaid.MaxSource/6+1), "too large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mermaid.New(t.TempDir(), tc.cmd, "/no/browser")
			src := writeDiagram(t, "d.mmd", tc.src)
			s, res := runMermaidSend(t, r, map[string]any{"file": src})
			if len(s.documentCalls) != 1 {
				t.Errorf("source not sent: %v", s.documentCalls)
			}
			if len(s.photoNames) != 0 {
				t.Errorf("unexpected photo: %v", s.photoNames)
			}
			if !strings.HasPrefix(res, "Sent: d.mmd (image not sent: ") || !strings.Contains(res, tc.want) {
				t.Errorf("result = %q, want a not-sent note containing %q", res, tc.want)
			}
		})
	}
}

func TestSendToChat_NonMermaidOrExplicitSendAsIsNotRendered(t *testing.T) {
	t.Parallel()
	cmd, runs := mermaidtest.FakeCmd(t)
	r := mermaid.New(t.TempDir(), cmd, "/no/browser")
	md := writeDiagram(t, "notes.md", "```mermaid\ngraph TD\n A-->B\n```\n")
	mmd := writeDiagram(t, "d.mmd", "graph TD\n A-->B\n")

	for _, p := range []map[string]any{
		{"file": md},
		{"file": mmd, "send_as": "photo"},
	} {
		s, _ := runMermaidSend(t, r, p)
		if len(s.photoNames) > 1 || (p["send_as"] == nil && len(s.photoNames) != 0) {
			t.Errorf("%v: photos = %v", p, s.photoNames)
		}
	}
	if n := mermaidtest.RunCount(t, runs); n != 0 {
		t.Errorf("renders = %d, want 0", n)
	}
}
