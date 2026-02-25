package tools

import "testing"

func TestCheckJSON(t *testing.T) {
	if err := checkJSON([]byte(`{"key": "value"}`)); err != nil {
		t.Errorf("valid JSON rejected: %v", err)
	}
	if err := checkJSON([]byte(`{"key": }`)); err == nil {
		t.Error("invalid JSON accepted")
	}
}

func TestCheckTOML(t *testing.T) {
	if err := checkTOML([]byte("[section]\nkey = \"val\"\n")); err != nil {
		t.Errorf("valid TOML rejected: %v", err)
	}
	if err := checkTOML([]byte("[section\nkey = val")); err == nil {
		t.Error("invalid TOML accepted")
	}
}

func TestCheckGo(t *testing.T) {
	if err := checkGo([]byte("package main\n\nfunc main() {}\n")); err != nil {
		t.Errorf("valid Go rejected: %v", err)
	}
	if err := checkGo([]byte("package main\n\nfunc main() {\n")); err == nil {
		t.Error("invalid Go accepted")
	}
}

func TestCheckSyntaxUnknownExtension(t *testing.T) {
	// Unknown extensions should pass (no checker)
	if err := checkSyntax("file.txt", []byte("anything")); err != nil {
		t.Errorf("unknown extension should pass: %v", err)
	}
}
