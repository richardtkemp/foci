// Command find-unscoped-logging flags every package-level
// log.{Debugf,Infof,Warnf,Errorf}("component", ...) call — logging that passes
// the component string inline at the call site instead of going through a
// shared *foci/internal/log.ComponentLogger.
//
// The convention this enforces: the component name lives in exactly one place —
// a ComponentLogger constructed once and reused. That is either a scoped logger
// on a type (func (b *Backend) logger() *log.ComponentLogger returning
// log.NewComponentLogger("ccstream:"+b.label), so lines name their owning
// agent/session) or a package-level var for a process-global component
// (var clog = log.NewComponentLogger("main")). Every log line then goes through
// <logger>.Debugf/Infof/Warnf/Errorf(...), which take no component argument.
//
// What is flagged: any call to the package-level log.Debugf/Infof/Warnf/Errorf
// funcs (whose first arg is the component string), anywhere in non-test code.
// Calls on a *ComponentLogger value (b.logger().Debugf(...), clog.Infof(...))
// have the same method names but a different receiver and are NOT flagged.
//
// Runs as part of `make lint`. A finding is fixed by routing through a shared
// logger. A genuinely unavoidable inline component (e.g. a one-shot free
// function whose component is built from a dynamic id with no logger to hold)
// is suppressed with a same-line `unscoped-log:ignore: <reason>` comment.
//
// Run with: go run ./scripts/find-unscoped-logging/... [package patterns]
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
	"go/types"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// logPkgPath is the import path of foci's logging package.
const logPkgPath = "foci/internal/log"

// ignoreMarker suppresses a finding when present on the same source line.
const ignoreMarker = "unscoped-log:ignore"

// componentFuncs are the package-level log functions whose first argument is
// the component string — the ones a shared *ComponentLogger replaces.
var componentFuncs = map[string]bool{
	"Debugf": true, "Infof": true, "Warnf": true, "Errorf": true,
}

type finding struct {
	file string
	line int
	text string
}

func main() {
	verbose := flag.Bool("v", false, "log per-package progress")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: find-unscoped-logging [-v] <package patterns>\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	patterns := flag.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports,
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
		fmt.Fprintf(os.Stderr, "\n%d unscoped log call(s) found — route through the type's logger()/Logger()\n", len(findings))
		os.Exit(1)
	}
}

// scanPackage flags every package-level component-taking log call in pkg's
// non-test files. Any such call passes the component string inline instead of
// going through a shared *log.ComponentLogger, which is what this tool forbids.
func scanPackage(pkg *packages.Package, lineCache map[string][]string) []finding {
	var out []finding
	for _, file := range pkg.Syntax {
		filename := pkg.Fset.Position(file.Pos()).Filename
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isComponentLogCall(pkg, call) {
				return true
			}
			pos := pkg.Fset.Position(call.Pos())
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

// isComponentLogCall reports whether call is a package-level
// log.{Debugf,Infof,Warnf,Errorf}(...) call against the foci/internal/log
// package (not a *ComponentLogger method call, and not a same-named func from
// another package).
func isComponentLogCall(pkg *packages.Package, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !componentFuncs[sel.Sel.Name] {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	pkgName, ok := pkg.TypesInfo.Uses[ident].(*types.PkgName)
	if !ok {
		return false
	}
	return pkgName.Imported().Path() == logPkgPath
}

// sourceLine returns line n (1-based) of filename, reading and caching the
// whole file split into lines on first access.
func sourceLine(cache map[string][]string, filename string, n int) string {
	lines, ok := cache[filename]
	if !ok {
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
	if n >= 1 && n <= len(lines) {
		return lines[n-1]
	}
	return ""
}
