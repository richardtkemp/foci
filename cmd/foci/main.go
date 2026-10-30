package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "foci/internal/delegator/all" // register every delegated backend
	"foci/internal/fociroot"
)

// Build info — set via ldflags: go build -ldflags "-X main.version=... -X main.gitCommit=... -X main.buildTime=..."
var (
	version   = "dev"
	gitCommit = "unknown"
	buildTime = "unknown"
	goVersion = runtime.Version()
)

const defaultAddr = "127.0.0.1:18791"

var client = &http.Client{Timeout: 5 * time.Minute}

// Convention: every CLI flag must have a corresponding FOCI_ env var, and every
// FOCI_ env var must have a corresponding CLI flag. Resolution order: flag > env > default.
// When adding new flags, add the env var fallback in parseSendFlags (for send flags)
// or cmdBranch (for branch-specific flags), and update the usage() text for both.

// envDefault returns val if non-empty, otherwise falls back to the env var.
func envDefault(val, envKey string) string {
	if val != "" {
		return val
	}
	return os.Getenv(envKey)
}

// envBool returns true if val is true, or the env var is non-empty.
func envBool(val bool, envKey string) bool {
	return val || os.Getenv(envKey) != ""
}

// isHelpWord reports whether tok is the help word in either spelling —
// the one predicate every help check shares: the whole-line scan
// (wantsHelp), the first-token checks (cmdTodo, cmdSecrets) and the
// leading flag region of the one placement scan (scanCommandLine — a
// help word before the free text asks for help; inside the text it is
// text).
func isHelpWord(tok string) bool {
	return tok == "-h" || tok == "--help"
}

// wantsHelp returns true if args contain -h or --help.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if isHelpWord(a) {
			return true
		}
	}
	return false
}

// flagMatch reports whether tok is the named flag in either of its two
// spellings — "--name value", with the value in the NEXT token (attached
// false), or "--name=value", carrying it on the token itself — and returns
// the attached value when there is one. This is the one place the two
// spellings are told apart: parseFlagValue, the global flag scan,
// parseAgentFlag and the todo agent-flag scan all delegate here, so they
// cannot drift.
func flagMatch(tok, name string) (value string, attached bool, ok bool) {
	if tok == name {
		return "", false, true
	}
	if strings.HasPrefix(tok, name+"=") {
		return tok[len(name)+1:], true, true
	}
	return "", false, false
}

// agentFlag reports whether tok is the agent flag in either spelling,
// returning the attached value when there is one. Shared by parseAgentFlag
// (send/branch/command and the todo leading-flag scan), so every scanner
// recognises the same -a/--agent forms.
func agentFlag(tok string) (value string, attached, ok bool) {
	if v, a, ok := flagMatch(tok, "-a"); ok {
		return v, a, true
	}
	return flagMatch(tok, "--agent")
}

// takeAgentFlag consumes the agent flag at args[i] together with its value
// and returns the value plus next, the index of the first token after the
// pair — the one value step every scanner that reads -a/--agent shares
// (parseAgentFlag and the one placement scan, scanCommandLine). The =value
// spellings carry the value on the token itself; the detached spellings
// take the next token. taken is false when args[i] is not the agent flag,
// and also when a detached spelling has no next token: its value is
// missing, which the callers report as flagNeedsValue or leave to their
// own text rules.
func takeAgentFlag(args []string, i int) (value string, next int, taken bool) {
	v, attached, ok := agentFlag(args[i])
	if !ok {
		return "", i, false
	}
	if attached {
		return v, i + 1, true
	}
	if i+1 >= len(args) {
		return "", i, false
	}
	return args[i+1], i + 2, true
}

// flagNeedsValue is the one wording for a value-flag typed where its value
// cannot follow it — a dangling -a before the todo id, a dangling -s or
// gate before the command family's text: one line naming the flag only.
// flagName keeps an attached value (an API key, an address) out of it.
func flagNeedsValue(tok string) error {
	return fmt.Errorf("flag %s needs a value", flagName(tok))
}

// duplicateAgentFlag is the one wording for an -a/--agent typed again in
// the leading flag run, where the scan already holds an agent: one
// line naming the rule and where the flag belongs — before the landmark
// the scan guards (the id for `todo done`, the text for every other
// free-text command). Only a COMPLETE pair is a duplicate; a dangling
// spelling is flagNeedsValue's to report.
func duplicateAgentFlag(landmark string) error {
	return fmt.Errorf("only one -a/--agent may precede the %s", landmark)
}

