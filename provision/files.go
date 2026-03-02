package provision

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// copyDir copies all files from src to dst (non-recursive, files only).
func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := copyFile(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies a single file.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	_, err = io.Copy(out, in)
	return err
}

// templateSoulFile replaces placeholder comments in a SOUL.md file with actual values.
func templateSoulFile(path, displayName string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no SOUL.md to template
		}
		return err
	}
	if displayName == "" {
		return nil
	}
	content := string(data)
	content = strings.Replace(content, "<!-- your name -->", displayName, 1)
	return os.WriteFile(path, []byte(content), 0644)
}
