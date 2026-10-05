package main

// schema.go implements rule 4: each backend package's StartOptions and
// backend_config reads match its Spec's StartFields and ConfigKeys (#2178).

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

const (
	startOptionsType = delegatorPkg + ".StartOptions"
	schemaPointer    = "declare it in the backend's Spec (internal/delegator/config_schema.go)"
)

// cfgAccessors are the delegator functions that read a backend_config key
// from the cfg map they are passed, and the key each reads.
var cfgAccessors = map[string]string{
	"SkipPermissions": "skip_permissions",
}

// declaredList is one Spec list field: each declared name and where.
type declaredList struct {
	set   map[string]token.Pos
	field token.Pos // the field's key, for "missing list" findings
}

type backendSchema struct {
	pkg         *packages.Package
	specPos     token.Pos
	configKeys  declaredList
	startFields declaredList
	readKeys    map[string]token.Pos
	readFields  map[string]token.Pos
}

func checkSchemas(pkgs []*packages.Package, lineCache map[string][]string) []finding {
	var out []finding
	report := func(pkg *packages.Package, pos token.Pos, msg string) {
		p := pkg.Fset.Position(pos)
		line := sourceLine(lineCache, p.Filename, p.Line)
		if strings.Contains(line, ignoreMarker) {
			return
		}
		out = append(out, finding{file: p.Filename, line: p.Line, msg: msg, text: strings.TrimSpace(line)})
	}

	schemas := map[string]*backendSchema{}
	for _, pkg := range pkgs {
		forEachProdFile(pkg, func(file *ast.File) {
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if t := pkg.TypesInfo.TypeOf(lit); t == nil || t.String() != specType {
					return true
				}
				// Only a backend's Spec (constant Name, as discoverBackends) —
				// not e.g. the zero Spec an API agent gets.
				bs := &backendSchema{pkg: pkg, specPos: lit.Pos(), readKeys: map[string]token.Pos{}, readFields: map[string]token.Pos{}}
				named := false
				for _, el := range lit.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					id, ok := kv.Key.(*ast.Ident)
					if !ok {
						continue
					}
					var dst *declaredList
					switch id.Name {
					case "Name":
						if tv, ok := pkg.TypesInfo.Types[kv.Value]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
							named = true
						}
						continue
					case "ConfigKeys":
						dst = &bs.configKeys
					case "StartFields":
						dst = &bs.startFields
					default:
						continue
					}
					dst.field = kv.Pos()
					dst.set = map[string]token.Pos{}
					list, ok := kv.Value.(*ast.CompositeLit)
					if !ok {
						report(pkg, kv.Pos(), fmt.Sprintf("Spec.%s must be a []string literal of constants, so it can be checked", id.Name))
						continue
					}
					for _, e := range list.Elts {
						tv, ok := pkg.TypesInfo.Types[e]
						if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
							report(pkg, e.Pos(), fmt.Sprintf("Spec.%s element is not a constant string", id.Name))
							continue
						}
						dst.set[constant.StringVal(tv.Value)] = e.Pos()
					}
				}
				if named {
					schemas[pkg.PkgPath] = bs
				}
				return false
			})
		})
	}

	// Attribute every package's reads to the backend it belongs to.
	for _, pkg := range pkgs {
		bs := owningSchema(schemas, pkg.PkgPath)
		if bs == nil {
			continue
		}
		forEachProdFile(pkg, func(file *ast.File) {
			collectReads(pkg, file, bs)
		})
	}

	paths := make([]string, 0, len(schemas))
	for p := range schemas {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		bs := schemas[path]
		compare := func(kind, field string, declared declaredList, read map[string]token.Pos) {
			if declared.set == nil {
				if len(read) > 0 {
					report(bs.pkg, bs.specPos, fmt.Sprintf("Spec has no %s but the package reads %s %s — %s", field, kind, strings.Join(sortedKeys(read), ", "), schemaPointer))
				}
				return
			}
			for _, name := range sortedKeys(read) {
				if _, ok := declared.set[name]; !ok {
					report(bs.pkg, read[name], fmt.Sprintf("reads %s %q, which Spec.%s does not declare — %s", kind, name, field, schemaPointer))
				}
			}
			for _, name := range sortedKeys(declared.set) {
				if _, ok := read[name]; !ok {
					report(bs.pkg, declared.set[name], fmt.Sprintf("Spec.%s declares %s %q, which the package never reads — remove it", field, kind, name))
				}
			}
		}
		compare("StartOptions field", "StartFields", bs.startFields, bs.readFields)
		compare("backend_config key", "ConfigKeys", bs.configKeys, bs.readKeys)
	}
	return out
}

// owningSchema returns the schema of the backend package path is, or is a
// subpackage of.
func owningSchema(schemas map[string]*backendSchema, path string) *backendSchema {
	for p, bs := range schemas {
		if path == p || strings.HasPrefix(path, p+"/") {
			return bs
		}
	}
	return nil
}

func forEachProdFile(pkg *packages.Package, fn func(*ast.File)) {
	for _, file := range pkg.Syntax {
		if strings.HasSuffix(pkg.Fset.Position(file.Pos()).Filename, "_test.go") {
			continue
		}
		fn(file)
	}
}

// collectReads records the StartOptions fields and backend_config keys file
// reads. Building a StartOptions literal is not a read.
func collectReads(pkg *packages.Package, file *ast.File, bs *backendSchema) {
	note := func(m map[string]token.Pos, name string, pos token.Pos) {
		if _, ok := m[name]; !ok {
			m[name] = pos
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			sel := pkg.TypesInfo.Selections[x]
			if sel == nil || sel.Kind() != types.FieldVal {
				return true
			}
			recv := sel.Recv()
			if p, ok := recv.(*types.Pointer); ok {
				recv = p.Elem()
			}
			if recv.String() == startOptionsType {
				note(bs.readFields, sel.Obj().Name(), x.Sel.Pos())
			}
		case *ast.IndexExpr:
			if !isCfgMap(pkg, x.X) {
				return true
			}
			tv, ok := pkg.TypesInfo.Types[x.Index]
			if ok && tv.Value != nil && tv.Value.Kind() == constant.String {
				note(bs.readKeys, constant.StringVal(tv.Value), x.Pos())
			}
		case *ast.CallExpr:
			var id *ast.Ident
			switch f := x.Fun.(type) {
			case *ast.SelectorExpr:
				id = f.Sel
			case *ast.Ident:
				id = f
			}
			if id == nil {
				return true
			}
			fn, ok := pkg.TypesInfo.Uses[id].(*types.Func)
			if !ok || fn.Pkg() == nil || fn.Pkg().Path() != delegatorPkg {
				return true
			}
			if key, ok := cfgAccessors[fn.Name()]; ok {
				note(bs.readKeys, key, x.Pos())
			}
		}
		return true
	})
}

// isCfgMap reports whether e is the backend's config map: a map[string]any
// named cfg (the Constructor parameter, or the field it is stored in).
func isCfgMap(pkg *packages.Package, e ast.Expr) bool {
	var name string
	switch v := e.(type) {
	case *ast.Ident:
		name = v.Name
	case *ast.SelectorExpr:
		name = v.Sel.Name
	default:
		return false
	}
	if name != "cfg" {
		return false
	}
	t := pkg.TypesInfo.TypeOf(e)
	if t == nil {
		return false
	}
	m, ok := t.Underlying().(*types.Map)
	if !ok {
		return false
	}
	return types.TypeString(m.Key(), nil) == "string" && types.IsInterface(m.Elem())
}

func sortedKeys(m map[string]token.Pos) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