// detachedAgentWord reports whether tok is the -a/--agent spelling that
// carries no value on the token itself — the one value-flag word every
// free-text command shares, which is why the one scan needs it outside
// any command's own flag set.
func detachedAgentWord(tok string) bool {
	_, attached, ok := agentFlag(tok)
	return ok && !attached
}

// leadSpec declares the command-specific half of the one placement scan:
// the command word its connection-flag error names, the landmark its
// duplicate -a error names (where the free text starts — "text", or the
// id for `todo done`), and whether a literal -- ends the flag region and
// is dropped (`todo done` allows one so an id that looks like a flag can
// still be named).
type leadSpec struct {
	word     string
	landmark string
	dashDash bool
}

// ownFlagLine is what a command contributes to the one scan beyond the
// flags every command shares: its own flags, other than -a/--agent,
// consumed in the scan's single pass over the leading flag region.
type ownFlagLine interface {
	// consumeOwnFlag parses the command's own flag at args[i] and
	// returns the index of the last token it consumed — the same
	// convention as tryParseFlagArg, so those parses pass their index
	// straight through; the scan resumes at the following token.
	// consumed is false when args[i] is not one — including a detached
	// spelling whose value is missing, which the scan's landmark checks
	// report.
	consumeOwnFlag(args []string, i int) (last int, consumed bool)
	// ownValueFlagWord reports whether tok is the detached spelling of
	// one of the command's own value-taking flags — the word half of
	// consumeOwnFlag, minus the agent flag the scan itself owns.
	ownValueFlagWord(tok string) bool
}

// noOwnFlags is the own-flag set of the free-text commands whose only
// leading flag is the shared agent selector: `eval` (a shell command is
// the text) and `todo done` (the id, then the reason).
type noOwnFlags struct{}

func (noOwnFlags) consumeOwnFlag(_ []string, _ int) (int, bool) { return 0, false }
func (noOwnFlags) ownValueFlagWord(_ string) bool               { return false }

// leadScan is what the one scan returns: the selected agent (with the
// FOCI_AGENT fallback every command applies already in it), whether
// -h/--help appeared in the flag region, and the words from the landmark
// on, verbatim.
type leadScan struct {
	agent string
	help  bool
	rest  []string
}

// finish applies the agent env fallback shared by every command the scan
// serves (each command's own env defaults stay with the command).
func (l leadScan) finish() leadScan {
	l.agent = envDefault(l.agent, "FOCI_AGENT")
	return l
}

// scanCommandLine is the ONE scan over a command line's flag region —
// the operator's one placement rule (Dick, 2026-10-17), written once:
// flags are interpreted only BEFORE the free text starts. It walks args
// in a single pass, consuming the shared -a/--agent pair and the
// command's own flags (own), and stops at the landmark: the first token
// none of them consumes, which is where the command's free text (or, for
// `todo done`, its id) starts. From there the words belong to the
// caller, verbatim — never parsed as a flag again, never refused,
// whatever they look like.
//
// freeText draws the disposition. True for the commands whose arguments
// are free text (command, send, branch, eval, todo done): the scan stops
// at the landmark and decides the three boundary refusals there, so no
// command repeats them — a connection flag cannot start free text (the
// transport was fixed before the command word was dispatched, so the
// error says where the flag goes, naming the flag only: an "=value" tail
// may be a credential); a detached value-flag spelling that ends the
// line there has no value, and dispatching its bare word as text would
// type a command nobody meant; and a second COMPLETE -a pair sits where
// the line reads flags and is refused rather than silently dispatched.
// Help (-h/--help) is recognised in the flag region: inside the text it
// is text. False for the fixed-word shorthands ping and pair (no free
// text): own flags are consumed wherever they sit and every other word
// is returned in place — the historical whole-line scan, where a second
// -a pair and a valueless flag word stay words, dispatched as typed.
func scanCommandLine(args []string, own ownFlagLine, spec leadSpec, freeText bool) (leadScan, error) {
	var lead leadScan
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if spec.dashDash && tok == "--" {
			lead.rest = args[i+1:] // a literal -- ends the flag region and is dropped
			return lead.finish(), nil
		}
		if freeText && isHelpWord(tok) {
			lead.help = true
			lead.rest = args[i:]
			return lead.finish(), nil
		}
		if v, next, taken := takeAgentFlag(args, i); taken {
			if lead.agent == "" {
				lead.agent = v
				i = next - 1
				continue
			}
			if freeText {
				return leadScan{}, duplicateAgentFlag(spec.landmark)
			}
			// Whole-line disposition, agent already selected: the pair
			// stays ordinary words — fall through to the word handling.
		} else if last, consumed := own.consumeOwnFlag(args, i); consumed {
			i = last
			continue
		}
		if freeText {
			// The landmark starts here. A connection flag cannot (it
			// cannot route the request from there); flagName keeps an
			// attached value (an API key, an address) out of the line.
			if _, ok := matchGlobalFlag(tok); ok {
				return leadScan{}, fmt.Errorf("%s must come before the %s word", flagName(tok), spec.word)
			}
			if i+1 >= len(args) && (detachedAgentWord(tok) || own.ownValueFlagWord(tok)) {
				return leadScan{}, flagNeedsValue(tok)
			}
			lead.rest = args[i:]
			return lead.finish(), nil
		}
		lead.rest = append(lead.rest, tok)
	}
	return lead.finish(), nil
}

