package tools

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"foci/internal/tools/browserjs"

	"github.com/go-rod/rod"
	"gopkg.in/yaml.v3"
)

// Snapshot holds the result of an accessibility tree capture, including
// frame registry for cross-frame ref resolution.
type Snapshot struct {
	frames     []*rod.Page
	text       string
	generation int
}

// String returns the formatted snapshot text.
func (s *Snapshot) String() string { return s.text }

// Generation returns the snapshot generation number.
func (s *Snapshot) Generation() int { return s.generation }

// injectSnapshotJS injects the ARIA snapshot engine into a page's JS context.
// Must be called before each capture since page navigation resets JS context.
// Uses page.Eval (rod's Runtime.callFunctionOn) so the injection targets the
// same execution context that subsequent page.Eval calls use. Raw
// proto.RuntimeEvaluate can target a stale context after page redirects.
// The engine is assigned to window.snapshotEngine since var inside a function
// is local, but we need it globally for AriaSnapshot and QueryEleByAria.
func injectSnapshotJS(page *rod.Page) error {
	_, err := page.Eval("() => { " + browserjs.SnapshotJS + "; window.snapshotEngine = snapshotEngine; }")
	return err
}

// BuildSnapshot captures an accessibility tree snapshot of the given page,
// recursively including iframe contents with frame-prefixed refs.
func BuildSnapshot(page *rod.Page, generation int) (*Snapshot, error) {
	snap := &Snapshot{
		generation: generation,
	}

	yamlDoc, err := snap.captureWithFrames(page)
	if err != nil {
		return nil, err
	}

	yamlBytes, err := yaml.Marshal(yamlDoc)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot YAML: %w", err)
	}

	info, err := page.Info()
	if err != nil {
		return nil, fmt.Errorf("get page info: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "- Page URL: %s\n", info.URL)
	fmt.Fprintf(&b, "- Page Title: %s\n", info.Title)
	fmt.Fprintf(&b, "- Frame Count: %d\n", len(snap.frames))
	b.WriteString("- Page Snapshot\n```yaml\n")
	b.WriteString(strings.TrimSpace(string(yamlBytes)))
	b.WriteString("\n```\n")

	snap.text = b.String()
	return snap, nil
}

// captureWithFrames injects JS into the page, runs the ARIA snapshot,
// unmarshals the YAML, and walks the tree to handle iframes.
func (s *Snapshot) captureWithFrames(page *rod.Page) (*yaml.Node, error) {
	s.frames = append(s.frames, page)
	frameIndex := len(s.frames) - 1

	if err := injectSnapshotJS(page); err != nil {
		return nil, fmt.Errorf("inject snapshot JS (frame %d): %w", frameIndex, err)
	}

	result, err := page.Eval(browserjs.AriaSnapshot, "document.body", "({ref: true})")
	if err != nil {
		return nil, fmt.Errorf("capture snapshot (frame %d): %w", frameIndex, err)
	}

	var snapNode yaml.Node
	if err := yaml.Unmarshal([]byte(result.Value.String()), &snapNode); err != nil {
		return nil, fmt.Errorf("unmarshal snapshot YAML (frame %d): %w", frameIndex, err)
	}

	return s.walk(&snapNode, frameIndex, page)
}

// walk recursively processes the YAML tree, prefixing refs for non-root frames
// and inlining iframe snapshots.
func (s *Snapshot) walk(node *yaml.Node, frameIndex int, frame *rod.Page) (*yaml.Node, error) {
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		processed, err := s.walk(node.Content[0], frameIndex, frame)
		if err != nil {
			return nil, err
		}
		node.Content[0] = processed
		return node, nil
	}

	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i < len(node.Content); i += 2 {
			newKey, err := s.walk(node.Content[i], frameIndex, frame)
			if err != nil {
				return nil, err
			}
			newValue, err := s.walk(node.Content[i+1], frameIndex, frame)
			if err != nil {
				return nil, err
			}
			node.Content[i] = newKey
			node.Content[i+1] = newValue
		}

	case yaml.SequenceNode:
		for i, item := range node.Content {
			processed, err := s.walk(item, frameIndex, frame)
			if err != nil {
				return nil, err
			}
			node.Content[i] = processed
		}

	case yaml.ScalarNode:
		if node.Tag == "!!str" {
			value := node.Value

			// Prefix refs for non-root frames: [ref=s1e5] → [ref=f1s1e5]
			if frameIndex > 0 {
				node.Value = strings.Replace(value, "[ref=", fmt.Sprintf("[ref=f%d", frameIndex), 1)
			}

			// Handle iframe nodes — capture child frame snapshot inline
			if strings.HasPrefix(value, "iframe ") {
				re := regexp.MustCompile(`\[ref=(.*?)\]`)
				matches := re.FindStringSubmatch(value)
				if len(matches) > 1 {
					ref := matches[1]

					pairNode := &yaml.Node{
						Kind: yaml.MappingNode,
						Content: []*yaml.Node{
							{Kind: yaml.ScalarNode, Value: node.Value},
							{Kind: yaml.ScalarNode, Value: "<could not capture iframe snapshot>"},
						},
					}

					childFrameEle, err := queryEleByAria(frame, ref)
					if err != nil {
						return pairNode, nil
					}

					childFrame, err := childFrameEle.Frame()
					if err != nil {
						return pairNode, nil
					}

					childSnapshot, err := s.captureWithFrames(childFrame)
					if err != nil {
						return pairNode, nil
					}

					if len(childSnapshot.Content) > 0 {
						pairNode.Content[1] = childSnapshot.Content[0]
					}

					return pairNode, nil
				}
			}
		}
	}

	return node, nil
}

