package tools

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

// SyntaxChecker validates file content and returns nil if valid.
type SyntaxChecker func(content []byte) error

// syntaxCheckers maps file extensions to their validators.
var syntaxCheckers = map[string]SyntaxChecker{
	".json":  checkJSON,
	".toml":  checkTOML,
	".go":    checkGo,
	".yaml":  checkYAML,
	".yml":   checkYAML,
	".xml":   checkXML,
	".py":    checkPython,
	".sh":    checkShell,
	".bash":  checkShell,
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
		return fmt.Errorf("toml: %w", err)
	}
	return nil
}

func checkGo(content []byte) error {
	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, "", content, parser.AllErrors)
	if err != nil {
		return fmt.Errorf("go: %w", err)
	}
	return nil
}

func checkYAML(content []byte) error {
	var v interface{}
	if err := yaml.Unmarshal(content, &v); err != nil {
		return fmt.Errorf("yaml: %w", err)
	}
	return nil
}

func checkXML(content []byte) error {
	d := xml.NewDecoder(bytes.NewReader(content))
	for {
		_, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("xml: %w", err)
		}
	}
	return nil
}

func checkPython(content []byte) error {
	path, err := exec.LookPath("python3")
	if err != nil {
		return nil // python3 not available, skip
	}
	cmd := exec.Command(path, "-c", "import ast,sys; ast.parse(sys.stdin.read())")
	cmd.Stdin = bytes.NewReader(content)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("python: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func checkShell(content []byte) error {
	path, err := exec.LookPath("bash")
	if err != nil {
		return nil // bash not available, skip
	}
	cmd := exec.Command(path, "-n")
	cmd.Stdin = bytes.NewReader(content)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("shell: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