// parseFlagValue extracts a flag value from args, returning the value and remaining args.
// Supports both --flag value and --flag=value formats.
func parseFlagValue(args []string, flagName string) (value string, rest []string) {
	for i := 0; i < len(args); i++ {
		v, attached, ok := flagMatch(args[i], "--"+flagName)
		if !ok {
			continue
		}
		if attached {
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+1:]...)
			return v, rest
		}
		if i+1 >= len(args) {
			continue // trailing --flag with no value: keep it for the caller
		}
		v = args[i+1]
		rest = append(rest, args[:i]...)
		rest = append(rest, args[i+2:]...)
		return v, rest
	}
	return "", args
}

// globalFlags is the set of flags main consumes itself: the gateway
// address, the HTTP API key and the gateway socket, each taking one
// value.
type globalFlags struct {
	addr   string
	apiKey string
	socket string
}

// globalFlagTable is the ONE declaration of those flags: each row pairs a
// flag's name (as scanned, without the leading dashes) with the
// globalFlags field its value is stored into. The token recogniser
// (matchGlobalFlag) and the scanner (parseGlobalFlags) both walk this
// table, and parseGlobalFlags reads the flags in its order — a new global
// flag is one row here plus the field it stores into, nothing else.
var globalFlagTable = []struct {
	name string
	set  func(*globalFlags, string)
}{
	{"addr", func(f *globalFlags, v string) { f.addr = v }},
	{"api-key", func(f *globalFlags, v string) { f.apiKey = v }},
	{"socket", func(f *globalFlags, v string) { f.socket = v }},
}

// matchGlobalFlag reports whether tok is one of the global flags, in
// either spelling, and whether it carries its value on the token itself
// (the =value form) rather than in the next token. No caller needs the
// value here — the scanner reads values through parseFlagValue.
func matchGlobalFlag(tok string) (attached, ok bool) {
	for _, g := range globalFlagTable {
		if _, a, ok := flagMatch(tok, "--"+g.name); ok {
			return a, true
		}
	}
	return false, false
}

// parseGlobalFlags extracts the global flags from args and returns their
// values plus args with them removed — the whole-line scan every command
// outside freeTextCommands uses (`foci status -a x --addr host:port`
// works; the free-text commands read them only from the leading run, see
// main). Each flag is read once, in globalFlagTable order.
func parseGlobalFlags(args []string) (globalFlags, []string) {
	var f globalFlags
	rest := args
	for _, g := range globalFlagTable {
		v, r := parseFlagValue(rest, g.name)
		g.set(&f, v)
		rest = r
	}
	return f, rest
}