// queryEleByAria finds an element on a page by its ARIA snapshot ref.
func queryEleByAria(frame *rod.Page, selector string) (*rod.Element, error) {
	return frame.ElementByJS(rod.Eval(browserjs.QueryEleByAria, selector))
}

var frameRefRe = regexp.MustCompile(`^f(\d+)(.+)`)

// LocatorInFrame resolves a snapshot ref string to a rod.Element in the
// correct frame. Refs like "s1e5" target the main frame; "f2s1e5" targets
// frame index 2.
func (s *Snapshot) LocatorInFrame(ref string) (*rod.Element, error) {
	if s == nil || len(s.frames) == 0 {
		return nil, fmt.Errorf("no snapshot available — use 'snapshot' action first")
	}

	frame := s.frames[0]
	matches := frameRefRe.FindStringSubmatch(ref)
	if len(matches) > 0 {
		frameIndex, err := strconv.Atoi(matches[1])
		if err != nil {
			return nil, fmt.Errorf("invalid frame index in ref %q: %w", ref, err)
		}
		if frameIndex < 0 || frameIndex >= len(s.frames) {
			return nil, fmt.Errorf("frame index %d out of range (have %d frames)", frameIndex, len(s.frames))
		}
		frame = s.frames[frameIndex]
		ref = matches[2]
	}

	el, err := queryEleByAria(frame, ref)
	if err != nil {
		return nil, fmt.Errorf("element not found for ref %q (snapshot may be stale — try 'snapshot' action): %w", ref, err)
	}
	return el, nil
}

// ParseRef validates a ref string format. Returns an error if invalid.
func ParseRef(ref string) error {
	// Strip optional frame prefix
	r := ref
	if m := frameRefRe.FindStringSubmatch(ref); len(m) > 0 {
		r = m[2]
	}
	// Must match s<gen>e<id>
	matched, _ := regexp.MatchString(`^s\d+e\d+$`, r)
	if !matched {
		return fmt.Errorf("invalid ref format %q — expected [ref=s<N>e<N>] from snapshot", ref)
	}
	return nil
}
