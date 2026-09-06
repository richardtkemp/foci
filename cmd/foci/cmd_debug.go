package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"foci/internal/config"
	"foci/internal/secrets"
	"foci/internal/session"

	"github.com/fsnotify/fsnotify"
)

// outputFormat controls how session lines are rendered.
type outputFormat int

const (
	outputHuman outputFormat = iota
	outputJSON
)

func cmdDebug(args []string) error {
	// Parse --config before subcommand dispatch so it can appear anywhere
	// (e.g. "foci debug --config path session scout").
	configPath, args := parseFlagValue(args, "config")

	if len(args) == 0 || wantsHelp(args) {
		debugUsage()
		return nil
	}

	subcmd := args[0]
	switch subcmd {
	case "session":
		return cmdDebugSession(args[1:], configPath)
	case "at":
		return cmdDebugAt(args[1:], configPath)
	case "rebuild-index":
		return cmdDebugRebuildIndex(configPath)
	case "pprof":
		return cmdDebugPprof(args[1:])
	default:
		return fmt.Errorf("unknown debug subcommand: %s", subcmd)
	}
}

func cmdDebugSession(args []string, configPath string) error {
	// --config may also appear after "session"; parse it from remaining args too.
	if flagVal, rest := parseFlagValue(args, "config"); flagVal != "" {
		configPath = flagVal
		args = rest
	}
	if configPath == "" {
		configPath = envDefault("", "FOCI_CONFIG")
	}
	if configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home dir: %w", err)
		}
		configPath = filepath.Join(home, "config", "foci.toml")
	}

	// Parse optional flags
	fromStr, args := parseFlagValue(args, "from")
	toStr, args := parseFlagValue(args, "to")
	formatStr, args := parseFlagValue(args, "format")

	if len(args) == 0 {
		return fmt.Errorf("usage: foci debug session <key> [--from <time>] [--to <time>] [--format human|json]")
	}
	keyArg := args[0]

	// Parse output format
	format := outputHuman
	switch formatStr {
	case "", "human":
		// default
	case "json":
		format = outputJSON
	default:
		return fmt.Errorf("unknown format %q: expected \"human\" or \"json\"", formatStr)
	}

	// Parse time range
	var fromTime, toTime time.Time
	hasTimeRange := fromStr != "" || toStr != ""
	if fromStr != "" {
		t, err := parseTimeArg(fromStr)
		if err != nil {
			return fmt.Errorf("parse --from: %w", err)
		}
		fromTime = t
	}
	if toStr != "" {
		t, err := parseTimeArg(toStr)
		if err != nil {
			return fmt.Errorf("parse --to: %w", err)
		}
		toTime = t
	}

	// Load config for paths
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	store := session.NewStore(cfg.Sessions.Dir)

	// Open session index read-only
	dbPath := cfg.DataPath("state.db")
	idx, err := session.OpenSessionIndexReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("open session index: %w", err)
	}
	defer idx.Close() //nolint:errcheck

	// Resolve session key
	sessionKey, err := resolveSessionKey(idx, keyArg)
	if err != nil {
		return err
	}

	// Resolve file path
	filePath, err := store.SessionPath(sessionKey)
	if err != nil {
		return fmt.Errorf("session path: %w", err)
	}

	// Check file exists. A root session with no root turns yet (only ever
	// cron/keepalive/reflection/background branch turns) never gets a
	// root.jsonl — see handleMissingRootSession.
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return handleMissingRootSession(sessionKey, filePath, hasTimeRange, fromTime, toTime, format)
	}

	// Print header
	fmt.Printf("── session: %s ──\n", sessionKey)
	fmt.Printf("── file: %s ──\n\n", filePath)

	if hasTimeRange {
		// Time range mode: filter and print once, then exit
		return printFilteredContent(filePath, fromTime, toTime, format)
	}

	// Follow mode: print existing content then tail
	offset, err := printExistingContent(filePath, format)
	if err != nil {
		return fmt.Errorf("read session: %w", err)
	}

	// Set up fsnotify watcher for tailing
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}
	defer watcher.Close() //nolint:errcheck

	// Watch the directory containing the file (fsnotify requires watching dirs on some platforms)
	watchDir := filepath.Dir(filePath)
	if err := watcher.Add(watchDir); err != nil {
		return fmt.Errorf("watch %s: %w", watchDir, err)
	}

	// Handle Ctrl+C
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	fmt.Fprintf(os.Stderr, "[tailing — Ctrl+C to stop]\n")

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			// Only process writes to our target file
			if event.Name != filePath {
				continue
			}
			if event.Op&fsnotify.Write == 0 {
				continue
			}
			newOffset, err := printNewContent(filePath, offset, format)
			if err != nil {
				fmt.Fprintf(os.Stderr, "read error: %v\n", err)
				continue
			}
			offset = newOffset

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			fmt.Fprintf(os.Stderr, "watcher error: %v\n", err)

		case <-sigCh:
			fmt.Fprintln(os.Stderr, "\n[stopped]")
			return nil
		}
	}
}

