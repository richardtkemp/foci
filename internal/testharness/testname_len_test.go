package testharness

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Unix domain sockets cap the path at sun_path (108 bytes incl. NUL → 107
// usable). Under `make integration`, t.TempDir() embeds the test name:
//
//	/tmp/foci/integration-<unix10>/<TestName><rand10>/001/...
//	└──────── 33 ────────────────┘└─ name ┘└── 10 ──┘└ 4 ┘
//
// The enforced invariant (MaxIntegTestNameLen, defined with the harness's
// socket allocations in gateway.go) is that t.TempDir() ITSELF stays within
// sun_path. That is necessary (not sufficient) for any socket ever placed
// under a test's TempDir — real sockets need headroom on top, which is why
// harness sockets live in short, name-independent /tmp/fcs*//tmp/fgw* dirs
// instead (and why the harness fails fast if the gateway socket doesn't
// bind). This gate stops the unbounded-name-growth direction of the hazard:
// the failure only manifests under make integration, passing in isolation
// (TODO #804 cost a debugging session).

// TestIntegrationTestNamesFitSunPath is the build-time gate for the sun_path
// hazard: it parses every test function name in test/integration and fails
// on any name long enough that the test's TempDir itself would exceed
// sun_path under make integration. Fix by shortening the test name; if a
// test needs a socket, allocate it in a short, name-independent /tmp dir
// like the harness does (gwSock/controlSock).
func TestIntegrationTestNamesFitSunPath(t *testing.T) {
	dir := filepath.Join("..", "..", "test", "integration")
	fset := token.NewFileSet()
	// Parse without build tags: the integration tag gates compilation, not
	// parsing, so the gate sees every test file.
	pkgs, err := parser.ParseDir(fset, dir, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}

	checked := 0
	for _, pkg := range pkgs {
		for fileName, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
					continue
				}
				checked++
				if n := len(fn.Name.Name); n > MaxIntegTestNameLen {
					t.Errorf("%s: %s is %d chars — over the %d-char sun_path budget for make integration "+
						"(the test's TempDir itself would exceed sun_path; see TODO #804). "+
						"Shorten the name; sockets belong in short /tmp dirs, never under TempDir.",
						filepath.Base(fileName), fn.Name.Name, n, MaxIntegTestNameLen)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no test functions found in test/integration — gate is miswired")
	}
}
