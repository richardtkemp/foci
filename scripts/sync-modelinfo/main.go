// Package main implements sync-modelinfo: a tool that synchronises the model
// pricing data in internal/modelinfo/models.jsonl against the OpenRouter API.
//
// Usage:
//
//	go run scripts/sync-modelinfo/main.go [flags]
//
// Flags:
//
//	--add-popular N      Add the N newest models not already in the registry (default: 20).
//	--add-anthropic N    Also add the N newest Anthropic models not already present (default: 10).
//	--add-openai N       Also add the N newest OpenAI models not already present (default: 10).
//	--repo PATH          Path to the foci repo root (default: auto-detect via git).
//	--dry-run            Report discrepancies without creating a worktree.
//	--verbose            Print per-model details during the sync.
//
// What it does:
//
//  1. Reads internal/modelinfo/models.jsonl from the repo.
//  2. Fetches https://openrouter.ai/api/v1/models.
//  3. For each existing entry: checks availability, compares prices, updates if changed.
//  4. Adds the N newest API models not already present (newest by 'created' timestamp,
//     since the API does not expose usage/popularity metrics).
//  5. Also ensures the latest N Anthropic and N OpenAI releases are present.
//  6. Writes the updated JSONL to a git worktree and commits.
//  7. Prints a summary: "X new models, Y price changes, see <worktree-path>"
//
// :nitro variants in the JSONL are verified against their base model (the API
// does not list :nitro as separate entries).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
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
	Dev             string  `json:"dev,omitempty"`
	ContextWindow   int     `json:"context_window,omitempty"`
	Effort          bool    `json:"effort,omitempty"`
	Thinking        bool    `json:"thinking,omitempty"`
	Speed           bool    `json:"speed,omitempty"`
	Caching         bool    `json:"caching,omitempty"`
	InputPer1M      float64 `json:"input_per_1m,omitempty"`
	OutputPer1M     float64 `json:"output_per_1m,omitempty"`
	CacheReadPer1M  float64 `json:"cache_read_per_1m,omitempty"`
	CacheWritePer1M float64 `json:"cache_write_per_1m,omitempty"`
	// Extended pricing + quality — STORED but not yet used in cost calc (TODO
	// #1407). Token-based prices are per-1M (like the base rates above); the
	// per-unit ones (web_search per call, image/audio) are kept raw as the API
	// gives them, since their unit varies by model.
	CacheWrite1hPer1M      float64     `json:"cache_write_1h_per_1m,omitempty"`
	InternalReasoningPer1M float64     `json:"internal_reasoning_per_1m,omitempty"`
	WebSearchPerCall       float64     `json:"web_search_per_call,omitempty"`
	ImagePrice             float64     `json:"image_price,omitempty"`
	AudioPrice             float64     `json:"audio_price,omitempty"`
	PriceTiers             []PriceTier `json:"price_tiers,omitempty"`
	IntelligenceIndex      float64     `json:"intelligence_index,omitempty"`
	// Fetched is the UTC date (YYYY-MM-DD) on which this entry's pricing was
	// last confirmed valid against the OpenRouter API. Refreshed for every
	// entry the sync still finds in the API; entries that have gone
	// unavailable keep their older stamp, so a stale date flags stale data.
	Fetched string `json:"fetched,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// PriceTier is a usage-dependent price schedule (OpenRouter's pricing.overrides,
// e.g. Claude's ~2x rate above 200k prompt tokens). All prices per-1M tokens.
type PriceTier struct {
	MinPromptTokens   int     `json:"min_prompt_tokens"`
	InputPer1M        float64 `json:"input_per_1m,omitempty"`
	OutputPer1M       float64 `json:"output_per_1m,omitempty"`
	CacheReadPer1M    float64 `json:"cache_read_per_1m,omitempty"`
	CacheWritePer1M   float64 `json:"cache_write_per_1m,omitempty"`
	CacheWrite1hPer1M float64 `json:"cache_write_1h_per_1m,omitempty"`
}

// orModel is the subset of the OpenRouter API model we care about.
type orModel struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	ContextLength int       `json:"context_length"`
	Created       int64     `json:"created"`
	Pricing       orPricing `json:"pricing"`
	Benchmarks    struct {
		ArtificialAnalysis struct {
			IntelligenceIndex float64 `json:"intelligence_index"`
		} `json:"artificial_analysis"`
	} `json:"benchmarks"`
}

// orPricing is one price schedule from the API — reused for the base rates and
// for each tiered override (which additionally carries min_prompt_tokens). All
// values are strings in per-token / per-unit USD.
type orPricing struct {
	MinPromptTokens   int         `json:"min_prompt_tokens,omitempty"`
	Prompt            string      `json:"prompt"`
	Completion        string      `json:"completion"`
	InputCacheRead    string      `json:"input_cache_read"`
	InputCacheWrite   string      `json:"input_cache_write"`
	InputCacheWrite1h string      `json:"input_cache_write_1h"`
	WebSearch         string      `json:"web_search"`
	Image             string      `json:"image"`
	Audio             string      `json:"audio"`
	InternalReasoning string      `json:"internal_reasoning"`
	Overrides         []orPricing `json:"overrides,omitempty"`
}

// orResponse is the top-level API response envelope.
type orResponse struct {
	Data []orModel `json:"data"`
}

func main() {
	repoFlag := flag.String("repo", "", "path to the foci repo root (default: auto-detect)")
	baseFlag := flag.String("base", "main", "branch/commit to fork the review worktree from (use the feature branch while models.jsonl is unmerged)")
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
	// The moment this data is known-valid: stamped onto every entry the fetch
	// still confirms (below), so a stale `fetched` date flags stale pricing.
	fetchDate := time.Now().UTC().Format("2006-01-02")

	// Build lookup: bareID → orModel (bareID = part after first '/').
	apiByBare := make(map[string]orModel, len(apiModels))
	for _, m := range apiModels {
		bare := stripProvider(m.ID)
		if bare != "" {
			apiByBare[bare] = m
		}
	}

	// --- Backfill the `dev` (author/vendor) field on existing entries ---
	//
	// `dev` is a STATIC identity attribute (the OpenRouter author slug —
	// moonshotai, anthropic, …), not time-varying price/caps data, so unlike a
	// price change it is corrected IN PLACE on every historic row rather than by
	// appending a new row. Every row whose bare id matches a live API model and
	// has no `dev` yet gets it from that model's API id. Rows for models no
	// longer in the API (unavailable) can't be resolved here and are left for a
	// manual fix.
	backfilledDevs := 0
	for i := range entries {
		if entries[i].Dev != "" {
			continue
		}
		lookupID := strings.TrimSuffix(entries[i].ID, ":nitro")
		if api, ok := apiByBare[lookupID]; ok {
			if dev := devFromAPIID(api.ID); dev != "" {
				entries[i].Dev = dev
				backfilledDevs++
			}
		}
	}

	// --- Verify existing entries (append-only history) ---
	//
	// models.jsonl is an APPEND-ONLY ledger: a model may have several rows over
	// time, each stamped with the `fetched` date on which that snapshot was
	// observed. Existing rows are NEVER modified or deleted. When the API shows
	// a change, we append ONE new row; when nothing changed, we append nothing
	// (no timestamp-only churn). At runtime the modelinfo package keys the
	// registry to the LATEST row per (id, provider); older rows are historical
	// reference only. This keeps a full price/caps history in git while live
	// lookups always see the newest value.
	//
	// WHAT WE SYNC FROM THE API (authoritative, may change over time):
	//   input/output price, context window, and the CACHE caps —
	//   caching + cache_read/cache_write price (pricing.input_cache_read/write).
	//
	// WHAT WE DELIBERATELY DO NOT SYNC (curated; carried forward unchanged):
	//   effort, thinking, speed. OpenRouter's `supported_parameters` reports a
	//   generic "reasoning" for nearly every modern model — opus, sonnet,
	//   haiku, gemini all identical — so it cannot reproduce foci's per-model
	//   effort/thinking distinctions (those describe which control knobs foci
	//   exposes, not whether the model reasons). And "speed" (Claude fast mode)
	//   has no API signal at all. Auto-deriving these would wrongly flip
	//   haiku/gemini thinking→true and downgrade opus effort/speed. So a new
	//   appended row clones effort/thinking/speed (and comment) from the prior
	//   latest row and only overwrites the API-authoritative fields. Do not
	//   "helpfully" wire these to supported_parameters — it corrupts the caps.

	var priceChanges []priceChange
	var unavailable []string

	// Latest row per (id, provider): max `fetched`, file order breaks ties.
	latest := map[string]int{}
	for i := range entries {
		k := entries[i].ID + "\x00" + entries[i].Provider
		if j, ok := latest[k]; !ok || entries[i].Fetched >= entries[j].Fetched {
			latest[k] = i
		}
	}

	var appended []jsonlEntry
	// Iterate in file order, acting once per group at its latest row, so output
	// is deterministic.
	for i := range entries {
		cur := entries[i]
		if latest[cur.ID+"\x00"+cur.Provider] != i {
			continue
		}

		// :nitro variants — verify against the base model.
		lookupID := strings.TrimSuffix(cur.ID, ":nitro")
		api, ok := apiByBare[lookupID]
		if !ok {
			unavailable = append(unavailable, cur.ID)
			if *verbose {
				fmt.Fprintf(os.Stderr, "  ⚠ %s: not found in API\n", cur.ID)
			}
			continue
		}

		af, ok := apiFields(api)
		if !ok {
			continue // free/negative/garbage pricing — never append
		}
		if !fieldsDiffer(cur, af) {
			continue // no API-field change → no new row (no timestamp-only churn)
		}

		// Append a NEW row: clone the prior latest (preserving curated
		// effort/thinking/speed/comment) and overwrite only the API fields.
		nr := cur
		if abs(af.in-cur.InputPer1M) > 0.005 {
			priceChanges = append(priceChanges, priceChange{id: cur.ID, field: "input_per_1m", old: cur.InputPer1M, new: af.in})
		}
		if abs(af.out-cur.OutputPer1M) > 0.005 {
			priceChanges = append(priceChanges, priceChange{id: cur.ID, field: "output_per_1m", old: cur.OutputPer1M, new: af.out})
		}
		applyAPIFields(&nr, af)
		nr.Fetched = fetchDate
		appended = append(appended, nr)
		if *verbose {
			fmt.Fprintf(os.Stderr, "  ✏ %s: appended new row (fetched %s)\n", cur.ID, fetchDate)
		}
	}

	// --- Add every model missing from the registry ---
	//
	// No ranking or top-N window: add anything present in the API but not yet
	// tracked, skipping free (price 0/0) and sentinel/negative-priced entries.
	// This converges — the first run seeds the whole catalogue, and later runs
	// add only genuinely-new models (plus the update rows appended above).

	// Track which bare IDs we already have (including :nitro base IDs).
	existing := make(map[string]bool, len(entries))
	for _, e := range entries {
		existing[e.ID] = true
		existing[strings.TrimSuffix(e.ID, ":nitro")] = true
	}

	var newEntries []jsonlEntry
	for _, m := range apiModels {
		bare := stripProvider(m.ID)
		if bare == "" || existing[bare] {
			continue
		}
		af, ok := apiFields(m)
		if !ok {
			if af.negative && *verbose {
				fmt.Fprintf(os.Stderr, "  ⤫ %s: skipped (negative price $%.4f/$%.4f)\n", bare, af.in, af.out)
			}
			continue
		}
		// New model: effort/thinking/speed left false — curated by hand (not
		// derivable from the API; see the note in the verify section).
		ne := jsonlEntry{ID: bare, Provider: "openrouter", Dev: devFromAPIID(m.ID), Fetched: fetchDate}
		applyAPIFields(&ne, af)
		newEntries = append(newEntries, ne)
		existing[bare] = true
		if *verbose {
			fmt.Fprintf(os.Stderr, "  + %s: added ($%.4f/$%.4f per 1M)\n", bare, af.in, af.out)
		}
	}

	// Append-only: keep every historic row, add the new snapshots. Existing
	// rows in `entries` were never mutated.
	entries = append(entries, appended...)
	entries = append(entries, newEntries...)

	// --- Summary ---

	summary := fmt.Sprintf("%d new models, %d changed (rows appended), %d dev backfilled, %d unavailable",
		len(newEntries), len(appended), backfilledDevs, len(unavailable))

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

	wtPath, branch, err := createWorktree(repo, *baseFlag)
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
		removeWorktree(repo, wtPath, branch)
		fmt.Println(summary)
		return
	}

	committed, err := gitCommit(wtPath, fmt.Sprintf("modelinfo: sync with OpenRouter API (%s)", timestamp()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error committing:", err)
		removeWorktree(repo, wtPath, branch)
		fmt.Println(summary)
		return
	}
	if !committed {
		// Nothing changed — don't leave an empty review worktree/branch behind.
		removeWorktree(repo, wtPath, branch)
		fmt.Printf("%s\nno changes — nothing to commit\n", summary)
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
	// Group each model's history together and order it chronologically:
	// by ID, then by `fetched` (empty baseline rows first, then oldest→newest).
	// Stable diffs, and the latest row for a model is always its last line.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].ID != entries[j].ID {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Fetched < entries[j].Fetched
	})

	var buf strings.Builder
	for _, e := range entries {
		// Round prices to 6 dp on write. Prices are derived as apiString*1e6,
		// which introduces float noise (e.g. 0.1 → 0.09999999999999999); 6 dp
		// is far finer than any real per-1M price granularity and marshals
		// cleanly. `e` is a copy (range value), so this doesn't mutate callers.
		e.InputPer1M = round6(e.InputPer1M)
		e.OutputPer1M = round6(e.OutputPer1M)
		e.CacheReadPer1M = round6(e.CacheReadPer1M)
		e.CacheWritePer1M = round6(e.CacheWritePer1M)
		e.CacheWrite1hPer1M = round6(e.CacheWrite1hPer1M)
		e.InternalReasoningPer1M = round6(e.InternalReasoningPer1M)
		// web_search/image/audio are raw per-unit values (often < 1e-6) — do
		// NOT round them to 6dp or they'd vanish to zero.
		if len(e.PriceTiers) > 0 {
			tiers := make([]PriceTier, len(e.PriceTiers))
			copy(tiers, e.PriceTiers)
			for i := range tiers {
				tiers[i].InputPer1M = round6(tiers[i].InputPer1M)
				tiers[i].OutputPer1M = round6(tiers[i].OutputPer1M)
				tiers[i].CacheReadPer1M = round6(tiers[i].CacheReadPer1M)
				tiers[i].CacheWritePer1M = round6(tiers[i].CacheWritePer1M)
				tiers[i].CacheWrite1hPer1M = round6(tiers[i].CacheWrite1hPer1M)
			}
			e.PriceTiers = tiers
		}
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(buf.String()), 0644)
}

// round6 rounds to 6 decimal places, clearing the float noise from the
// apiString*1e6 price scaling.
func round6(f float64) float64 { return math.Round(f*1e6) / 1e6 }

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

// devFromAPIID extracts the model author/dev slug — the segment BEFORE the
// first '/' — from an OpenRouter API id (e.g. "moonshotai/kimi-k3" →
// "moonshotai"). This is the counterpart to stripProvider (which keeps the part
// AFTER the slash). A leading '~' (OpenRouter's shadow/variant listing marker,
// e.g. "~moonshotai/kimi-latest") is dropped so the dev is the plain author
// slug. Returns "" when the id has no '/'.
func devFromAPIID(id string) string {
	if i := strings.IndexByte(id, '/'); i > 0 {
		return strings.ToLower(strings.TrimPrefix(id[:i], "~"))
	}
	return ""
}

// stripProvider removes the "provider/" prefix from an OpenRouter model ID.
func stripProvider(id string) string {
	if i := strings.IndexByte(id, '/'); i > 0 {
		return id[i+1:]
	}
	return id
}

func createWorktree(repo, base string) (wtPath, branch string, err error) {
	branch = "sync-modelinfo-" + timestamp()
	// Worktree as a sibling of the repo, following foci convention.
	wtName := filepath.Base(repo) + "-wt-sync-modelinfo"
	wtPath = filepath.Join(filepath.Dir(repo), wtName)

	// Remove if stale from a previous run (best-effort).
	_ = os.RemoveAll(wtPath)

	cmd := exec.Command("git", "-c", "core.sharedRepository=false",
		"-C", repo, "worktree", "add", "-b", branch, wtPath, base)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return wtPath, branch, nil
}

// removeWorktree tears down a worktree and its branch — used when a run turns
// out to be a no-op, so an empty review branch isn't left littering the repo.
func removeWorktree(repo, wtPath, branch string) {
	_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wtPath).Run()
	_ = exec.Command("git", "-C", repo, "branch", "-D", branch).Run()
}

// gitCommit stages and commits the worktree. It returns committed=false (with
// no error) when there is nothing to commit, so the caller can tear down the
// empty worktree instead of leaving it behind.
func gitCommit(wt, msg string) (committed bool, err error) {
	cmd := exec.Command("git", "-C", wt, "add", "-A")
	if out, e := cmd.CombinedOutput(); e != nil {
		return false, fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), e)
	}
	// Nothing staged → nothing changed; not an error.
	if exec.Command("git", "-C", wt, "diff", "--cached", "--quiet").Run() == nil {
		return false, nil
	}
	cmd = exec.Command("git", "-C", wt, "commit", "-m", msg)
	if out, e := cmd.CombinedOutput(); e != nil {
		return false, fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), e)
	}
	return true, nil
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

// apiDerived holds the fields the sync treats the OpenRouter API as
// authoritative for (per 1M tokens; ctx in tokens). effort/thinking/speed are
// deliberately absent — see the note in the verify section.
type apiDerived struct {
	in, out               float64
	cacheRead, cacheWrite float64
	cacheWrite1h          float64
	internalReasoning     float64
	webSearch             float64 // raw $/call
	image, audio          float64 // raw per-unit
	tiers                 []PriceTier
	intelligence          float64
	caching               bool
	ctx                   int
	negative              bool // true if pricing was negative (for reporting)
}

// apiFields extracts the API-authoritative fields for a model, scaling prices
// to per-1M tokens. ok is false for unusable pricing: both-zero (free/clutter)
// or any negative (sentinel like the auto-router's -1, which would corrupt cost
// math). caching + cache prices come straight from pricing.input_cache_*.
func apiFields(m orModel) (apiDerived, bool) {
	pf := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
	in, out := pf(m.Pricing.Prompt), pf(m.Pricing.Completion)
	cr, cw := pf(m.Pricing.InputCacheRead), pf(m.Pricing.InputCacheWrite)
	f := apiDerived{
		in: in * 1e6, out: out * 1e6,
		cacheRead: cr * 1e6, cacheWrite: cw * 1e6,
		cacheWrite1h:      pf(m.Pricing.InputCacheWrite1h) * 1e6,
		internalReasoning: pf(m.Pricing.InternalReasoning) * 1e6,
		webSearch:         pf(m.Pricing.WebSearch), // raw $/call
		image:             pf(m.Pricing.Image),     // raw per-unit
		audio:             pf(m.Pricing.Audio),     // raw per-unit
		caching:           cr > 0 || cw > 0,
		ctx:               m.ContextLength,
		intelligence:      m.Benchmarks.ArtificialAnalysis.IntelligenceIndex,
	}
	for _, o := range m.Pricing.Overrides {
		f.tiers = append(f.tiers, PriceTier{
			MinPromptTokens:   o.MinPromptTokens,
			InputPer1M:        pf(o.Prompt) * 1e6,
			OutputPer1M:       pf(o.Completion) * 1e6,
			CacheReadPer1M:    pf(o.InputCacheRead) * 1e6,
			CacheWritePer1M:   pf(o.InputCacheWrite) * 1e6,
			CacheWrite1hPer1M: pf(o.InputCacheWrite1h) * 1e6,
		})
	}
	if in < 0 || out < 0 || cr < 0 || cw < 0 {
		f.negative = true
		return f, false
	}
	if in == 0 && out == 0 {
		return f, false
	}
	return f, true
}

// fieldsDiffer reports whether the API-authoritative fields differ from the
// current row (prices compared with a small epsilon; caching/context exact).
// A true result is what triggers appending a new history row.
func fieldsDiffer(cur jsonlEntry, f apiDerived) bool {
	return abs(f.in-cur.InputPer1M) > 0.005 ||
		abs(f.out-cur.OutputPer1M) > 0.005 ||
		abs(f.cacheRead-cur.CacheReadPer1M) > 0.005 ||
		abs(f.cacheWrite-cur.CacheWritePer1M) > 0.005 ||
		abs(f.cacheWrite1h-cur.CacheWrite1hPer1M) > 0.005 ||
		abs(f.internalReasoning-cur.InternalReasoningPer1M) > 0.005 ||
		abs(f.webSearch-cur.WebSearchPerCall) > 1e-9 ||
		abs(f.image-cur.ImagePrice) > 1e-9 ||
		abs(f.audio-cur.AudioPrice) > 1e-9 ||
		abs(f.intelligence-cur.IntelligenceIndex) > 0.05 ||
		f.caching != cur.Caching ||
		(f.ctx > 0 && f.ctx != cur.ContextWindow) ||
		!tiersEqual(f.tiers, cur.PriceTiers)
}

// tiersEqual compares tier schedules with the same per-1M epsilon as prices.
func tiersEqual(a, b []PriceTier) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].MinPromptTokens != b[i].MinPromptTokens ||
			abs(a[i].InputPer1M-b[i].InputPer1M) > 0.005 ||
			abs(a[i].OutputPer1M-b[i].OutputPer1M) > 0.005 ||
			abs(a[i].CacheReadPer1M-b[i].CacheReadPer1M) > 0.005 ||
			abs(a[i].CacheWritePer1M-b[i].CacheWritePer1M) > 0.005 ||
			abs(a[i].CacheWrite1hPer1M-b[i].CacheWrite1hPer1M) > 0.005 {
			return false
		}
	}
	return true
}

// applyAPIFields sets every API-authoritative field on an entry from a fresh
// fetch (used both when appending an update row and when adding a new model).
// Curated fields (effort/thinking/speed/comment/provider) are left untouched.
func applyAPIFields(e *jsonlEntry, f apiDerived) {
	e.InputPer1M = f.in
	e.OutputPer1M = f.out
	e.CacheReadPer1M = f.cacheRead
	e.CacheWritePer1M = f.cacheWrite
	e.CacheWrite1hPer1M = f.cacheWrite1h
	e.InternalReasoningPer1M = f.internalReasoning
	e.WebSearchPerCall = f.webSearch
	e.ImagePrice = f.image
	e.AudioPrice = f.audio
	e.PriceTiers = f.tiers
	e.IntelligenceIndex = f.intelligence
	e.Caching = f.caching
	if f.ctx > 0 {
		e.ContextWindow = f.ctx
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sync-modelinfo: "+format+"\n", args...)
	os.Exit(1)
}
