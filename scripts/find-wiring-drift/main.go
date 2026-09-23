// Command find-wiring-drift checks that the package dependency tree in
// docs/WIRING.md ("## Package Dependency Graph") matches the real imports
// reported by `go list`.
//
// Each tree entry looks like
//
//	├── config        → display, execguard, log, modelinfo, provider (notes...)
//	│     ├── tools/spill    → (stdlib only) notes...
//	├── display       (no deps — notes...)
//
// For every entry the listed foci-internal deps (the comma-separated names
// after "→", up to the first " (" or " — " note) must equal the package's
// non-test foci imports exactly: a missing internal import and a listed one
// the package no longer imports both fail. An entry with no "→" list claims
// no foci deps. Third-party tokens (fsnotify, BurntSushi/toml, ...) are
// accepted only while they still name one of the package's real imports, so
// a dropped third-party dep that WIRING still lists also fails; third-party
// imports missing from the line are not required.
//
// Names resolve to foci/internal/<name> first, else to the unique package in
// the module whose path ends in /<name> (e.g. prompts → foci/shared/prompts).
// Packages without a tree entry are not checked.
//
// Why: the tree drifted silently whenever a change moved imports without
// touching WIRING (#1995). Runs as part of `make lint`.
//
// Usage: find-wiring-drift [-wiring docs/WIRING.md] (run from the repo root)
//
// Exit codes: 0 = in sync, 1 = drift found, 2 = tool error.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

const (
	modulePrefix = "foci/"
	internalPfx  = "foci/internal/"
	graphHeading = "## Package Dependency Graph"
)

// entry is one parsed tree line.
type entry struct {
	line int
	name string   // package as written, e.g. "secrets/bitwarden"
	deps []string // dep tokens as written; empty = claims no foci deps
}

// entryRe matches a tree entry: tree glyphs, "├──"/"└──", the package name,
// then the rest of the line.
var entryRe = regexp.MustCompile(`^[\s│]*[├└]──\s+(\S+)\s*(.*)$`)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("find-wiring-drift", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	wiring := fs.String("wiring", "docs/WIRING.md", "path to WIRING.md")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: find-wiring-drift [-wiring docs/WIRING.md]")
		fmt.Fprintln(os.Stderr, "Checks WIRING.md's package dependency tree against `go list` imports.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	doc, err := os.ReadFile(*wiring)
	if err != nil {
		fmt.Fprintf(os.Stderr, "find-wiring-drift: %v\n", err)
		return 2
	}
	entries, err := parseGraph(doc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "find-wiring-drift: %s: %v\n", *wiring, err)
		return 2
	}
	imports, err := goListImports()
	if err != nil {
		fmt.Fprintf(os.Stderr, "find-wiring-drift: go list: %v\n", err)
		return 2
	}

	findings := check(entries, imports)
	for _, f := range findings {
		fmt.Printf("%s:%s\n", *wiring, f)
	}
	if len(findings) > 0 {
		fmt.Printf("\n%d WIRING.md dependency line(s) out of sync with `go list` — update the tree in %q\n", len(findings), graphHeading)
		return 1
	}
	return 0
}

// parseGraph extracts the tree entries from the first fenced block after the
// graph heading.
func parseGraph(doc []byte) ([]entry, error) {
	sc := bufio.NewScanner(bytes.NewReader(doc))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var (
		out                       []entry
		lineNo                    int
		inSection, inFence, found bool
	)
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		switch {
		case !inSection:
			inSection = strings.TrimSpace(line) == graphHeading
			continue
		case !inFence:
			if strings.HasPrefix(line, "```") {
				inFence, found = true, true
			} else if strings.HasPrefix(line, "## ") {
				return nil, fmt.Errorf("no fenced block under %q", graphHeading)
			}
			continue
		case strings.HasPrefix(line, "```"):
			if len(out) == 0 {
				return nil, fmt.Errorf("no tree entries under %q", graphHeading)
			}
			return out, nil
		}
		m := entryRe.FindStringSubmatch(line)
		if m == nil {
			continue // root line or a continuation note
		}
		out = append(out, entry{line: lineNo, name: m[1], deps: parseDeps(m[2])})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("heading %q or its fenced block not found", graphHeading)
	}
	return nil, errors.New("unterminated fenced block")
}

