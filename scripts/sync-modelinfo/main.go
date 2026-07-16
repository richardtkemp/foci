// Package main implements sync-modelinfo: a tool that synchronises the model
// pricing data in internal/modelinfo/models.jsonl against the OpenRouter API.
//
// Usage:
//
//	go run scripts/sync-modelinfo/main.go [flags]
//
// Flags:
//
//	--add-popular N   Add the N newest models not already in the registry (default: 20).
//	--repo PATH       Path to the foci repo root (default: auto-detect via git).
//	--dry-run         Report discrepancies without creating a worktree.
//	--verbose         Print per-model details during the sync.
//
// What it does:
//
//  1. Reads internal/modelinfo/models.jsonl from the repo.
//  2. Fetches https://openrouter.ai/api/v1/models.
//  3. For each existing entry: checks availability, compares prices, updates if changed.
//  4. Adds the N newest API models not already present (newest by 'created' timestamp,
//     since the API does not expose usage/popularity metrics).
//  5. Writes the updated JSONL to a git worktree and commits.
//  6. Prints a summary: "X new models, Y price changes, see <worktree-path>"
//
// :nitro variants in the JSONL are verified against their base model (the API
// does not list :nitro as separate entries).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const openrouterAPI = "https://openrouter.ai/api/v1/models"

// jsonlEntry matches the format in internal/modelinfo/models.jsonl.
type jsonlEntry struct {
	ID              string  `json:"id"`
	Provider        string  `json:"provider"`
	ContextWindow   int     `json:"context_window,omitempty"`
	Effort          bool    `json:"effort,omitempty"`
	Thinking        bool    `json:"thinking,omitempty"`
	Speed           bool    `json:"speed,omitempty"`
	Caching         bool    `json:"caching,omitempty"`
	InputPer1M      float64 `json:"input_per_1m,omitempty"`
	OutputPer1M     float64 `json:"output_per_1m,omitempty"`
	CacheReadPer1M  float64 `json:"cache_read_per_1m,omitempty"`
	CacheWritePer1M float64 `json:"cache_write_per_1m,omitempty"`
	Comment         string  `json:"comment,omitempty"`
}

// orModel is the subset of the OpenRouter API model we care about.
type orModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
	Created       int64  `json:"created"`
	Pricing       struct {
		Prompt         string `json:"prompt"`
		Completion     string `json:"completion"`
		InputCacheRead string `json:"input_cache_read"`
	} `json:"pricing"`
}

// orResponse is the top-level API response envelope.
type orResponse struct {
	Data []orModel `json:"data"`
}

