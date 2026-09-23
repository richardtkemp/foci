package main

import (
	"reflect"
	"strings"
	"testing"
)

const testDoc = "# x\n\n" + graphHeading + "\n\n```\nmain\n" +
	" ├── config        → display, log (notes, with commas)\n" +
	" │                  (continuation note — ignored)\n" +
	" ├── display       (no deps — table rendering)\n" +
	" ├── prompts       (top-level, not internal — lives at `shared/prompts/`) → log (embedded files)\n" +
	" ├── memory        → log, fsnotify, blevesearch/bleve/v2 (FTS5 + bleve)\n" +
	" │     ├── tools/spill    → (stdlib only) spill writer\n" +
	" └── evals         → log, yaml.v3 — rubric registry\n" +
	"```\n\n## Next\n"

var testImports = map[string][]string{
	"foci/internal/config":      {"fmt", "foci/internal/display", "foci/internal/log"},
	"foci/internal/display":     {"strings"},
	"foci/internal/log":         {"os"},
	"foci/shared/prompts":       {"embed", "foci/internal/log"},
	"foci/internal/memory":      {"foci/internal/log", "github.com/fsnotify/fsnotify", "github.com/blevesearch/bleve/v2"},
	"foci/internal/tools/spill": {"os"},
	"foci/internal/evals":       {"foci/internal/log", "gopkg.in/yaml.v3"},
}

func TestParseGraph(t *testing.T) {
	entries, err := parseGraph([]byte(testDoc))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, e := range entries {
		got[e.name] = e.deps
	}
	want := map[string][]string{
		"config":      {"display", "log"},
		"display":     nil,
		"prompts":     {"log"},
		"memory":      {"log", "fsnotify", "blevesearch/bleve/v2"},
		"tools/spill": nil,
		"evals":       {"log", "yaml.v3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed deps:\n got %v\nwant %v", got, want)
	}
}

func TestCheckInSync(t *testing.T) {
	entries, err := parseGraph([]byte(testDoc))
	if err != nil {
		t.Fatal(err)
	}
	if f := check(entries, testImports); len(f) != 0 {
		t.Fatalf("expected no findings, got %v", f)
	}
}

// Each mutation is a kind of drift the gate exists to catch.
func TestCheckFlagsDrift(t *testing.T) {
	cases := []struct {
		name, from, to, wantSub string
	}{
		{"import added, line not updated", "→ display, log (notes", "→ display (notes", "config: missing log"},
		{"import removed, line not updated", "→ log (embedded", "→ log, display (embedded", "prompts: not imported display"},
		{"no-deps claim with a real dep", " ├── config        → display, log (notes, with commas)", " ├── config        (no deps)", "config: missing display, log"},
		{"third-party dep dropped", "→ log, yaml.v3", "→ log, yaml.v3, gorilla/websocket", "evals: not imported gorilla/websocket"},
		{"package renamed away", " ├── display ", " ├── displayx ", "displayx: no such package"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := strings.Replace(testDoc, tc.from, tc.to, 1)
			if doc == testDoc {
				t.Fatalf("mutation %q did not apply", tc.from)
			}
			entries, err := parseGraph([]byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			f := check(entries, testImports)
			if len(f) != 1 || !strings.Contains(f[0], tc.wantSub) {
				t.Fatalf("want one finding containing %q, got %v", tc.wantSub, f)
			}
		})
	}
}

func TestParseGraphErrors(t *testing.T) {
	for name, doc := range map[string]string{
		"no heading":   "# x\n```\n ├── a\n```\n",
		"unterminated": graphHeading + "\n```\n ├── a\n",
		"empty tree":   graphHeading + "\n```\nmain\n```\n",
	} {
		if _, err := parseGraph([]byte(doc)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
