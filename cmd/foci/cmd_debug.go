package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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

	// Check file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return fmt.Errorf("session file not found: %s", filePath)
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

// parseTimeArg parses a time argument as either an RFC3339 timestamp or a
// relative duration like "1h", "30m", "2h30m" (interpreted as that duration ago).
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
	f, err := os.Open(path)
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
// Supports: bare agent name ("scout"), partial key ("scout/c123"), full key ("scout/c123/17095...").
// Bare names and partial keys dispatch to SessionIndex.ResolveLooseKey (the same
// resolver send_to_session uses); full keys (3+ segments) are returned as-is.
func resolveSessionKey(idx *session.SessionIndex, keyArg string) (string, error) {
	if strings.Count(keyArg, "/") >= 2 {
		// Full key (3+ segments) → use directly.
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
  rebuild-index        Rebuild session index from disk
  pprof on|off|status  Toggle or query the live /debug/pprof/* gate

Session key formats:
  scout                        Agent name (resolves to most recent active session)
  scout/c5970082313            Partial key (resolves to latest version)
  scout/c5970082313/1709590000 Full session key

Flags:
  --config <path>    Config file path (default: ~/config/foci.toml)
  --from <time>      Start of time range (RFC3339 or duration like "1h", "30m")
  --to <time>        End of time range (RFC3339 or duration like "1h", "30m")
  --format <fmt>     Output format: "human" (default) or "json"

When --from or --to is specified, matching messages are printed and the command
exits (no tailing). Without time range flags, the session is tailed live.
`)
}
