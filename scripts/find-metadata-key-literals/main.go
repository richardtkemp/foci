// Command find-metadata-key-literals enforces that session_metadata keys come
// from the internal/session MetaKey* registry: no non-test call to the
// session_metadata API may pass a bare string literal as the key.
//
// Checked calls (any receiver), with the key argument's position:
//
//	SetSessionMetadata(sessionKey, key, value)   key = arg 1
//	GetSessionMetadata(sessionKey, key)          key = arg 1
//	DeleteSessionMetadata(sessionKey, key)       key = arg 1
//	SessionKeysWithMetadata(key)                 key = arg 0
//
// Every non-test .go file under the root is parsed; dot-dirs, vendor,
// testdata and bin are skipped.
//
// Why a lint tool rather than a go test: this check used to be
// internal/session's TestSessionMetadataCallSitesUseRegistry, which read every
// .go file in the repo, so Go keyed that package's cached test result on all
// of them and any change anywhere re-ran internal/session (#2349). Runs as
// part of `make lint`. Stdlib-only, so it lives in the main module.
//
// Usage: find-metadata-key-literals [root] (default ".", the repo root)
//
// Exit codes: 0 = clean, 1 = literal keys found, 2 = tool error.
package main

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// keyArgIndex maps each session_metadata API method to its key argument.
var keyArgIndex = map[string]int{
	"SetSessionMetadata":      1,
	"GetSessionMetadata":      1,
	"DeleteSessionMetadata":   1,
	"SessionKeysWithMetadata": 0,
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fset := flag.NewFlagSet("find-metadata-key-literals", flag.ContinueOnError)
	fset.SetOutput(os.Stderr)
	fset.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: find-metadata-key-literals [root]")
		fmt.Fprintln(os.Stderr, "Flags string-literal keys passed to the session_metadata API outside _test.go files.")
	}
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	root := "."
	switch fset.NArg() {
	case 0:
	case 1:
		root = fset.Arg(0)
	default:
		fset.Usage()
		return 2
	}

	findings, err := scanTree(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "find-metadata-key-literals: %v\n", err)
		return 2
	}
	for _, f := range findings {
		fmt.Printf("%s: string-literal session_metadata key — use a session.MetaKey* constant\n", f)
	}
	if len(findings) > 0 {
		fmt.Printf("\n%d session_metadata call(s) with a string-literal key\n", len(findings))
		return 1
	}
	return 0
}

// scanTree walks root and returns "file:line:col" for every offending call in
// a non-test .go file, sorted.
func scanTree(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			// Never prune the root itself: its base may be "." or "..".
			if path == root {
				return nil
			}
			if strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || name == "bin" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found, err := scanFile(path, src)
		if err != nil {
			return err
		}
		out = append(out, found...)
		return nil
	})
	sort.Strings(out)
	return out, err
}

// scanFile parses one Go source file and returns the position of every
// session_metadata API call whose key argument is a string literal.
func scanFile(path string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		idx, ok := keyArgIndex[sel.Sel.Name]
		if !ok || idx >= len(call.Args) {
			return true
		}
		if lit, ok := call.Args[idx].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			out = append(out, fset.Position(call.Pos()).String())
		}
		return true
	})
	return out, nil
}