// splitGlobalFlags splits argv into the leading run of global flags (with
// their values) and the rest, which starts at the command word. The scan
// stops at the first token that is not a global flag — including unknown
// -arguments, which belong to the command — so it locates the command
// word without ever consuming a command's own arguments. The
// freeTextCommands use the split to bound where their global flags may
// appear; see main.
func splitGlobalFlags(args []string) (flags, rest []string) {
	i := 0
	for i < len(args) {
		attached, ok := matchGlobalFlag(args[i])
		if !ok {
			break
		}
		if attached {
			i++ // --flag=value carries its own value
			continue
		}
		if i+1 >= len(args) {
			break // trailing --flag with no value: leave it for the dispatch
		}
		i += 2 // --flag value
	}
	return args[:i], args[i:]
}

// freeTextCommands names the gateway commands whose own arguments are
// free text that may look like a flag — `todo` (its close reason is
// arbitrary words; cmd_todo.go), `command` (a dispatched slash command
// carries arbitrary words; cmd_admin.go), and `send`, `branch` and `eval`
// (a chat message, a branch text or a shell command is free text the
// same way; cmd_send.go and cmd_admin.go). A command whose arguments are
// free text registers itself here; main reads the global flags of these
// commands only from the leading run before the command word, while every
// other command keeps the historical whole-line scan.
var freeTextCommands = map[string]bool{
	"todo":    true,
	"command": true,
	"send":    true,
	"branch":  true,
	"eval":    true,
}

// flagName cuts an attached value off a flag-shaped word: "--api-key=sekret"
// reports "--api-key". An error line naming a misplaced or malformed flag
// names the flag only — the value can be an API key, a socket path or an
// address, and the line reaches cron logs and the landing's log, so it must
// never be echoed.
func flagName(w string) string {
	if i := strings.IndexByte(w, '='); i >= 0 {
		return w[:i]
	}
	return w
}

// authTransport is an http.RoundTripper that injects an Authorization: Bearer
// header on every request. Wraps an underlying transport.
type authTransport struct {
	key  string
	base http.RoundTripper
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.key)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// unixSocketTransport returns an http.Transport that dials a Unix domain socket.
// No API key is needed — the kernel verifies peer credentials on the socket.
func unixSocketTransport(sockPath string) *http.Transport {
	return &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", sockPath)
		},
	}
}

// resolveGWSocket returns the gateway Unix socket path if one is available.
// Resolution order: --socket flag > FOCI_GW_SOCK env var > default path.
// Returns empty string if no socket is found. main checks an explicit
// --socket first (checkExplicitSocket), so only the env var and the
// default path can come back empty here and fall back to TCP.
func resolveGWSocket(flagValue string) string {
	sock := envDefault(flagValue, "FOCI_GW_SOCK")
	if sock != "" {
		if isSocket(sock) {
			return sock
		}
		return ""
	}
	// Default: <install root>/data/foci-gw.sock ($FOCI_HOME, else $HOME)
	home, err := fociroot.Dir()
	if err != nil {
		return ""
	}
	sock = filepath.Join(home, "data", "foci-gw.sock")
	if isSocket(sock) {
		return sock
	}
	return ""
}

// checkExplicitSocket refuses an explicit --socket path that is not a Unix
// socket, naming the path and the cause. An empty path (no flag) is fine:
// the FOCI_GW_SOCK env var and the default path keep their fall-back to
// TCP, which tests and scripts use on purpose (FOCI_GW_SOCK=/nonexistent).
func checkExplicitSocket(path string) error {
	if path == "" {
		return nil
	}
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("--socket %s: no such file; no request was sent", path)
	case err != nil:
		return fmt.Errorf("--socket %s: %v; no request was sent", path, err)
	case fi.Mode()&os.ModeSocket == 0:
		return fmt.Errorf("--socket %s: not a Unix socket; no request was sent", path)
	}
	return nil
}

