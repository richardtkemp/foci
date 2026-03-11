package tools

import (
	"os/exec"
	"testing"
)

func TestCheckJSON(t *testing.T) {
	t.Parallel()
	if err := checkJSON([]byte(`{"key": "value"}`)); err != nil {
		t.Errorf("valid JSON rejected: %v", err)
	}
	if err := checkJSON([]byte(`{"key": }`)); err == nil {
		t.Error("invalid JSON accepted")
	}
}

func TestCheckTOML(t *testing.T) {
	t.Parallel()
	if err := checkTOML([]byte("[section]\nkey = \"val\"\n")); err != nil {
		t.Errorf("valid TOML rejected: %v", err)
	}
	if err := checkTOML([]byte("[section\nkey = val")); err == nil {
		t.Error("invalid TOML accepted")
	}
}

func TestCheckGo(t *testing.T) {
	t.Parallel()
	if err := checkGo([]byte("package main\n\nfunc main() {}\n")); err != nil {
		t.Errorf("valid Go rejected: %v", err)
	}
	if err := checkGo([]byte("package main\n\nfunc main() {\n")); err == nil {
		t.Error("invalid Go accepted")
	}
}

func TestCheckSyntaxUnknownExtension(t *testing.T) {
	// Unknown extensions should pass (no checker)
	t.Parallel()
	if err := checkSyntax("file.txt", []byte("anything")); err != nil {
		t.Errorf("unknown extension should pass: %v", err)
	}
}

func TestCheckYAML(t *testing.T) {
	t.Parallel()
	valid := []byte("name: test\nitems:\n  - one\n  - two\n")
	if err := checkYAML(valid); err != nil {
		t.Errorf("valid YAML rejected: %v", err)
	}

	invalid := []byte("name: test\n  bad indent:\n- broken\n")
	if err := checkYAML(invalid); err == nil {
		t.Error("invalid YAML accepted")
	}
}

func TestCheckYAMLExtensions(t *testing.T) {
	t.Parallel()
	content := []byte("key: value\n")
	if err := checkSyntax("config.yaml", content); err != nil {
		t.Errorf(".yaml extension rejected: %v", err)
	}
	if err := checkSyntax("config.yml", content); err != nil {
		t.Errorf(".yml extension rejected: %v", err)
	}
}

func TestCheckXML(t *testing.T) {
	t.Parallel()
	valid := []byte(`<?xml version="1.0"?><root><item>hello</item></root>`)
	if err := checkXML(valid); err != nil {
		t.Errorf("valid XML rejected: %v", err)
	}

	invalid := []byte(`<root><item>unclosed</root>`)
	if err := checkXML(invalid); err == nil {
		t.Error("invalid XML accepted")
	}
}

func TestCheckPython(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	valid := []byte("def hello():\n    print('hello')\n")
	if err := checkPython(valid); err != nil {
		t.Errorf("valid Python rejected: %v", err)
	}

	invalid := []byte("def hello(\n    print('hello')\n")
	if err := checkPython(invalid); err == nil {
		t.Error("invalid Python accepted")
	}
}

func TestCheckPythonSkipsWhenUnavailable(t *testing.T) {
	// This tests the graceful skip logic — if python3 is missing, checkPython returns nil
	t.Parallel()
	// We can't easily simulate a missing python3, but we verify the function signature works
	content := []byte("x = 1\n")
	err := checkPython(content)
	// Either nil (python3 found and valid) or nil (python3 not found, skipped)
	if err != nil {
		t.Errorf("valid Python or skip should return nil: %v", err)
	}
}

func TestCheckShell(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	valid := []byte("#!/bin/bash\necho hello\nif true; then\n  echo yes\nfi\n")
	if err := checkShell(valid); err != nil {
		t.Errorf("valid shell rejected: %v", err)
	}

	invalid := []byte("#!/bin/bash\nif true; then\n  echo yes\n")
	if err := checkShell(invalid); err == nil {
		t.Error("invalid shell accepted")
	}
}

func TestCheckShellExtensions(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	content := []byte("echo hello\n")
	if err := checkSyntax("script.sh", content); err != nil {
		t.Errorf(".sh extension rejected: %v", err)
	}
	if err := checkSyntax("script.bash", content); err != nil {
		t.Errorf(".bash extension rejected: %v", err)
	}
}
