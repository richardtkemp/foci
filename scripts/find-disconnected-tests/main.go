// Command find-disconnected-tests reports Test* functions whose bodies
// (transitively, via test helpers in the same package) do not reach any
// identifier defined in the package under test. Such tests only exercise
// test infrastructure (mocks, fixtures, helpers) and therefore cannot
// catch a regression in production code — even if they pass.
//
// This is the test-side counterpart to `deadcode ./...`:
//
//   - `deadcode ./...` finds production code with no production caller (the
//     production half of a useless test/code pair).
//   - This tool finds tests that don't (transitively) touch production
//     code (the test half of a useless test/code pair).
//
// Run with: go run ./scripts/find-disconnected-tests/... ./...
//
// Exit codes:
//   - 0: no findings
//   - 1: at least one disconnected test reported
//   - 2: tool error (load failure, etc.)
//
// Algorithm:
//   - Load each package with Tests: true.
//   - Collect "production identifiers": every *types.Object defined in a
//     non-test file of the package (functions, methods, types, vars,
//     consts).
//   - For every function declared anywhere in the package (test files
//     included), build a per-function "references" set — every object the
//     body uses, with same-package function references additionally
//     tracked as call-graph edges.
//   - For each Test* function, do a BFS over the call-graph edges. At
//     each visited function, check whether its references intersect the
//     production identifier set. If any do, the test (transitively)
//     touches production. If the BFS completes without ever hitting a
//     production object, the test is disconnected.
//
// Limitations:
//   - Only same-package call-graph edges are followed. A test that calls
//     a helper in a different package which in turn touches production
//     will be flagged as disconnected. The fix is to add cross-package
//     edges, which requires loading all packages once and stitching their
//     callgraphs together — left as a future extension.
//   - External-test packages (`package foo_test`) are skipped with a
//     warning; their production identifiers live in a separately-loaded
//     package this tool doesn't yet stitch in. The foci tree currently has
//     2 such files.
//   - "References" includes types, vars, and consts in addition to
//     functions, so a test that just declares `var x prodPackage.Foo`
//     counts as touching production even if it never calls a method.
//   - Tests that fully delegate to test fixtures *and* whose fixtures
//     never reach production are correctly flagged. Tests whose only
//     verification is on values they themselves constructed (the
//     tautological pattern) cannot be detected by this tool — mutation
//     testing is the right tool for that.
//
// Suppression:
//
// Two false-positive shapes have known opt-outs:
//
//  1. TestMain(m *testing.M) is the test setup/teardown hook, not a test
//     itself. It is excluded automatically.
//
//  2. Black-box / integration tests that exec a subprocess or hit an HTTP
//     server cannot reach package symbols by reference and would always
//     fire. Mark them with a doc comment containing the directive
//     "disconnected-test-ok" (optionally followed by a reason), e.g.:
//
//     // TestCLIIntegration spawns the built binary and asserts on
//     // its stdout — it cannot reference package symbols directly.
//     //
//     // disconnected-test-ok: black-box CLI integration test
//     func TestCLIIntegration(t *testing.T) { ... }
//
// A test in cmd/X that only references identifiers from imported packages
// (but not from cmd/X itself) is a *real* finding — it is testing those
// imported packages and should live with them. Move it rather than
// suppress it.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/types"
	"os"
	"strings"

	"golang.org/x/tools/go/packages"
)

func main() {
	verbose := flag.Bool("v", false, "log per-package progress")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: find-disconnected-tests [-v] <package patterns>\n")
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
		Tests: true,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(2)
	}
	if packages.PrintErrors(pkgs) > 0 {
		os.Exit(2)
	}

	totalFindings := 0
	for _, pkg := range pkgs {
		findings := analysePackage(pkg, *verbose)
		totalFindings += findings
	}

	if totalFindings > 0 {
		fmt.Fprintf(os.Stderr, "\n%d disconnected test(s) found\n", totalFindings)
		os.Exit(1)
	}
}

// funcInfo records what one function declaration in the package references
// and which same-package functions it calls (call-graph edges for BFS).
type funcInfo struct {
	decl        *ast.FuncDecl
	refs        map[types.Object]bool
	calls       map[*types.Func]bool
	isTestEntry bool // true if this is a Test* function in a _test.go file
}

// analysePackage walks one package's syntax, builds the production
// identifier set + per-function reference map, then BFSes from each Test*
// to check whether it transitively reaches any production identifier.
func analysePackage(pkg *packages.Package, verbose bool) int {
	files := pkg.CompiledGoFiles
	syntax := pkg.Syntax
	if len(files) != len(syntax) {
		return 0
	}

	hasInternalTest := false
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			hasInternalTest = true
			break
		}
	}
	if !hasInternalTest {
		return 0
	}

	// External-test packages are skipped — their production identifiers
	// live in a separately-loaded package this tool doesn't stitch in yet.
	if strings.HasSuffix(pkg.Name, "_test") {
		fmt.Fprintf(os.Stderr, "skip external test package %s (not yet supported)\n", pkg.PkgPath)
		return 0
	}

	prodObjs := collectProductionObjects(pkg, files, syntax)
	if len(prodObjs) == 0 {
		return 0
	}

	funcMap := buildFuncMap(pkg, files, syntax)
	if verbose {
		fmt.Fprintf(os.Stderr, "scan %s (%d prod identifiers, %d funcs)\n",
			pkg.PkgPath, len(prodObjs), len(funcMap))
	}

	findings := 0
	for funcObj, fi := range funcMap {
		if !fi.isTestEntry {
			continue
		}
		if reachesProduction(funcObj, funcMap, prodObjs) {
			continue
		}
		pos := pkg.Fset.Position(fi.decl.Pos())
		fmt.Printf("%s:%d:%d: %s does not reach any identifier from %s\n",
			pos.Filename, pos.Line, pos.Column, fi.decl.Name.Name, pkg.PkgPath)
		findings++
	}
	return findings
}