// parseDeps turns the text after a package name into its dep tokens. A
// leading parenthetical note is skipped (prompts puts one before its "→").
func parseDeps(rest string) []string {
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "(") {
		if i := closingParen(rest); i >= 0 {
			rest = strings.TrimSpace(rest[i+1:])
		}
	}
	after, ok := strings.CutPrefix(rest, "→")
	if !ok {
		return nil
	}
	after = strings.TrimSpace(after)
	if strings.HasPrefix(after, "(") {
		return nil // "→ (stdlib only) ..." / "→ (shared by ...)"
	}
	for _, sep := range []string{" (", " — "} {
		if i := strings.Index(after, sep); i >= 0 {
			after = after[:i]
		}
	}
	var deps []string
	for _, tok := range strings.Split(after, ",") {
		if tok = strings.TrimSpace(tok); tok != "" {
			deps = append(deps, tok)
		}
	}
	return deps
}

func closingParen(s string) int {
	depth := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// goListImports maps every package in the module to its non-test imports.
func goListImports() (map[string][]string, error) {
	cmd := exec.Command("go", "list", "-f", "{{.ImportPath}} {{join .Imports \" \"}}", "./...")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	res := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		res[fields[0]] = fields[1:]
	}
	return res, nil
}

// check compares each entry against imports (package path → its imports).
func check(entries []entry, imports map[string][]string) []string {
	var findings []string
	for _, e := range entries {
		pkg, ok := resolve(e.name, imports)
		if !ok {
			findings = append(findings, fmt.Sprintf("%d: %s: no such package in the module (renamed or deleted?)", e.line, e.name))
			continue
		}
		listed := map[string]bool{}
		var staleThirdParty []string
		for _, tok := range e.deps {
			if dep, ok := resolve(tok, imports); ok {
				listed[dep] = true
			} else if !namesThirdParty(tok, imports[pkg]) {
				staleThirdParty = append(staleThirdParty, tok)
			}
		}
		actual := map[string]bool{}
		for _, imp := range imports[pkg] {
			if strings.HasPrefix(imp, modulePrefix) {
				actual[imp] = true
			}
		}
		missing := diff(actual, listed, imports)
		extra := append(diff(listed, actual, imports), staleThirdParty...)
		if len(missing) == 0 && len(extra) == 0 {
			continue
		}
		var parts []string
		if len(missing) > 0 {
			parts = append(parts, "missing "+strings.Join(missing, ", "))
		}
		if len(extra) > 0 {
			parts = append(parts, "not imported "+strings.Join(extra, ", "))
		}
		want := displayNames(actual, imports)
		if len(want) == 0 {
			want = []string{"(no foci deps)"}
		}
		findings = append(findings, fmt.Sprintf("%d: %s: %s; go list says → %s",
			e.line, e.name, strings.Join(parts, "; "), strings.Join(want, ", ")))
	}
	return findings
}

// resolve maps a written name to a module package path.
func resolve(name string, imports map[string][]string) (string, bool) {
	if _, ok := imports[internalPfx+name]; ok {
		return internalPfx + name, true
	}
	var match string
	for p := range imports {
		if strings.HasSuffix(p, "/"+name) {
			if match != "" {
				return "", false // ambiguous
			}
			match = p
		}
	}
	return match, match != ""
}

// namesThirdParty reports whether tok still names one of pkgImports' non-foci
// imports (e.g. "fsnotify" → github.com/fsnotify/fsnotify).
func namesThirdParty(tok string, pkgImports []string) bool {
	for _, imp := range pkgImports {
		if !strings.HasPrefix(imp, modulePrefix) && strings.Contains(imp, tok) {
			return true
		}
	}
	return false
}

// diff returns the display names of a's keys absent from b, sorted.
func diff(a, b map[string]bool, imports map[string][]string) []string {
	d := map[string]bool{}
	for k := range a {
		if !b[k] {
			d[k] = true
		}
	}
	return displayNames(d, imports)
}

func shortNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for p := range set {
		if s, ok := strings.CutPrefix(p, internalPfx); ok {
			p = s
		} else {
			p = strings.TrimPrefix(p, modulePrefix)
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// displayNames is shortNames, but a non-internal package whose last path
// element resolves back to it uniquely is shown by that element — the form
// the tree writes it in (shared/prompts → prompts).
func displayNames(set map[string]bool, imports map[string][]string) []string {
	out := make([]string, 0, len(set))
	for p := range set {
		name := shortNames(map[string]bool{p: true})[0]
		if !strings.HasPrefix(p, internalPfx) {
			last := p[strings.LastIndex(p, "/")+1:]
			if got, ok := resolve(last, imports); ok && got == p {
				name = last
			}
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