// handleMissingRootSession answers "foci debug session <key>" when the root
// session file (root.jsonl) does not exist on disk. This is the normal shape
// for an agent whose default session only ever receives branch turns (cron,
// keepalive, reflection, background spawns) — those turns each get their own
// b<epoch>.jsonl sibling file, and root.jsonl is never created (#1839).
// Rather than a bare "session file not found" (which reads as "this session
// does not exist" when it very much does), this reports the branch files
// found alongside it and, if a time range was requested, searches across them.
func handleMissingRootSession(sessionKey, filePath string, hasTimeRange bool, from, to time.Time, format outputFormat) error {
	dir := filepath.Dir(filePath)
	branches, _ := branchFilesInRootDir(dir)
	if len(branches) == 0 {
		return fmt.Errorf("session file not found: %s", filePath)
	}

	if hasTimeRange {
		fmt.Printf("── session: %s ──\n── no root.jsonl; searching %d branch file(s) in %s ──\n\n", sessionKey, len(branches), dir)
		return printBranchFilesInRange(branches, from, to, format)
	}

	var first, last time.Time
	for _, b := range branches {
		bf, bl, ok := branchFileSpan(b)
		if !ok {
			continue
		}
		if first.IsZero() || bf.Before(first) {
			first = bf
		}
		if last.IsZero() || bl.After(last) {
			last = bl
		}
	}
	spanMsg := "none with a usable time"
	if !first.IsZero() {
		spanMsg = fmt.Sprintf("spanning %s to %s (delegated-backend branches hold only their meta line; the messages are in conversation.db and the backend transcript)", first.Format(time.RFC3339), last.Format(time.RFC3339))
	}
	return fmt.Errorf(
		"no root.jsonl for %s — this session has only ever had branch turns (cron/keepalive/reflection/background), never a root/chat turn\n"+
			"  %d branch file(s) found in %s, %s\n"+
			"  retry with --from/--to to search across them, e.g.:\n"+
			"    foci debug session %s --from <start> --to <end>",
		sessionKey, len(branches), dir, spanMsg, sessionKey)
}

// branchFilesInRootDir returns the branch (child) files that live alongside a
// root session's root.jsonl — filenames "b<epoch>.jsonl" or
// "b<epoch>.jsonl.gz" (branches can be gzipped by the idle-archive sweep same
// as any other session file) — sorted oldest-first by their epoch.
func branchFilesInRootDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		path  string
		epoch int64
	}
	var cands []candidate
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		plain := strings.TrimSuffix(name, ".gz")
		if !strings.HasPrefix(plain, "b") || !strings.HasSuffix(plain, ".jsonl") {
			continue
		}
		epoch, ok := branchFileEpoch(name)
		if !ok {
			continue // not a b<epoch>.jsonl name
		}
		cands = append(cands, candidate{path: filepath.Join(dir, name), epoch: epoch})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].epoch < cands[j].epoch })
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.path
	}
	return out, nil
}

// branchFileEpoch parses the start epoch out of a "b<epoch>.jsonl[.gz]"
// branch file name.
func branchFileEpoch(name string) (int64, bool) {
	plain := strings.TrimSuffix(filepath.Base(name), ".gz")
	if !strings.HasPrefix(plain, "b") || !strings.HasSuffix(plain, ".jsonl") {
		return 0, false
	}
	epoch, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(plain, "b"), ".jsonl"), 10, 64)
	if err != nil {
		return 0, false
	}
	return epoch, true
}

