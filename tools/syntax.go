package tools

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// SyntaxChecker validates file content and returns nil if valid.
type SyntaxChecker func(content []byte) error

// syntaxCheckers maps file extensions to their validators.
var syntaxCheckers = map[string]SyntaxChecker{
	".json": checkJSON,
	".toml": checkTOML,
	".go":   checkGo,
}

// checkSyntax looks up a checker by file extension and validates content.
// Returns nil if no checker exists for the extension.
func checkSyntax(path string, content []byte) error {
	ext := filepath.Ext(path)
	checker, ok := syntaxCheckers[ext]
	if !ok {
		return nil
	}
	return checker(content)
}

func checkJSON(content []byte) error {
	if !json.Valid(content) {
		return fmt.Errorf("invalid JSON syntax")
	}
	return nil
}

func checkTOML(content []byte) error {
	var v interface{}
	if _, err := toml.Decode(string(content), &v); err != nil {
		return fmt.Errorf("TOML: %w", err)
	}
	return nil
}

func checkGo(content []byte) error {
	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, "", content, parser.AllErrors)
	if err != nil {
		return fmt.Errorf("Go: %w", err)
	}
	return nil
}
