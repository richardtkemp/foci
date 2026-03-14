package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"foci/internal/anthropic"
	"foci/internal/provision"
	"foci/internal/secrets"
)

// consoleUI wraps a bufio.Reader for interactive console prompts.
type consoleUI struct {
	reader *bufio.Reader
}

func (c *consoleUI) Prompt(prompt string, current string) (input string, back bool) {
	p := prompt
	if current != "" {
		p = fmt.Sprintf("%s [%s]", prompt, current)
	}
	if p != "" {
		fmt.Printf("%s: ", p)
	} else {
		// Empty prompt — just wait for Enter
		fmt.Print("  ")
	}
	line, _ := c.reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "back" {
		return "", true
	}
	if line == "" && current != "" {
		return current, false
	}
	return line, false
}

func (c *consoleUI) Menu(prompt string, options []string) (index int, back bool) {
	if prompt != "" {
		fmt.Printf("  %s\n", prompt)
	}
	for i, opt := range options {
		fmt.Printf("  [%d] %s\n", i+1, opt)
	}
	fmt.Println()

	for {
		fmt.Print("> ")
		line, _ := c.reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "back" {
			return 0, true
		}
		var idx int
		if _, err := fmt.Sscanf(line, "%d", &idx); err == nil && idx >= 1 && idx <= len(options) {
			return idx - 1, false
		}
		fmt.Printf("  Enter a number between 1 and %d.\n", len(options))
	}
}

func (c *consoleUI) Print(text string) {
	fmt.Println(text)
}

// discoverModelFamily queries the Anthropic API to find the latest model in a family.
// Falls back to provision.ResolveModelAlias on failure.
func discoverModelFamily(store *secrets.Store, alias string) string {
	fallback := provision.ResolveModelAlias(alias)

	// Determine which family to search for
	family := strings.ToLower(strings.TrimSpace(alias))
	switch family {
	case "opus", "sonnet", "haiku":
		// proceed with API query
	default:
		// Full model ID or unknown alias — just use static resolution
		return fallback
	}

	// Get a token for the API call
	token := ""
	if v, ok := store.Get("anthropic.setup_token"); ok {
		token = v
	} else if v, ok := store.Get("anthropic.api_key"); ok {
		token = v
	}
	if token == "" {
		return fallback
	}

	fmt.Printf("  Querying Anthropic API for latest %s model... ", family)

	client := anthropic.NewClient(anthropic.StaticToken(token), 5*time.Second)
	models, err := client.ListModels()
	if err != nil {
		fmt.Printf("(using default: %s)\n", fallback)
		return fallback
	}

	// Find the latest model in the requested family
	var bestID string
	var bestTime time.Time
	for _, m := range models {
		if !strings.Contains(strings.ToLower(m.ID), family) {
			continue
		}
		if m.CreatedAt.After(bestTime) {
			bestTime = m.CreatedAt
			bestID = m.ID
		}
	}

	if bestID == "" {
		fmt.Printf("(not found, using default: %s)\n", fallback)
		return fallback
	}

	fmt.Printf("  %s\n", bestID)
	return bestID
}

// mdImportOptions configures the importMDFiles file picker.
type mdImportOptions struct {
	label     string                 // e.g. "character" or "memory"
	preSelect func(name string) bool // which files to pre-select
	emptySkip bool                   // true = skip gracefully on empty, false = error
}

// importMDFiles lists .md files from srcDir and lets the user select which to import into destDir.
func importMDFiles(reader *bufio.Reader, srcDir, destDir string, opts mdImportOptions) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("read directory %s: %w", srcDir, err)
	}

	type fileEntry struct {
		name     string
		size     int64
		selected bool
	}

	var files []fileEntry
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, fileEntry{
			name:     entry.Name(),
			size:     info.Size(),
			selected: opts.preSelect(entry.Name()),
		})
	}

	if len(files) == 0 {
		if opts.emptySkip {
			fmt.Printf("  No .md files found in %s, skipping %s import.\n", srcDir, opts.label)
			return nil
		}
		return fmt.Errorf("no .md files found in %s", srcDir)
	}

	// Sort: pre-selected files first, then alphabetical
	sort.Slice(files, func(i, j int) bool {
		si := files[i].selected
		sj := files[j].selected
		if si != sj {
			return si
		}
		return files[i].name < files[j].name
	})

	printFileList := func() {
		fmt.Printf("\n  Found %d .md files (top-level only):\n", len(files))
		for i, f := range files {
			check := "[ ]"
			if f.selected {
				check = "[x]"
			}
			fmt.Printf("    %2d. %s %s (%.1f KB)\n", i+1, check, f.name, float64(f.size)/1024)
		}
		fmt.Println()
		fmt.Printf("  Toggle with number, 'a' for all, Enter to confirm.\n")
	}

	printFileList()

	for {
		fmt.Print("> ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		if input == "back" {
			return fmt.Errorf("cancelled")
		}
		if input == "" {
			// Confirm selection
			break
		}
		if input == "a" {
			allSelected := true
			for _, f := range files {
				if !f.selected {
					allSelected = false
					break
				}
			}
			for i := range files {
				files[i].selected = !allSelected
			}
			printFileList()
			continue
		}

		var idx int
		if _, err := fmt.Sscanf(input, "%d", &idx); err == nil && idx >= 1 && idx <= len(files) {
			files[idx-1].selected = !files[idx-1].selected
			check := "[ ]"
			if files[idx-1].selected {
				check = "[x]"
			}
			fmt.Printf("    %2d. %s %s\n", idx, check, files[idx-1].name)
			continue
		}
		fmt.Printf("  Enter a number (1-%d), 'a' for all, or Enter to confirm.\n", len(files))
	}

	// Copy selected files
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	count := 0
	for _, f := range files {
		if !f.selected {
			continue
		}
		src := filepath.Join(srcDir, f.name)
		dst := filepath.Join(destDir, f.name)
		data, err := os.ReadFile(src)
		if err != nil {
			fmt.Printf("  Warning: could not read %s: %v\n", f.name, err)
			continue
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			fmt.Printf("  Warning: could not write %s: %v\n", f.name, err)
			continue
		}
		count++
	}
	fmt.Printf("  Imported %d %s files to %s/\n", count, opts.label, destDir)
	return nil
}

// importCharacterFiles lists .md files from srcDir and lets the user select which to import.
func importCharacterFiles(reader *bufio.Reader, srcDir, destDir string) error {
	knownCharacterFiles := map[string]bool{
		"SOUL.md":      true,
		"CRAFT.md":     true,
		"COHERENCE.md": true,
		"USER.md":      true,
		"MEMORY.md":    true,
	}
	return importMDFiles(reader, srcDir, destDir, mdImportOptions{
		label:     "character",
		preSelect: func(name string) bool { return knownCharacterFiles[name] },
		emptySkip: false,
	})
}

// importMemoryFiles lists .md files from srcDir and lets the user select which to import.
func importMemoryFiles(reader *bufio.Reader, srcDir, destDir string) error {
	return importMDFiles(reader, srcDir, destDir, mdImportOptions{
		label:     "memory",
		preSelect: func(_ string) bool { return true },
		emptySkip: true,
	})
}

// copyMap returns a shallow copy of a string map.
func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