// branchFileSpan returns the time span a branch file covers: the earliest
// and latest message timestamps in it (transparently decompressing .gz).
// A branch of a delegated (CC/codex) session holds only its branch_meta
// line — the messages live in the backend transcript and conversation.db,
// not here — so when no line carries a timestamp the span collapses to the
// branch's start time, taken from the b<epoch> file name. ok=false only when
// neither source yields a time.
func branchFileSpan(path string) (first, last time.Time, ok bool) {
	defer func() {
		if first.IsZero() {
			if epoch, epochOK := branchFileEpoch(path); epochOK {
				first = time.Unix(epoch, 0)
				last = first
				ok = true
			}
		}
	}()
	r, err := openSessionReader(path)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	defer r.Close() //nolint:errcheck

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		ts := lineTimestamp(line)
		if ts.IsZero() {
			continue
		}
		if first.IsZero() || ts.Before(first) {
			first = ts
		}
		if last.IsZero() || ts.After(last) {
			last = ts
		}
	}
	return first, last, !first.IsZero()
}

// spanOverlaps reports whether a file whose timestamped content runs
// [first, last] could hold anything in the requested [from, to] window. A
// zero from/to bound is open-ended.
func spanOverlaps(first, last, from, to time.Time) bool {
	if !to.IsZero() && first.After(to) {
		return false
	}
	if !from.IsZero() && last.Before(from) {
		return false
	}
	return true
}

// printBranchFilesInRange filters and prints each branch file whose span
// overlaps [from, to], labelled with its path so the source of each message
// is unambiguous when several branch files are searched at once.
func printBranchFilesInRange(branches []string, from, to time.Time, format outputFormat) error {
	found := false
	for _, path := range branches {
		first, last, ok := branchFileSpan(path)
		if !ok || !spanOverlaps(first, last, from, to) {
			continue
		}
		fmt.Printf("── branch file: %s ──\n\n", path)
		if err := printFilteredContent(path, from, to, format); err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		found = true
	}
	if !found {
		fmt.Println("(no branch files overlap that time range)")
	}
	return nil
}

// openSessionReader opens a session JSONL file for reading, transparently
// decompressing it if it is gzipped (branch files, like root files, can be
// gzipped by the idle-archive sweep). Caller must Close the result.
func openSessionReader(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(path, ".gz") {
		return f, nil
	}
	gr, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("gzip reader %s: %w", path, err)
	}
	return &gzipReadCloser{gr: gr, f: f}, nil
}

// gzipReadCloser closes both the gzip reader and its underlying file.
type gzipReadCloser struct {
	gr *gzip.Reader
	f  *os.File
}

func (g *gzipReadCloser) Read(p []byte) (int, error) { return g.gr.Read(p) }

func (g *gzipReadCloser) Close() error {
	_ = g.gr.Close()
	return g.f.Close()
}

// parseTimeArg parses a time argument as either an RFC3339 timestamp or a
// relative duration like "1h", "30m", "2h30m" (interpreted as that duration ago).
// cmdDebugAt answers "where is session <key>'s history at time <t>": prints
// the JSONL file covering that moment (live file, or the earliest archive
// rotated at-or-after it — provenance table first, filename stamps as
// fallback) and the CC resume ID observed live at that moment, if any.
func cmdDebugAt(args []string, configPath string) error {
	if flagVal, rest := parseFlagValue(args, "config"); flagVal != "" {
		configPath = flagVal
		args = rest
	}
	if configPath == "" {
		configPath = envDefault("", "FOCI_CONFIG")
	}
	if configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home dir: %w", err)
		}
		configPath = filepath.Join(home, "config", "foci.toml")
	}
	if len(args) < 2 {
		return fmt.Errorf("usage: foci debug at <key> <time>  (time: RFC3339 or duration ago like \"1h\")")
	}
	keyArg, timeArg := args[0], args[1]

	at, err := parseTimeArg(timeArg)
	if err != nil {
		return err
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	store := session.NewStore(cfg.Sessions.Dir)
	idx, err := session.OpenSessionIndexReadOnly(cfg.DataPath("state.db"))
	if err != nil {
		return fmt.Errorf("open session index: %w", err)
	}
	defer idx.Close() //nolint:errcheck

	sessionKey, err := resolveSessionKey(idx, keyArg)
	if err != nil {
		return err
	}

	fmt.Printf("session: %s\nat:      %s\n", sessionKey, at.Format(time.RFC3339))

	// File covering that moment: provenance table, filename-stamp fallback,
	// else the live file.
	path, archivedAt, ok := idx.ArchiveFileAt(sessionKey, at)
	source := "archive (recorded)"
	if !ok {
		path, archivedAt, ok = store.ArchiveFileAt(sessionKey, at)
		source = "archive (filename stamp)"
	}
	if ok {
		fmt.Printf("file:    %s\n         %s, archived %s\n", path, source, archivedAt.Format(time.RFC3339))
	} else {
		livePath, err := store.SessionPath(sessionKey)
		if err != nil {
			return fmt.Errorf("resolve live path: %w", err)
		}
		fmt.Printf("file:    %s (live)\n", livePath)
	}

	if id, observedAt, ok := idx.BackendResumeAt(sessionKey, at); ok {
		fmt.Printf("cc:      %s (observed %s)\n", id, observedAt.Format(time.RFC3339))
	} else {
		fmt.Printf("cc:      none observed at or before that time\n")
	}
	return nil
}

