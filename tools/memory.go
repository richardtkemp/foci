package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func NewMemorySearchTool(memoryDir string) *Tool {
	return &Tool{
		Name:        "memory_search",
		Description: "Search across all markdown files in the memory directory. Returns matching lines with filenames.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "Search query (case-insensitive substring match)"
				}
			},
			"required": ["query"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return memorySearch(ctx, params, memoryDir)
		},
	}
}

func memorySearch(ctx context.Context, params json.RawMessage, memoryDir string) (string, error) {
	var p struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("parse params: %w", err)
	}

	query := strings.ToLower(p.Query)

	var results strings.Builder
	matches := 0
	const maxMatches = 100

	err := filepath.Walk(memoryDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		if matches >= maxMatches {
			return filepath.SkipAll
		}

		f, err := os.Open(path)
		if err != nil {
			return nil // skip files we can't read
		}
		defer f.Close()

		relPath, _ := filepath.Rel(memoryDir, path)
		scanner := bufio.NewScanner(f)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Text()
			if strings.Contains(strings.ToLower(line), query) {
				fmt.Fprintf(&results, "%s:%d: %s\n", relPath, lineNum, line)
				matches++
				if matches >= maxMatches {
					break
				}
			}
		}
		return nil
	})

	if err != nil {
		return "", fmt.Errorf("search memory: %w", err)
	}

	if results.Len() == 0 {
		return "No matches found.", nil
	}
	return results.String(), nil
}