func main() {
	addPopular := flag.Int("add-popular", 20, "number of newest models to add if missing")
	repoFlag := flag.String("repo", "", "path to the foci repo root (default: auto-detect)")
	dryRun := flag.Bool("dry-run", false, "report without creating a worktree")
	verbose := flag.Bool("verbose", false, "print per-model details")
	flag.Parse()

	repo := *repoFlag
	if repo == "" {
		out, err := gitOutput("", "rev-parse", "--show-toplevel")
		if err != nil {
			fail("could not determine repo root: %v", err)
		}
		repo = strings.TrimSpace(out)
	}

	jsonlPath := filepath.Join(repo, "internal", "modelinfo", "models.jsonl")

	// --- Read existing entries ---

	entries, err := readJSONL(jsonlPath)
	if err != nil {
		fail("reading %s: %v", jsonlPath, err)
	}
	if *verbose {
		fmt.Fprintf(os.Stderr, "loaded %d entries from %s\n", len(entries), jsonlPath)
	}

	// --- Fetch OpenRouter API ---

	if *verbose {
		fmt.Fprintln(os.Stderr, "fetching OpenRouter API...")
	}
	apiModels, err := fetchAPI()
	if err != nil {
		fail("fetching OpenRouter API: %v", err)
	}
	if *verbose {
		fmt.Fprintf(os.Stderr, "fetched %d models from API\n", len(apiModels))
	}

	// Build lookup: bareID → orModel (bareID = part after first '/').
	apiByBare := make(map[string]orModel, len(apiModels))
	for _, m := range apiModels {
		bare := stripProvider(m.ID)
		if bare != "" {
			apiByBare[bare] = m
		}
	}

	// --- Verify existing entries ---

	var priceChanges []priceChange
	var unavailable []string

	for i := range entries {
		e := &entries[i]
		bare := e.ID

		// :nitro variants — verify against the base model.
		lookupID := strings.TrimSuffix(bare, ":nitro")

		api, ok := apiByBare[lookupID]
		if !ok {
			unavailable = append(unavailable, bare)
			if *verbose {
				fmt.Fprintf(os.Stderr, "  ⚠ %s: not found in API\n", bare)
			}
			continue
		}

		apiIn, _ := strconv.ParseFloat(api.Pricing.Prompt, 64)
		apiOut, _ := strconv.ParseFloat(api.Pricing.Completion, 64)
		apiIn *= 1e6
		apiOut *= 1e6

		changed := false
		if apiIn > 0 && abs(apiIn-e.InputPer1M) > 0.005 {
			priceChanges = append(priceChanges, priceChange{
				id: bare, field: "input_per_1m",
				old: e.InputPer1M, new: apiIn,
			})
			e.InputPer1M = apiIn
			changed = true
		}
		if apiOut > 0 && abs(apiOut-e.OutputPer1M) > 0.005 {
			priceChanges = append(priceChanges, priceChange{
				id: bare, field: "output_per_1m",
				old: e.OutputPer1M, new: apiOut,
			})
			e.OutputPer1M = apiOut
			changed = true
		}

		// Update context window if we have it and it differs.
		if api.ContextLength > 0 && api.ContextLength != e.ContextWindow {
			if e.ContextWindow > 0 {
				if *verbose {
					fmt.Fprintf(os.Stderr, "  ℹ %s: context %d → %d\n", bare, e.ContextWindow, api.ContextLength)
				}
			}
			e.ContextWindow = api.ContextLength
			changed = true
		}

		if changed && *verbose {
			fmt.Fprintf(os.Stderr, "  ✏ %s: updated\n", bare)
		}
	}

	// --- Add popular (newest) models ---

	// Sort API models by created timestamp descending (newest first).
	sort.Slice(apiModels, func(i, j int) bool {
		return apiModels[i].Created > apiModels[j].Created
	})

	// Track which bare IDs we already have (including :nitro base IDs).
	existing := make(map[string]bool, len(entries))
	for _, e := range entries {
		existing[e.ID] = true
		existing[strings.TrimSuffix(e.ID, ":nitro")] = true
	}

	var newEntries []jsonlEntry
	for _, m := range apiModels {
		if len(newEntries) >= *addPopular {
			break
		}
		bare := stripProvider(m.ID)
		if bare == "" || existing[bare] {
			continue
		}
		// Skip free models (price = "0") to avoid cluttering the registry.
		in, _ := strconv.ParseFloat(m.Pricing.Prompt, 64)
		out, _ := strconv.ParseFloat(m.Pricing.Completion, 64)
		if in == 0 && out == 0 {
			continue
		}
		newEntries = append(newEntries, jsonlEntry{
			ID:            bare,
			Provider:      "openrouter",
			ContextWindow: m.ContextLength,
			InputPer1M:    in * 1e6,
			OutputPer1M:   out * 1e6,
		})
		existing[bare] = true
		if *verbose {
			fmt.Fprintf(os.Stderr, "  + %s: added ($%.4f/$%.4f per 1M)\n", bare, in*1e6, out*1e6)
		}
	}

	entries = append(entries, newEntries...)

	// --- Summary ---

	summary := fmt.Sprintf("%d new models, %d price changes, %d unavailable",
		len(newEntries), len(priceChanges), len(unavailable))

	if len(unavailable) > 0 {
		summary += "\n  unavailable: " + strings.Join(unavailable, ", ")
	}
	for _, pc := range priceChanges {
		summary += fmt.Sprintf("\n  %s %s: $%.4f → $%.4f", pc.id, pc.field, pc.old, pc.new)
	}

	if *dryRun {
		fmt.Println(summary)
		return
	}

	// --- Write to worktree ---

	wtPath, err := createWorktree(repo)
	if err != nil {
		// If worktree creation fails, we still report what would change.
		fmt.Fprintln(os.Stderr, "error creating worktree:", err)
		fmt.Println(summary)
		fmt.Println("NOTE: no worktree created — run without --dry-run after fixing the error")
		return
	}

	wtJSONL := filepath.Join(wtPath, "internal", "modelinfo", "models.jsonl")
	if err := writeJSONL(wtJSONL, entries); err != nil {
		fmt.Fprintln(os.Stderr, "error writing JSONL:", err)
		fmt.Println(summary)
		return
	}

	if err := gitCommit(wtPath, fmt.Sprintf("modelinfo: sync with OpenRouter API (%s)", timestamp())); err != nil {
		fmt.Fprintln(os.Stderr, "error committing:", err)
		fmt.Println(summary)
		return
	}

	fmt.Printf("%s\nchanges at %s\n", summary, wtPath)
}

// --- Types ---

type priceChange struct {
	id, field string
	old, new  float64
}

// --- Helpers ---

func readJSONL(path string) ([]jsonlEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []jsonlEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e jsonlEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("line %q: %w", line, err)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func writeJSONL(path string, entries []jsonlEntry) error {
	// Sort entries alphabetically by ID for stable diffs.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ID < entries[j].ID
	})

	var buf strings.Builder
	for _, e := range entries {
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(buf.String()), 0644)
}

func fetchAPI() ([]orModel, error) {
	resp, err := http.Get(openrouterAPI)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var r orResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	return r.Data, nil
}

// stripProvider removes the "provider/" prefix from an OpenRouter model ID.
func stripProvider(id string) string {
	if i := strings.IndexByte(id, '/'); i > 0 {
		return id[i+1:]
	}
	return id
}

func createWorktree(repo string) (string, error) {
	branch := "sync-modelinfo-" + timestamp()
	// Worktree as a sibling of the repo, following foci convention.
	wtName := filepath.Base(repo) + "-wt-sync-modelinfo"
	wtPath := filepath.Join(filepath.Dir(repo), wtName)

	// Remove if stale from a previous run (best-effort).
	_ = os.RemoveAll(wtPath)

	cmd := exec.Command("git", "-c", "core.sharedRepository=false",
		"-C", repo, "worktree", "add", "-b", branch, wtPath, "main")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return wtPath, nil
}

func gitCommit(wt, msg string) error {
	cmd := exec.Command("git", "-C", wt, "add", "-A")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	cmd = exec.Command("git", "-C", wt, "commit", "-m", msg)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	return string(out), err
}

func timestamp() string {
	return time.Now().UTC().Format("20060102-150405")
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sync-modelinfo: "+format+"\n", args...)
	os.Exit(1)
}