// isSocket returns true if path exists and is a Unix socket.
func isSocket(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSocket != 0
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	// Handle "foci auth" and "foci first-run" before normal command dispatch — they don't need a gateway.
	if os.Args[1] == "auth" {
		if err := cmdAuth(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "auth failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if os.Args[1] == "first-run" {
		if err := cmdSetup(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "first-run failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if os.Args[1] == "secrets" {
		if err := cmdSecrets(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "secrets: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if os.Args[1] == "pretool" {
		if err := cmdPretool(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "pretool: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if os.Args[1] == "debug" {
		if err := cmdDebug(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "debug: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// The global transport flags are read — and removed — from the whole
	// command line, as ever, so `foci status -a x --addr host:port` works.
	// The freeTextCommands are the exception: their own arguments are
	// free text that may look like a global flag, so there the flags are
	// read only from the leading run before the command word, and the
	// dispatch line starts at the command word — words that would parse
	// as globals stay in the command's own arguments.
	var globals globalFlags
	var allArgs []string
	if flags, rest := splitGlobalFlags(os.Args[1:]); len(rest) > 0 && freeTextCommands[rest[0]] {
		globals, _ = parseGlobalFlags(flags)
		allArgs = rest
	} else {
		globals, allArgs = parseGlobalFlags(os.Args[1:])
	}

	// Transport resolution: prefer Unix socket (no secret needed), fall back to TCP + API key.
	// An explicit --socket never falls back: a mistyped path would send the
	// request — a write, for todo done — to whatever listens on the TCP
	// address instead.
	if err := checkExplicitSocket(globals.socket); err != nil {
		logStderr("error: %v", err)
		os.Exit(1)
	}
	var base string
	if sock := resolveGWSocket(globals.socket); sock != "" {
		client.Transport = unixSocketTransport(sock)
		// URL host is ignored by the unix transport; use a placeholder.
		base = "http://foci-gw"
	} else {
		addr := envDefault(globals.addr, "FOCI_ADDR")
		if addr == "" {
			addr = defaultAddr
		}
		apiKey := envDefault(globals.apiKey, "FOCI_API_KEY")
		if apiKey != "" {
			client.Transport = &authTransport{key: apiKey, base: client.Transport}
		}
		base = "http://" + addr
	}

	if err := runGatewayCommand(base, allArgs); err != nil {
		// The single funnel for every command's fatal error, which is why
		// stamping here covers the HTTP 412s and the "reading message file"
		// failures that fill cron.log without stamping each call site.
		// errAlreadyReported is the one error whose line was printed in its
		// final position ahead of the usage block; it keeps the exit code.
		if !errors.Is(err, errAlreadyReported) {
			logStderr("error: %v", err)
		}
		os.Exit(1)
	}
}

// errAlreadyReported marks an error whose message runGatewayCommand has
// already printed in its final position — the unknown-command verdict
// goes FIRST, ahead of the usage block, so the usage does not bury what
// was wrong (the order the CLI always used). main gives it the exit
// code without printing it a second time.
var errAlreadyReported = errors.New("foci: already reported")

// runGatewayCommand dispatches one gateway command — main's command switch,
// extracted so tests can drive the real dispatch in-process (the black-box
// tests exec a built binary, which coverage cannot see). allArgs is the
// command line with the global flags already parsed away, so allArgs[0] is
// the command. Usage-level failures (no command, unknown command) print the
// usage and return an error for main's stamped funnel — the process still
// exits 1, but a caller in-process gets the error instead of dying with it;
// a command's own error is returned the same way.
func runGatewayCommand(base string, allArgs []string) error {
	if len(allArgs) == 0 {
		usage()
		return errors.New("a command is required (usage above)")
	}
	cmd := allArgs[0]
	args := allArgs[1:]

	var err error
	switch cmd {
	case "send":
		err = cmdSend(base, args)
	case "branch":
		err = cmdBranch(base, args)
	case "status":
		err = cmdStatus(base, args)
	case "score":
		err = cmdScore(base, args)
	case "evals":
		err = cmdEvals(base, args)
	case "eval":
		err = cmdEval(base, args)
	case "command":
		err = cmdCommand(base, args)
	case "todo":
		err = cmdTodo(base, args)
	case "ping":
		if wantsHelp(args) {
			pingUsage()
		} else {
			err = cmdPing(base, args)
		}
	case "pair-key", "pairkey", "pair":
		if wantsHelp(args) {
			pairKeyUsage()
		} else {
			err = cmdPair(base, args)
		}
	case "auth":
		// Already handled above main dispatch, but list here for completeness.
		err = cmdAuth(args)
	case "help", "--help", "-h":
		usage()
	case "version", "--version", "-v":
		fmt.Printf("foci %s (commit %s, built %s, %s)\n", version, gitCommit, buildTime, goVersion)
	default:
		// The verdict line first, then the usage — the usage block must
		// not bury what was wrong.
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		return errAlreadyReported
	}
	return err
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: foci <command> [args...]

Commands:
  first-run            First-run setup wizard (config, auth, character files)
  auth                 Authenticate with Anthropic (setup token from Claude Code)
  secrets              Manage secrets (list, get, set, delete)
  send <text>          Send a message to the agent (main session)
  branch [text]        Trigger a branch session
                         --no-compact      Skip compaction if context limit reached
                         --no-reset-hook   Skip pre-reset memory hook
                         --oneshot          Quick task: no compaction, no reset hook
  status               Query agent status
  score <name> <value> Record a human score on a turn's trace (see foci score -h)
  evals list           List the loaded scoring rubrics
  eval <command>       Ask the agent to run a shell command
  command </cmd>       Dispatch a slash command (e.g. /ping, /cache)
  todo done <id> <reason>
                       Close one todo of an agent with a reason (-a required; see
                       foci todo done -h). Used by services without an agent exec,
                       like the factory's landing
  ping                 Shorthand for 'command /ping'
  pair [host]          Mint a single-use Android pairing key (-a for agent)
  debug session <key>  Tail a session file with formatted output
  pretool list|test    Check PreToolUse rules against the config (see foci pretool -h)
  version              Print version information

Flags:
  --socket <path>      Gateway Unix socket (auto-detected from ~/data/foci-gw.sock); an error if not a socket
  --addr <host:port>   Gateway address (default: %s)
  --api-key <key>      HTTP API key (for TCP auth; not needed with Unix socket)
  (those three may sit anywhere on the line, except after the "todo",
   "command", "send", "branch" or "eval" word — the commands whose own
   arguments are free text (a close reason, a dispatched slash command's
   words, a message, a branch text, a shell command); there they would
   land inside that text, which is stored and dispatched typed exactly,
   whatever it looks like)
  -a, --agent <id>     Target a specific agent (default: first agent; todo done requires one)
  -s, --session <id>   Target a specific session (default: main)
  --if-active <dur>    Skip if no user activity within duration (e.g. 8h, 30m)
  -mt, --message-text  Message text (default: trailing args)
  -mf, --message-file  Read message from file path

Environment (flag > env var > default):
  FOCI_GW_SOCK         Gateway Unix socket path (--socket, auto-detected)
  FOCI_ADDR            Gateway address (--addr)
  FOCI_API_KEY         HTTP API key (--api-key, not needed with Unix socket)
  FOCI_AGENT           Target agent (-a)
  FOCI_SESSION         Target session (-s)
  FOCI_IF_ACTIVE       Activity gate duration (--if-active)
  FOCI_SYNC            Wait for response (--sync/--wait, non-empty = true)
  FOCI_ASYNC           Fire-and-forget (--async/--no-wait, non-empty = true)
  FOCI_MESSAGE_TEXT    Message text (-mt)
  FOCI_MESSAGE_FILE    Message file path (-mf)
  FOCI_NO_COMPACT      Skip compaction (--no-compact, non-empty = true)
  FOCI_NO_RESET_HOOK   Skip reset hook (--no-reset-hook, non-empty = true)
  FOCI_ONESHOT         Oneshot mode (--oneshot, non-empty = true)
`, defaultAddr)
}

// parseAgentFlag extracts -a/--agent from args, returning the agent ID and
// remaining args. Returns empty string if no flag is present. The spellings
// and the value step are agentFlag/takeAgentFlag, the one shared authority;
// a trailing flag with no value is left for the caller (takeAgentFlag takes
// nothing without a value). The whole-line commands that are not free text
// use this (status, score, evals); the free-text commands get their agent
// from the one scan, which owns the same step.
func parseAgentFlag(args []string) (agentID string, rest []string) {
	for i := 0; i < len(args); i++ {
		v, next, taken := takeAgentFlag(args, i)
		if !taken {
			continue
		}
		rest = append(rest, args[:i]...)
		rest = append(rest, args[next:]...)
		return v, rest
	}
	// Env var fallback
	return envDefault("", "FOCI_AGENT"), args
}

// postSend* bound and instrument the connection-retry loop in postJSON (#998):
// when the CLI is invoked from cron/a script while the daemon is mid-restart,
// retry the dial every 5s for up to a minute before failing loudly. The last
// two are test seams.
var (
	postSendRetryEvery  = 5 * time.Second
	postSendGiveUpAfter = time.Minute
	postSendSleep       = time.Sleep
	postSendNow         = time.Now
)

// transportError marks a failure to obtain any response at all — the POST
// itself failed (dial refused, timeout) — so postJSON's retry loop can tell
// "daemon unreachable" (retry) from an error a served response produced
// (never retry). The message is the underlying error's, unchanged.
type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// postJSONOnce posts body — already JSON-encoded — exactly ONCE and hands
// the response to printResponse. For callers that need a scriptable
// verdict: `foci todo done` is run by the factory's landing, which must
// fail fast with one stderr line naming the todo, not banner-and-retry
// for a minute while the gateway restarts. A failure to reach the gateway
// at all comes back as a *transportError.
func postJSONOnce(url string, body []byte) error {
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return &transportError{err}
	}
	defer func() { _ = resp.Body.Close() }()
	return printResponse(resp)
}

func postJSON(url string, body interface{}) error {
	// The body is encoded ONCE, before the retry loop: a retry re-sends
	// the same bytes, it never re-marshals — the value cannot change
	// between attempts, so encoding it again is work for nothing.
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	// Retry ONLY transport-level failures (daemon unreachable — dial
	// refused). A returned *http.Response, even a non-2xx one, means the
	// daemon is up and is handled by printResponse; those are never retried.
	deadline := postSendNow().Add(postSendGiveUpAfter)
	for {
		err := postJSONOnce(url, data)
		if err == nil {
			return nil
		}
		var te *transportError
		if !errors.As(err, &te) {
			return err
		}
		if !postSendNow().Before(deadline) {
			fmt.Fprintf(os.Stderr, "\n*** foci unreachable — gave up after %s. Message NOT sent. Last error: %v ***\n",
				postSendGiveUpAfter, err)
			return fmt.Errorf("foci daemon unreachable after %s: %w", postSendGiveUpAfter, err)
		}
		fmt.Fprintf(os.Stderr, "foci unreachable (%v) — retrying in %s...\n", err, postSendRetryEvery)
		postSendSleep(postSendRetryEvery)
	}
	// postJSONOnce owns each response body, and every path through the
	// loop returns, so the function ends with the loop.
}

// logStderr writes one ISO-8601-stamped line to stderr.
//
// STDERR ONLY, deliberately. Every agent crontab appends both streams to one
// shared /home/foci/logs/cron.log, where an undated line cannot be attributed to
// a job or even a day: that file holds 4,591 "skipped: session recently active"
// and 229 "HTTP 412" lines with nothing to tell them apart, which is how a
// three-day delivery gap went unnoticed (#1787). Stdout is a DATA channel —
// `foci send ... | jq` and the "queued" single-token contract both depend on it
// being unprefixed — so it is left exactly as it was, and the attribution is
// carried on stderr instead, where nothing parses it.
func logStderr(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, a...))
}

func printResponse(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Try to extract "response" or "status" field from JSON. The routing
	// receipt (which session the message landed in, and which resolution
	// rung matched) goes to stderr so cron logs show where a send actually
	// went without polluting stdout for pipelines.
	var result struct {
		Response    string `json:"response"`
		Status      string `json:"status"`
		Session     string `json:"session"`
		ResolvedVia string `json:"resolved_via"`
		Held        string `json:"held"`
	}
	if json.Unmarshal(body, &result) == nil {
		// The receipt carries the STATUS too. Without it the only record of a
		// no-op run ("skipped: session recently active") is the bare stdout token,
		// which is undated and unattributable in a shared cron log — the whole
		// point of #1787. Duplicating one word on stderr is the cheap half of
		// that; corrupting stdout would be the expensive half. A held receipt
		// (the session is busy with background work, #2233) appends the reason
		// so the log says why nothing happened yet.
		if result.Session != "" || result.Status != "" {
			if result.Held != "" {
				logStderr("session: %s (%s) status=%s held=%s", result.Session, result.ResolvedVia, result.Status, result.Held)
			} else {
				logStderr("session: %s (%s) status=%s", result.Session, result.ResolvedVia, result.Status)
			}
		}
		if result.Response != "" {
			fmt.Println(result.Response)
			return nil
		}
		if result.Status != "" {
			fmt.Println(result.Status)
			return nil
		}
	}

	// Fallback: print raw body
	fmt.Print(string(body))
	return nil
}
