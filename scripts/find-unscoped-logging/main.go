// Command find-unscoped-logging flags "non-component-shared" logging: a
// package-level log.{Debugf,Infof,Warnf,Errorf}("component", ...) call made
// from inside a method whose receiver type already owns a scoped logger — a
// logger()/Logger() method returning *foci/internal/log.ComponentLogger.
//
// The convention this enforces: a type that carries an agent/session/component
// identity exposes a scoped logger (e.g. func (b *Backend) logger()
// *log.ComponentLogger returning log.NewComponentLogger("ccstream:"+b.label)),
// and every log line from that type goes through it — so the line names its
// owning agent/session ([ccstream:clutch]) instead of a bare [ccstream] shared
// across every concurrent session. Reaching for the package-level log.Xf with a
// literal component from such a method bypasses that logger and drops the id.
//
// Scope of detection (deliberately high-confidence, low-false-positive):
//   - ONLY methods whose receiver type has a logger()/Logger() scoped logger.
//     A bare log.Xf in a free function, or on a type with no scoped logger
//     (process-global monitors, package init), is NOT flagged — there is no
//     shared logger to route through, so it is a different (often legitimate)
//     case this tool intentionally leaves alone.
//   - ONLY the package-level component-taking funcs (Debugf/Infof/Warnf/Errorf,
//     whose first arg is the component string). Calls on a *ComponentLogger
//     value (b.logger().Debugf(...)) have the same method names but a different
//     receiver and are correctly ignored.
//
// A type's methods must live in the type's own package, so the "has a scoped
// logger" set is built per-package and matched against methods in that same
// package — no cross-package type identity concerns.
//
// Runs as part of `make lint`. A finding is either a real bypass (route it
// through the type's logger()) or an intentional exception (a genuinely
// process-global line emitted from a method that happens to hang off a
// logger-owning type) — annotate the latter with a same-line
// `unscoped-log:ignore: <reason>` comment to suppress it.
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

// componentLoggerType is the go/types String() form of the scoped logger a
// logger()/Logger() method must return to count as a scoped logger owner.
const componentLoggerType = "*foci/internal/log.ComponentLogger"

// ignoreMarker suppresses a finding when present on the same source line.
const ignoreMarker = "unscoped-log:ignore"

// componentFuncs are the package-level log functions whose first argument is
// the component string — the ones a scoped logger replaces.
var componentFuncs = map[string]bool{
	"Debugf": true, "Infof": true, "Warnf": true, "Errorf": true,
}

// scopedLoggerMethods are the method names that, when returning a
// *log.ComponentLogger, mark their receiver type as owning a scoped logger.
var scopedLoggerMethods = map[string]bool{
	"logger": true, "Logger": true,
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

// scanPackage finds, per package, the set of receiver types that own a scoped
// logger, then flags package-level component-taking log calls in methods of
// those types.
func scanPackage(pkg *packages.Package, lineCache map[string][]string) []finding {
	scoped := scopedLoggerOwners(pkg)
	if len(scoped) == 0 {
		return nil
	}

	var out []finding
	for _, file := range pkg.Syntax {
		filename := pkg.Fset.Position(file.Pos()).Filename
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil {
				continue
			}
			owner := receiverTypeName(pkg, fn)
			if owner == nil || !scoped[owner] {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
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
	}
	return out
}

// scopedLoggerOwners returns the set of named types in pkg that declare a
// logger()/Logger() method returning *log.ComponentLogger.
func scopedLoggerOwners(pkg *packages.Package) map[*types.TypeName]bool {
	owners := map[*types.TypeName]bool{}
	for _, file := range pkg.Syntax {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !scopedLoggerMethods[fn.Name.Name] {
				continue
			}
			obj, _ := pkg.TypesInfo.Defs[fn.Name].(*types.Func)
			if obj == nil {
				continue
			}
			sig, _ := obj.Type().(*types.Signature)
			if sig == nil || sig.Results().Len() != 1 {
				continue
			}
			if sig.Results().At(0).Type().String() != componentLoggerType {
				continue
			}
			if tn := receiverTypeNameFromSig(sig); tn != nil {
				owners[tn] = true
			}
		}
	}
	return owners
}

// receiverTypeName returns the *types.TypeName of fn's receiver base type
// (stripping a pointer), or nil.
func receiverTypeName(pkg *packages.Package, fn *ast.FuncDecl) *types.TypeName {
	obj, _ := pkg.TypesInfo.Defs[fn.Name].(*types.Func)
	if obj == nil {
		return nil
	}
	sig, _ := obj.Type().(*types.Signature)
	if sig == nil {
		return nil
	}
	return receiverTypeNameFromSig(sig)
}

func receiverTypeNameFromSig(sig *types.Signature) *types.TypeName {
	recv := sig.Recv()
	if recv == nil {
		return nil
	}
	t := recv.Type()
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok {
		return nil
	}
	return named.Obj()
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