func parseTimeArg(s string) (time.Time, error) {
	// Try RFC3339 first
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}

	// Try as relative duration
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a valid RFC3339 timestamp or duration (e.g. \"1h\", \"30m\")", s)
	}
	return time.Now().UTC().Add(-d), nil
}

// inTimeRange returns true if ts falls within [from, to].
// Zero from means no lower bound; zero to means no upper bound.
// Returns false if ts is zero (message has no timestamp).
func inTimeRange(ts, from, to time.Time) bool {
	if ts.IsZero() {
		return false
	}
	if !from.IsZero() && ts.Before(from) {
		return false
	}
	if !to.IsZero() && ts.After(to) {
		return false
	}
	return true
}

// printFilteredContent reads a session file and prints only lines with timestamps
// in the given range. Meta lines (session_meta, branch_meta) are always included.
func printFilteredContent(path string, from, to time.Time, format outputFormat) error {
	f, err := openSessionReader(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		ts := lineTimestamp(line)
		if !ts.IsZero() && !inTimeRange(ts, from, to) {
			continue
		}

		out := renderLine(line, format)
		if out != "" {
			fmt.Print(out)
		}
	}
	return scanner.Err()
}

// renderLine formats a JSONL line according to the output format.
func renderLine(line []byte, format outputFormat) string {
	switch format {
	case outputJSON:
		return string(line) + "\n"
	default:
		return formatLine(line)
	}
}

// resolveSessionKey resolves a user-provided key argument to a full session key.
// Supports: bare agent name ("scout") — resolved to the agent's default session
// via SessionIndex.ResolveLooseKey (the same resolver send_to_session uses) —
// and full session keys ("scout/c123", "scout/c123/b1709596800"), which are
// returned as-is.
func resolveSessionKey(idx *session.SessionIndex, keyArg string) (string, error) {
	if strings.Contains(keyArg, "/") {
		// Full session key → use directly.
		return keyArg, nil
	}
	if key := idx.ResolveLooseKey(keyArg); key != "" {
		return key, nil
	}
	return "", fmt.Errorf("no active session found for %q", keyArg)
}

// printExistingContent reads and formats all existing lines in the session file.
// Returns the file offset after reading.
func printExistingContent(path string, format outputFormat) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		out := renderLine(line, format)
		if out != "" {
			fmt.Print(out)
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}

	// Get current file offset
	offset, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		// Scanner consumed the whole file; stat for size
		info, statErr := f.Stat()
		if statErr != nil {
			return 0, statErr
		}
		return info.Size(), nil
	}
	return offset, nil
}

// printNewContent reads and formats lines added since the given offset.
// Returns the new offset.
func printNewContent(path string, offset int64, format outputFormat) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return offset, err
	}
	defer f.Close() //nolint:errcheck

	// Check if file was truncated (rotation)
	info, err := f.Stat()
	if err != nil {
		return offset, err
	}
	if info.Size() < offset {
		// File was truncated — read from beginning
		offset = 0
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		out := renderLine(line, format)
		if out != "" {
			fmt.Print(out)
		}
	}
	if err := scanner.Err(); err != nil {
		return offset, err
	}

	newOffset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return info.Size(), nil
	}
	return newOffset, nil
}

