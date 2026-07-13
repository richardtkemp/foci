// Command find-static-config-reads reports every read of the static,
// non-live *config.ResolvedAgentConfig snapshot — the "old way" of getting
// an agent's resolved config, superseded by config.LiveValue (see
// internal/config/live.go and the bucket A-D commits that migrated fields
// to it: ed71e6b3, eada0305, 78ab5007, a30414b8).
//
// The convention established by that work: a struct that needs an agent's
// resolved config holds it two ways —
//
//   - a plain `resolved *config.ResolvedAgentConfig` (or exported
//     `Resolved`) field: a one-time snapshot, frozen at construction. Any
//     value read off it is baked in and never sees a later config edit.
//   - a `resolvedLive *config.LiveValue[*config.ResolvedAgentConfig]` (or
//     `LiveConfig()`/`LiveConfigFn()` accessor) that re-reads via `.Load()`
//     on every call — see cmd/foci-gw/liveapply.go.
//
// This tool flags every access to a struct field literally named
// `resolved`/`Resolved` whose static type is *config.ResolvedAgentConfig —
// i.e. every place still reaching for the frozen snapshot instead of the
// live one. It intentionally does NOT flag reads through a LiveValue's
// Load()/LiveConfig() (those have a different static type) or reads of a
// local variable produced by such a call — the convention in this codebase
// is that a freshly-Load()ed snapshot gets a name other than
// "resolved"/"Resolved" (e.g. "live", "fresh", "snap"), so matching on the
// literal field name avoids re-flagging an already-correct read-once-per-op
// pattern.
//
// This is a discovery/triage tool, not (yet) a hard gate — main.go's
// bucket-A-D authors expect a large existing backlog. Each finding is
// either a genuine bug (fix it to read live) or an intentional bake (a
// structural/restart-required field, a one-time setup diagnostic, etc.) —
// annotate the latter with a same-line `static-cfg:ignore: <reason>`
// comment to suppress it.
//
// Run with: go run ./scripts/find-static-config-reads/... [package patterns]
// (defaults to ./... from the invoking directory)
//
// Exit codes:
//   - 0: no findings (or all suppressed)
//   - 1: at least one unsuppressed finding
//   - 2: tool error (load failure, etc.)
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// targetType is the exact go/types String() form of the static snapshot
// type this tool hunts for.
const targetType = "*foci/internal/config.ResolvedAgentConfig"

// ignoreMarker suppresses a finding when present on the same source line.
const ignoreMarker = "static-cfg:ignore"

type finding struct {
	file string
	line int
	text string
}

func main() {
	verbose := flag.Bool("v", false, "log per-package progress")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: find-static-config-reads [-v] <package patterns>\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	patterns := flag.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(2)
	}
	if packages.PrintErrors(pkgs) > 0 {
		os.Exit(2)
	}

	lineCache := map[string][]string{}
	var findings []finding
	for _, pkg := range pkgs {
		if *verbose {
			fmt.Fprintln(os.Stderr, "scanning", pkg.PkgPath)
		}
		findings = append(findings, scanPackage(pkg, lineCache)...)
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].file != findings[j].file {
			return findings[i].file < findings[j].file
		}
		return findings[i].line < findings[j].line
	})
	for _, f := range findings {
		fmt.Printf("%s:%d: %s\n", f.file, f.line, f.text)
	}
	if len(findings) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d static config read(s) found\n", len(findings))
		os.Exit(1)
	}
}

// scanPackage walks every non-test file in pkg for selector expressions
// reading a resolved/Resolved-named field of type *config.ResolvedAgentConfig.
func scanPackage(pkg *packages.Package, lineCache map[string][]string) []finding {
	var out []finding
	for _, file := range pkg.Syntax {
		filename := pkg.Fset.Position(file.Pos()).Filename
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "resolved" && sel.Sel.Name != "Resolved" {
				return true
			}
			t := pkg.TypesInfo.TypeOf(sel)
			if t == nil || t.String() != targetType {
				return true
			}
			pos := pkg.Fset.Position(sel.Pos())
			line := sourceLine(lineCache, pos.Filename, pos.Line)
			if strings.Contains(line, ignoreMarker) {
				return true
			}
			out = append(out, finding{file: pos.Filename, line: pos.Line, text: strings.TrimSpace(line)})
			return true
		})
	}
	return out
}

// sourceLine returns line n (1-based) of filename, reading and caching the
// whole file split into lines on first access.
func sourceLine(cache map[string][]string, filename string, n int) string {
	lines, ok := cache[filename]
	if !ok {
		lines = nil
		if f, err := os.Open(filename); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				lines = append(lines, sc.Text())
			}
			f.Close()
		}
		cache[filename] = lines
	}
	if n < 1 || n > len(lines) {
		return ""
	}
	return lines[n-1]
}
