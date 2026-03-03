package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLsBasic(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi"), 0644)
	os.WriteFile(filepath.Join(dir, "world.txt"), []byte("there"), 0644)

	tool := NewLsTool()
	params, _ := json.Marshal(map[string]string{"path": dir})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "hello.txt") {
		t.Errorf("expected hello.txt in output, got %q", result)
	}
	if !strings.Contains(result, "world.txt") {
		t.Errorf("expected world.txt in output, got %q", result)
	}
}

func TestLsFlags(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("data"), 0644)

	tool := NewLsTool()
	params, _ := json.Marshal(map[string]string{
		"path":   dir,
		"params": "-la",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// -l includes permissions, -a includes . and ..
	if !strings.Contains(result, "test.txt") {
		t.Errorf("expected test.txt in output, got %q", result)
	}
}

func TestLsNonexistentPath(t *testing.T) {
	tool := NewLsTool()
	params, _ := json.Marshal(map[string]string{"path": "/nonexistent/path/xyz"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v (should return error in output)", err)
	}
	if !strings.Contains(result, "Error:") {
		t.Errorf("expected error in result for nonexistent path, got %q", result)
	}
}

func TestFindByName(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "foo.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(dir, "bar.txt"), []byte("text"), 0644)

	tool := NewFindTool()
	params, _ := json.Marshal(map[string]string{
		"path":   dir,
		"params": `-name "*.go"`,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "foo.go") {
		t.Errorf("expected foo.go in output, got %q", result)
	}
	if strings.Contains(result, "bar.txt") {
		t.Errorf("should not contain bar.txt, got %q", result)
	}
}

func TestFindByType(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "subdir"), 0755)
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("data"), 0644)

	tool := NewFindTool()
	params, _ := json.Marshal(map[string]string{
		"path":   dir,
		"params": "-type d",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "subdir") {
		t.Errorf("expected subdir in output, got %q", result)
	}
}

func TestFindMaxdepth(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0755)
	os.WriteFile(filepath.Join(dir, "a", "b", "deep.txt"), []byte("deep"), 0644)
	os.WriteFile(filepath.Join(dir, "shallow.txt"), []byte("shallow"), 0644)

	tool := NewFindTool()
	params, _ := json.Marshal(map[string]string{
		"path":   dir,
		"params": "-maxdepth 1 -type f",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "shallow.txt") {
		t.Errorf("expected shallow.txt in output, got %q", result)
	}
	if strings.Contains(result, "deep.txt") {
		t.Errorf("should not contain deep.txt, got %q", result)
	}
}

func TestFindBlockedExec(t *testing.T) {
	tool := NewFindTool()
	for _, blocked := range []string{"-exec", "-execdir", "-delete", "-fls"} {
		params, _ := json.Marshal(map[string]string{
			"path":   "/tmp",
			"params": blocked + " rm {} \\;",
		})
		_, err := tool.Execute(context.Background(), params)
		if err == nil {
			t.Errorf("%s should have been blocked", blocked)
		}
		if !strings.Contains(err.Error(), "blocked predicate") {
			t.Errorf("%s error = %q, want 'blocked predicate'", blocked, err.Error())
		}
	}
}

func TestGrepBasicMatch(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hello world\nfoo bar\nhello again"), 0644)

	binary, name := resolveGrepBinary()
	tool := NewGrepTool(binary, name)
	params, _ := json.Marshal(map[string]string{
		"pattern": "hello",
		"path":    filepath.Join(dir, "test.txt"),
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "hello") {
		t.Errorf("expected 'hello' in output, got %q", result)
	}
}

func TestGrepParams(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("Hello World\nhello lower\nHELLO UPPER"), 0644)

	binary, name := resolveGrepBinary()
	tool := NewGrepTool(binary, name)
	params, _ := json.Marshal(map[string]string{
		"pattern": "hello",
		"path":    filepath.Join(dir, "test.txt"),
		"params":  "-i -c",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// -i -c should count all 3 matches
	if !strings.Contains(result, "3") {
		t.Errorf("expected count of 3, got %q", result)
	}
}

func TestGrepContextLines(t *testing.T) {
	dir := t.TempDir()
	content := "line1\nline2\ntarget\nline4\nline5\n"
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte(content), 0644)

	binary, name := resolveGrepBinary()
	tool := NewGrepTool(binary, name)
	params, _ := json.Marshal(map[string]string{
		"pattern": "target",
		"path":    filepath.Join(dir, "test.txt"),
		"params":  "-C 1",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "line2") {
		t.Errorf("expected context line 'line2', got %q", result)
	}
	if !strings.Contains(result, "line4") {
		t.Errorf("expected context line 'line4', got %q", result)
	}
}

func TestGrepNoMatch(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hello world"), 0644)

	binary, name := resolveGrepBinary()
	tool := NewGrepTool(binary, name)
	params, _ := json.Marshal(map[string]string{
		"pattern": "nonexistent_pattern_xyz",
		"path":    filepath.Join(dir, "test.txt"),
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "(no matches)") {
		t.Errorf("expected '(no matches)', got %q", result)
	}
}

func TestGrepRejectedParams(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hello world"), 0644)

	binary, name := resolveGrepBinary()
	tool := NewGrepTool(binary, name)
	params, _ := json.Marshal(map[string]string{
		"pattern": "hello",
		"path":    filepath.Join(dir, "test.txt"),
		"params":  "-i --badopt",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "--badopt was ignored") {
		t.Errorf("expected notice about --badopt, got %q", result)
	}
	if !strings.Contains(result, "hello") {
		t.Errorf("expected match output, got %q", result)
	}
}

func TestGrepGlobFlag(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.go"), []byte("package main\nfunc hello()"), 0644)
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hello text"), 0644)

	binary, name := resolveGrepBinary()
	if name != "rg" && name != "grep" {
		t.Skipf("--glob test only meaningful for rg/grep, have %s", name)
	}

	tool := NewGrepTool(binary, name)
	params, _ := json.Marshal(map[string]string{
		"pattern": "hello",
		"path":    dir,
		"params":  "--glob=*.go",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "hello") {
		t.Errorf("expected match, got %q", result)
	}
}

func TestResolveGrepBinary(t *testing.T) {
	path, name := resolveGrepBinary()
	if path == "" {
		t.Error("resolveGrepBinary returned empty path")
	}
	if name == "" {
		t.Error("resolveGrepBinary returned empty name")
	}
	// Must be one of the expected binaries
	valid := map[string]bool{"rg": true, "ack": true, "ag": true, "grep": true}
	if !valid[name] {
		t.Errorf("unexpected binary name: %q", name)
	}
}

func TestTranslateGrepFlags(t *testing.T) {
	tests := []struct {
		name     string
		params   string
		binary   string
		wantArgs []string
		wantNote bool // expect at least one notice
	}{
		{"simple -i", "-i", "rg", []string{"-i"}, false},
		{"combined -inl", "-inl", "rg", []string{"-i", "-n", "-l"}, false},
		{"context -C 3", "-C 3", "rg", []string{"-C", "3"}, false},
		{"attached context -C3", "-C3", "rg", []string{"-C", "3"}, false},
		{"-F for ack", "-F", "ack", []string{"-Q"}, false},
		{"-F for rg", "-F", "rg", []string{"-F"}, false},
		{"--hidden for rg", "--hidden", "rg", []string{"--hidden"}, false},
		{"--hidden for grep", "--hidden", "grep", nil, false},
		{"--glob for rg", "--glob=*.go", "rg", []string{"--glob=*.go"}, false},
		{"--glob for grep", "--glob=*.go", "grep", []string{"--include=*.go"}, false},
		{"--glob for ack", "--glob=*.go", "ack", nil, true},
		{"unknown flag", "--foobar", "rg", nil, true},
		{"unknown short", "-Z", "rg", nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, notices := translateGrepFlags(tt.params, tt.binary)
			if len(tt.wantArgs) == 0 && len(args) != 0 {
				t.Errorf("expected no args, got %v", args)
			}
			for i, want := range tt.wantArgs {
				if i >= len(args) {
					t.Errorf("missing arg at index %d: want %q", i, want)
					continue
				}
				if args[i] != want {
					t.Errorf("arg[%d] = %q, want %q", i, args[i], want)
				}
			}
			if tt.wantNote && len(notices) == 0 {
				t.Error("expected at least one notice, got none")
			}
			if !tt.wantNote && len(notices) > 0 {
				t.Errorf("unexpected notices: %v", notices)
			}
		})
	}
}

func TestSplitShellArgs(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{`-name "*.go"`, []string{"-name", "*.go"}},
		{`-type f`, []string{"-type", "f"}},
		{`-name '*.txt' -type f`, []string{"-name", "*.txt", "-type", "f"}},
		{`-maxdepth 1`, []string{"-maxdepth", "1"}},
		{``, nil},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := splitShellArgs(tt.input)
			if err != nil {
				t.Fatalf("splitShellArgs(%q): %v", tt.input, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i, w := range tt.want {
				if got[i] != w {
					t.Errorf("arg[%d] = %q, want %q", i, got[i], w)
				}
			}
		})
	}
}