func cmdDebugRebuildIndex(configPath string) error {
	if configPath == "" {
		configPath = envDefault("", "FOCI_CONFIG")
	}
	if configPath == "" {
		home, _ := os.UserHomeDir()
		configPath = filepath.Join(home, "config", "foci.toml")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config %s: %w", configPath, err)
	}

	sessions := session.NewStore(cfg.Sessions.Dir)
	idx, err := session.NewSessionIndex(cfg.DataPath("state.db"))
	if err != nil {
		return fmt.Errorf("open state.db: %w", err)
	}
	defer func() { _ = idx.Close() }()

	fmt.Fprintf(os.Stderr, "Rebuilding session index from %s...\n", cfg.Sessions.Dir)
	n, err := idx.Rebuild(sessions)
	if err != nil {
		return fmt.Errorf("rebuild: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Done: %d sessions indexed\n", n)

	pruned := idx.PruneOrphans()
	if pruned > 0 {
		fmt.Fprintf(os.Stderr, "Pruned %d orphan entries\n", pruned)
	}
	return nil
}

// cmdDebugPprof toggles or queries the live pprof gate on a running gateway.
// Usage: foci debug pprof on|off|status
func cmdDebugPprof(args []string) error {
	if len(args) == 0 || wantsHelp(args) {
		fmt.Fprintln(os.Stderr, "Usage: foci debug pprof on|off|status")
		fmt.Fprintln(os.Stderr, "  Toggle or query the live /debug/pprof/* gate on the running gateway.")
		fmt.Fprintln(os.Stderr, "  No restart needed — uses the /-/pprof admin endpoint.")
		return nil
	}
	action := args[0]
	var wantEnabled *bool
	switch action {
	case "on":
		t := true
		wantEnabled = &t
	case "off":
		f := false
		wantEnabled = &f
	case "status":
		// nil body = GET
	default:
		return fmt.Errorf("unknown pprof action %q: expected on, off, or status", action)
	}

	store, err := secrets.Load(resolveSecretsPath(""))
	if err != nil {
		return fmt.Errorf("load secrets: %w", err)
	}
	addr := envDefault("localhost:7420", "FOCI_ADDR")

	c := &http.Client{Timeout: 3 * time.Second}
	var u string
	if sock := resolveGWSocket(""); sock != "" {
		c.Transport = unixSocketTransport(sock)
		u = "http://foci-gw/-/pprof"
	} else {
		u = fmt.Sprintf("http://%s/-/pprof", addr)
		if apiKey, _ := store.Get("http.api_key"); apiKey != "" {
			c.Transport = &authTransport{key: apiKey}
		}
	}

	method := http.MethodGet
	var bodyReader io.Reader
	if wantEnabled != nil {
		method = http.MethodPost
		payload, _ := json.Marshal(map[string]bool{"enabled": *wantEnabled})
		bodyReader = strings.NewReader(string(payload))
	}
	req, err := http.NewRequest(method, u, bodyReader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("gateway not reachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var result struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if result.Enabled {
		fmt.Println("pprof: enabled")
	} else {
		fmt.Println("pprof: disabled")
	}
	return nil
}

func debugUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci debug <subcommand> [args...]

Subcommands:
  session <key>        Tail a session file with formatted output
  at <key> <time>      Point-in-time lookup: which JSONL file (live or archive)
                       holds the session's history at <time>, and which CC
                       resume ID was live then. <time> is RFC3339 or a
                       duration ago ("1h", "30m").
  rebuild-index        Rebuild session index from disk
  pprof on|off|status  Toggle or query the live /debug/pprof/* gate

Session key formats:
  scout                Agent name (resolves to the agent's default session)
  scout/c5970082313    Full session key (chat)
  scout/iresearch      Full session key (named independent)

Flags:
  --config <path>    Config file path (default: ~/config/foci.toml)
  --from <time>      Start of time range (RFC3339 or duration like "1h", "30m")
  --to <time>        End of time range (RFC3339 or duration like "1h", "30m")
  --format <fmt>     Output format: "human" (default) or "json"

When --from or --to is specified, matching messages are printed and the command
exits (no tailing). Without time range flags, the session is tailed live.
`)
}