// collectProductionObjects walks non-test files and returns the set of
// every top-level identifier declared there.
func collectProductionObjects(pkg *packages.Package, files []string, syntax []*ast.File) map[types.Object]bool {
	prod := map[types.Object]bool{}
	for i, syn := range syntax {
		if strings.HasSuffix(files[i], "_test.go") {
			continue
		}
		for _, decl := range syn.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if obj := pkg.TypesInfo.Defs[d.Name]; obj != nil {
					prod[obj] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.ValueSpec:
						for _, name := range s.Names {
							if obj := pkg.TypesInfo.Defs[name]; obj != nil {
								prod[obj] = true
							}
						}
					case *ast.TypeSpec:
						if obj := pkg.TypesInfo.Defs[s.Name]; obj != nil {
							prod[obj] = true
						}
					}
				}
			}
		}
	}
	return prod
}

// buildFuncMap walks every FuncDecl in the package (test and non-test
// files) and returns a map from the function's *types.Func to its
// reference set + same-package call edges.
func buildFuncMap(pkg *packages.Package, files []string, syntax []*ast.File) map[*types.Func]*funcInfo {
	out := map[*types.Func]*funcInfo{}
	for i, syn := range syntax {
		isTestFile := strings.HasSuffix(files[i], "_test.go")
		for _, decl := range syn.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			obj := pkg.TypesInfo.Defs[fn.Name]
			fnObj, ok := obj.(*types.Func)
			if !ok {
				continue
			}
			fi := &funcInfo{
				decl:        fn,
				refs:        map[types.Object]bool{},
				calls:       map[*types.Func]bool{},
				isTestEntry: isTestFile && isTestEntryDecl(fn),
			}
			if fn.Body != nil {
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					ident, ok := n.(*ast.Ident)
					if !ok {
						return true
					}
					o := pkg.TypesInfo.Uses[ident]
					if o == nil {
						return true
					}
					fi.refs[o] = true
					if f, ok := o.(*types.Func); ok && f.Pkg() == pkg.Types {
						fi.calls[f] = true
					}
					return true
				})
			}
			out[fnObj] = fi
		}
	}
	return out
}

// isTestEntryDecl reports whether fn is a Test* function this audit cares
// about. It excludes:
//   - TestMain(m *testing.M) — that's the test setup/teardown hook, not
//     an actual test.
//   - any function whose doc comment contains the "disconnected-test-ok"
//     directive — used by black-box / integration tests that legitimately
//     can't reference package symbols by name.
func isTestEntryDecl(fn *ast.FuncDecl) bool {
	if fn.Recv != nil {
		return false
	}
	if !strings.HasPrefix(fn.Name.Name, "Test") || fn.Name.Name == "Test" {
		return false
	}
	if fn.Name.Name == "TestMain" {
		return false
	}
	rest := fn.Name.Name[len("Test"):]
	if rest[0] != '_' && (rest[0] < 'A' || rest[0] > 'Z') {
		return false
	}
	if fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		return false
	}
	if hasSuppressionDirective(fn.Doc) {
		return false
	}
	return true
}

// hasSuppressionDirective reports whether the doc comment carries the
// "disconnected-test-ok" opt-out marker. The marker may be followed by an
// optional explanation; we don't parse the rest, just look for the prefix
// on any line of the comment group.
func hasSuppressionDirective(doc *ast.CommentGroup) bool {
	if doc == nil {
		return false
	}
	for _, c := range doc.List {
		text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
		text = strings.TrimSpace(strings.TrimPrefix(text, "/*"))
		text = strings.TrimSpace(strings.TrimSuffix(text, "*/"))
		if strings.HasPrefix(text, "disconnected-test-ok") {
			return true
		}
	}
	return false
}

// reachesProduction BFSes from start through same-package call edges and
// returns true if any visited function's reference set intersects prodObjs.
func reachesProduction(start *types.Func, funcMap map[*types.Func]*funcInfo, prodObjs map[types.Object]bool) bool {
	visited := map[*types.Func]bool{}
	queue := []*types.Func{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		fi := funcMap[cur]
		if fi == nil {
			continue
		}
		for ref := range fi.refs {
			if prodObjs[ref] {
				return true
			}
		}
		for callee := range fi.calls {
			if !visited[callee] {
				queue = append(queue, callee)
			}
		}
	}
	return false
}
