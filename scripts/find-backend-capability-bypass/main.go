// Command find-backend-capability-bypass reports code that decides what a
// delegated backend can do some way other than its declared Spec (#2154).
//
// Every backend difference is declared in the backend's delegator.Spec and
// asked through Spec.Supports / delegator.As[T] — see the banner in
// internal/delegator/capabilities.go. Outside internal/delegator/... (and
// outside _test.go files) this tool flags the three ways around that:
//
//  1. a type assertion or type switch on a value whose static type is
//     delegator.Delegator — reach optional methods with delegator.As[T], whose
//     interface must be a registered Capability;
//  2. a comparison of a string against a registered backend name ("codex"),
//     via ==, !=, a switch case, or strings.HasPrefix/HasSuffix/EqualFold/
//     Contains — add a Spec field or a Capability instead;
//  3. an import of a concrete backend package (one that registers a Spec) —
//     only internal/delegator/all may import them all.
//
// Inside each backend package (the one declaring a Spec, and its
// subpackages) it also checks the per-backend config schema (#2178):
//
//  4. every StartOptions field the package reads, and every constant key it
//     reads from its backend_config map (an index of a map[string]any named
//     cfg, or a delegator config accessor such as SkipPermissions), must be
//     declared in its Spec's StartFields / ConfigKeys; and every declared one
//     must be read. Both lists must be literals of constant strings.
//
// Backend names are not hard-coded: the tool reads them from every
// delegator.Spec composite literal it loads (Name must be a constant), and
// exits 2 if it finds none, since it would then be blind to rule 2 and 3.
//
// A legitimate exception carries a same-line `backend-cap:ignore: <reason>`
// comment. Runs as part of `make lint`.
//
// Run with: go run ./scripts/find-backend-capability-bypass/... [patterns]
// (defaults to ./... from the invoking directory)
//
// Exit codes:
//   - 0: no findings (or all suppressed)
//   - 1: at least one unsuppressed finding
//   - 2: tool error (load failure, no Spec found)
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

const (
	delegatorPkg  = "foci/internal/delegator"
	delegatorType = delegatorPkg + ".Delegator"
	specType      = delegatorPkg + ".Spec"
	allPkg        = delegatorPkg + "/all"
	ignoreMarker  = "backend-cap:ignore"
	pointer       = "declare backend differences in internal/delegator/capabilities.go (Spec / Capability)"
)

type finding struct {
	file string
	line int
	msg  string
	text string
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: find-backend-capability-bypass <package patterns>\n")
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

	names, backendPkgs := discoverBackends(pkgs)
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "find-backend-capability-bypass: no delegator.Spec literal with a constant Name found — load the backend packages (./...)")
		os.Exit(2)
	}

	lineCache := map[string][]string{}
	var findings []finding
	for _, pkg := range pkgs {
		if pkg.PkgPath == delegatorPkg || strings.HasPrefix(pkg.PkgPath, delegatorPkg+"/") {
			continue
		}
		findings = append(findings, scanPackage(pkg, names, backendPkgs, lineCache)...)
	}
	findings = append(findings, checkSchemas(pkgs, lineCache)...)

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].file != findings[j].file {
			return findings[i].file < findings[j].file
		}
		return findings[i].line < findings[j].line
	})
	for _, f := range findings {
		fmt.Printf("%s:%d: %s\n\t%s\n", f.file, f.line, f.msg, f.text)
	}
	if len(findings) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d backend capability bypass(es) found — %s; a justified exception takes a same-line `%s: <reason>`\n",
			len(findings), pointer, ignoreMarker)
		os.Exit(1)
	}
}

// discoverBackends returns the registered backend names (the constant Name of
// every delegator.Spec literal) and the packages declaring those literals.
func discoverBackends(pkgs []*packages.Package) (map[string]bool, map[string]bool) {
	names := map[string]bool{}
	backendPkgs := map[string]bool{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			if strings.HasSuffix(pkg.Fset.Position(file.Pos()).Filename, "_test.go") {
				continue
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				t := pkg.TypesInfo.TypeOf(lit)
				if t == nil || t.String() != specType {
					return true
				}
				for _, el := range lit.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if id, ok := kv.Key.(*ast.Ident); !ok || id.Name != "Name" {
						continue
					}
					if tv, ok := pkg.TypesInfo.Types[kv.Value]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
						names[constant.StringVal(tv.Value)] = true
						backendPkgs[pkg.PkgPath] = true
					}
				}
				return true
			})
		}
	}
	return names, backendPkgs
}

func scanPackage(pkg *packages.Package, names, backendPkgs map[string]bool, lineCache map[string][]string) []finding {
	var out []finding
	report := func(pos token.Pos, msg string) {
		p := pkg.Fset.Position(pos)
		line := sourceLine(lineCache, p.Filename, p.Line)
		if strings.Contains(line, ignoreMarker) {
			return
		}
		out = append(out, finding{file: p.Filename, line: p.Line, msg: msg, text: strings.TrimSpace(line)})
	}
	isDelegator := func(e ast.Expr) bool {
		t := pkg.TypesInfo.TypeOf(e)
		return t != nil && types.TypeString(t, nil) == delegatorType
	}
	backendName := func(e ast.Expr) (string, bool) {
		// A constant of the delegator package itself (e.g. ConfigFamilyClaudeCode)
		// is declared vocabulary, even when its value matches a backend name.
		var id *ast.Ident
		switch v := e.(type) {
		case *ast.Ident:
			id = v
		case *ast.SelectorExpr:
			id = v.Sel
		}
		if id != nil {
			if c, ok := pkg.TypesInfo.Uses[id].(*types.Const); ok && c.Pkg() != nil && c.Pkg().Path() == delegatorPkg {
				return "", false
			}
		}
		tv, ok := pkg.TypesInfo.Types[e]
		if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
			return "", false
		}
		s := constant.StringVal(tv.Value)
		return s, names[s]
	}

	for _, file := range pkg.Syntax {
		if strings.HasSuffix(pkg.Fset.Position(file.Pos()).Filename, "_test.go") {
			continue
		}
		if pkg.PkgPath != allPkg {
			for _, imp := range file.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				if backendPkgs[path] {
					report(imp.Pos(), fmt.Sprintf("imports backend package %s — only internal/delegator/all may; %s", path, pointer))
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.TypeAssertExpr:
				if x.Type != nil && isDelegator(x.X) {
					report(x.Pos(), "type assertion on a delegator.Delegator — use delegator.As[T] with a registered capability interface; "+pointer)
				}
			case *ast.TypeSwitchStmt:
				var operand ast.Expr
				switch a := x.Assign.(type) {
				case *ast.AssignStmt:
					if len(a.Rhs) == 1 {
						if ta, ok := a.Rhs[0].(*ast.TypeAssertExpr); ok {
							operand = ta.X
						}
					}
				case *ast.ExprStmt:
					if ta, ok := a.X.(*ast.TypeAssertExpr); ok {
						operand = ta.X
					}
				}
				if operand != nil && isDelegator(operand) {
					report(x.Pos(), "type switch on a delegator.Delegator — use delegator.As[T] with a registered capability interface; "+pointer)
				}
			case *ast.BinaryExpr:
				if x.Op != token.EQL && x.Op != token.NEQ {
					return true
				}
				for _, side := range []ast.Expr{x.X, x.Y} {
					if name, ok := backendName(side); ok {
						report(x.Pos(), fmt.Sprintf("compares against backend name %q — add a Spec field or a Capability instead; %s", name, pointer))
						break
					}
				}
			case *ast.SwitchStmt:
				if x.Tag == nil {
					return true
				}
				for _, stmt := range x.Body.List {
					cc, ok := stmt.(*ast.CaseClause)
					if !ok {
						continue
					}
					for _, e := range cc.List {
						if name, ok := backendName(e); ok {
							report(e.Pos(), fmt.Sprintf("switch case on backend name %q — add a Spec field or a Capability instead; %s", name, pointer))
						}
					}
				}
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				obj, ok := pkg.TypesInfo.Uses[sel.Sel].(*types.Func)
				if !ok || obj.Pkg() == nil || obj.Pkg().Path() != "strings" {
					return true
				}
				switch obj.Name() {
				case "HasPrefix", "HasSuffix", "EqualFold", "Contains":
				default:
					return true
				}
				for _, arg := range x.Args {
					if name, ok := backendName(arg); ok {
						report(x.Pos(), fmt.Sprintf("strings.%s against backend name %q — add a Spec field or a Capability instead; %s", obj.Name(), name, pointer))
						break
					}
				}
			}
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
		if f, err := os.Open(filename); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				lines = append(lines, sc.Text())
			}
			_ = f.Close()
		}
		cache[filename] = lines
	}
	if n < 1 || n > len(lines) {
		return ""
	}
	return lines[n-1]
}
