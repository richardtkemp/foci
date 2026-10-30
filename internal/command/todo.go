package command

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"foci/internal/display"
	"foci/internal/memory"
	"foci/internal/tools"
)

// todoArgs holds parsed arguments for the /todo command.
type todoArgs struct {
	subcommand string   // "", "new", "done", "start", "drop", "reopen", "edit", "show", "search", "rm", "stats" (each has aliases)
	ids        []int64  // target IDs for transitions/edit/show/rm
	badID      string   // first word of an id-list verb's arguments that is not a todo id — the verb refuses, changing nothing
	text       string   // free text (new item text, search query, edit text)
	reason     string   // close reason: the words after the first id of "done <id> <words>", byte for byte; "" for every other form
	tags       []string // t:TAG values (multiple for AND filtering)
	setTag     bool     // true if t: was explicitly provided (allows clearing)
	priority   string   // p:PRIORITY value
	status     string   // status filter for list mode
	sort       string   // sort field for list mode
	reverse    bool     // reverse sort order
	limit      int      // max results
}

// parseTodoArgs parses the raw argument string into a todoArgs struct.
// Phase 1: detect subcommand from the first word (one cutWord split — the
// raw remainder is kept for verbs that need it verbatim).
// Phase 2: parse the remaining words based on the mode.
func parseTodoArgs(raw string) todoArgs {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return todoArgs{status: "active", sort: "priority", limit: 15}
	}

	a := todoArgs{status: "active", sort: "priority", limit: 15}

	first, rest := cutWord(raw)

	switch strings.ToLower(first) {
	case "new", "add", "create":
		a.subcommand = "new"
		parseNewArgs(&a, strings.Fields(rest))
		return a
	case "start", "begin":
		a.subcommand = "start"
		a.ids, a.badID = parseIDs(strings.Fields(rest))
		return a
	case "drop", "cancel", "abandon":
		a.subcommand = "drop"
		a.ids, a.badID = parseIDs(strings.Fields(rest))
		return a
	case "reopen":
		a.subcommand = "reopen"
		a.ids, a.badID = parseIDs(strings.Fields(rest))
		return a
	case "edit", "update", "modify":
		a.subcommand = "edit"
		parseEditArgs(&a, strings.Fields(rest))
		return a
	case "show", "info", "detail":
		a.subcommand = "show"
		a.ids, a.badID = parseIDs(strings.Fields(rest))
		return a
	case "get":
		a.subcommand = "get"
		parseGetArgs(&a, strings.Fields(rest))
		return a
	case "search", "find":
		a.subcommand = "search"
		a.text = strings.Join(strings.Fields(rest), " ")
		return a
	case "rm", "remove", "delete", "del":
		a.subcommand = "rm"
		a.ids, a.badID = parseIDs(strings.Fields(rest))
		return a
	case "stats", "summary":
		a.subcommand = "stats"
		parseListArgs(&a, strings.Fields(rest))
		return a
	case "done", "complete", "finish", "close":
		// /todo done means CLOSE and nothing else (Richard, 2026-10-17):
		// the words after the verb must start with a plain decimal id.
		// Ids only (/todo done 7 8) closes them all with the empty
		// reason; when any text follows, only the first word is an id and
		// the rest is the reason, byte for byte (Richard, 2026-10-30:
		// /todo done 7 landed abc). A first word that is not an id, or no
		// words at all, is an error — never a listing; done todos list
		// through /todo list done.
		a.subcommand = "done"
		a.ids, a.reason, a.badID = parseDoneArgs(rest)
		return a
	}

	// List mode: parse all tokens for status/sort/reverse/limit/t:TAG/p:PRIORITY
	parseListArgs(&a, strings.Fields(raw))
	return a
}

// cutWord splits s at its first run of whitespace: the first word, and the
// rest with leading whitespace removed. The raw remainder (not Fields tokens)
// is what keeps a done-reason's spacing, quotes and newlines intact.
func cutWord(s string) (word, rest string) {
	i := strings.IndexFunc(s, unicode.IsSpace)
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimLeftFunc(s[i:], unicode.IsSpace)
}

// parseDoneArgs reads the raw text after the done verb. Two shapes
// (Richard, 2026-10-30):
//   - every word is a plain decimal id (memory.ParseTodoID — digits only):
//     all of them are closed, with the empty reason (done 7 8);
//   - otherwise ONLY THE FIRST word is an id, and the raw remainder after
//     it is the close reason, verbatim (done 2457 2 commits landed closes
//     #2457 alone, reason "2 commits landed").
//
// A first word that is not an id (done 14x landed abc, done t:x, done +14)
// is returned as badID so the verb's refusal names it and changes nothing;
// an empty rest leaves the ids empty, which the verb answers with its usage
// line — done never falls back to listing.
func parseDoneArgs(rest string) (ids []int64, reason, badID string) {
	first, remainder := cutWord(rest)
	if first == "" {
		return nil, "", ""
	}
	firstID, isID := memory.ParseTodoID(first)
	if !isID {
		return nil, "", first
	}
	words := strings.Fields(remainder)
	all := []int64{firstID}
	for _, w := range words {
		id, ok := memory.ParseTodoID(w)
		if !ok {
			// Text follows: only the first word is an id.
			return []int64{firstID}, remainder, ""
		}
		all = append(all, id)
	}
	return all, "", ""
}

// parseListArgs extracts list-mode options from tokens in any order.
func parseListArgs(a *todoArgs, tokens []string) {
	for _, tok := range tokens {
		lower := strings.ToLower(tok)

		// Negated tag: -t:TAG or !t:TAG → store as "!TAG"
		if strings.HasPrefix(lower, "-t:") || strings.HasPrefix(lower, "!t:") {
			a.tags = append(a.tags, "!"+tok[3:])
			a.setTag = true
			continue
		}
		// Negated priority: -p:PRIO or !p:PRIO → store as "!PRIO"
		if strings.HasPrefix(lower, "-p:") || strings.HasPrefix(lower, "!p:") {
			a.priority = "!" + strings.ToLower(tok[3:])
			continue
		}
		// t:TAG (including t:!TAG which naturally stores "!TAG")
		if strings.HasPrefix(lower, "t:") {
			a.tags = append(a.tags, tok[2:])
			a.setTag = true
			continue
		}
		// p:PRIORITY (including p:!PRIO which naturally stores "!PRIO")
		if strings.HasPrefix(lower, "p:") {
			a.priority = strings.ToLower(tok[2:])
			continue
		}

		switch lower {
		// Status tokens
		case "open":
			a.status = "open"
		case "done", "closed", "complete", "completed", "finish", "finished":
			a.status = "done"
		case "all":
			a.status = ""
		case "active":
			a.status = "active"
		case "dropped":
			a.status = "dropped"
		case "started":
			a.status = "started"

		// Sort tokens
		case "created":
			a.sort = "created"
		case "updated":
			a.sort = "updated"
		case "priority":
			a.sort = "priority"

		// Modifiers
		case "reverse":
			a.reverse = true

		default:
			// nN → limit (e.g. n5, n10); bare number → limit
			if len(tok) > 1 && (tok[0] == 'n' || tok[0] == 'N') {
				if n, err := strconv.Atoi(tok[1:]); err == nil && n > 0 {
					a.limit = n
				}
			} else if n, err := strconv.Atoi(tok); err == nil && n > 0 {
				a.limit = n
			}
		}
	}
}

// parseNewArgs extracts tag, priority, and text from tokens for the "new" subcommand.
func parseNewArgs(a *todoArgs, tokens []string) {
	var textParts []string
	for _, tok := range tokens {
		lower := strings.ToLower(tok)
		if strings.HasPrefix(lower, "t:") {
			a.tags = append(a.tags, tok[2:])
			a.setTag = true
			continue
		}
		if strings.HasPrefix(lower, "p:") {
			a.priority = strings.ToLower(tok[2:])
			continue
		}
		textParts = append(textParts, tok)
	}
	a.text = strings.Join(textParts, " ")
}

// parseEditArgs extracts the first token as an ID, then tag/priority/text
// from the rest. The id grammar is the one every todo surface shares
// (memory.ParseTodoID, digits only): a decorated word like "+14" is not
// an id, so it leaves ids empty and the verb answers with its usage line
// instead of editing ticket 14.
func parseEditArgs(a *todoArgs, tokens []string) {
	if len(tokens) == 0 {
		return
	}
	// First token must be the ID
	if id, ok := memory.ParseTodoID(tokens[0]); ok {
		a.ids = []int64{id}
	}
	var textParts []string
	for _, tok := range tokens[1:] {
		lower := strings.ToLower(tok)
		if strings.HasPrefix(lower, "t:") {
			a.tags = append(a.tags, tok[2:])
			a.setTag = true
			continue
		}
		if strings.HasPrefix(lower, "p:") {
			a.priority = strings.ToLower(tok[2:])
			continue
		}
		textParts = append(textParts, tok)
	}
	a.text = strings.Join(textParts, " ")
}

// parseGetArgs parses tokens for the "get" subcommand, which combines
// filters and full-text search. If "/" is present, the left side is parsed
// for filters and the right side is the search query. Without "/", recognised
// filter tokens are extracted and remaining tokens become the search query.
func parseGetArgs(a *todoArgs, tokens []string) {
	// Check for explicit "/" delimiter.
	slashIdx := -1
	for i, tok := range tokens {
		if tok == "/" {
			slashIdx = i
			break
		}
	}

	if slashIdx >= 0 {
		parseListArgs(a, tokens[:slashIdx])
		a.text = strings.Join(tokens[slashIdx+1:], " ")
		return
	}

	// No delimiter — greedy-match recognised filter tokens, collect the rest
	// as search terms.
	var searchParts []string
	for _, tok := range tokens {
		lower := strings.ToLower(tok)

		// Negated tag: -t:TAG or !t:TAG → store as "!TAG"
		if strings.HasPrefix(lower, "-t:") || strings.HasPrefix(lower, "!t:") {
			a.tags = append(a.tags, "!"+tok[3:])
			a.setTag = true
			continue
		}
		// Negated priority: -p:PRIO or !p:PRIO → store as "!PRIO"
		if strings.HasPrefix(lower, "-p:") || strings.HasPrefix(lower, "!p:") {
			a.priority = "!" + strings.ToLower(tok[3:])
			continue
		}
		if strings.HasPrefix(lower, "t:") {
			a.tags = append(a.tags, tok[2:])
			a.setTag = true
			continue
		}
		if strings.HasPrefix(lower, "p:") {
			a.priority = strings.ToLower(tok[2:])
			continue
		}

		switch lower {
		case "open", "done", "closed", "complete", "completed", "finish", "finished",
			"all", "active", "dropped", "started",
			"created", "updated", "priority",
			"reverse":
			// Re-use parseListArgs logic for this single token.
			parseListArgs(a, []string{tok})
		default:
			// #880: a bare digits-only id selects a todo by ID (get 500 →
			// #500; the one id grammar, memory.ParseTodoID — a decorated
			// word like "+500" is not an id, it is a search term); the
			// count-limit form is nN (get n500 → limit 500).
			if len(tok) > 1 && (tok[0] == 'n' || tok[0] == 'N') {
				if n, err := strconv.Atoi(tok[1:]); err == nil && n > 0 {
					a.limit = n
					break
				}
			}
			if id, ok := memory.ParseTodoID(tok); ok && id > 0 {
				a.ids = append(a.ids, id)
			} else {
				searchParts = append(searchParts, tok)
			}
		}
	}
	a.text = strings.Join(searchParts, " ")
}

// parseIDs reads an id-list verb's argument words as todo ids
// (memory.ParseTodoID — digits only). A word that is not an id is never
// skipped: it is returned as bad so the verb can refuse the whole command
// naming it — acting on the ids so far would change some todos and
// silently leave the rest (/todo drop 3 +4 once dropped only #3, and
// before that read +4 as another id and dropped #4 too).
func parseIDs(tokens []string) (ids []int64, bad string) {
	for _, tok := range tokens {
		id, ok := memory.ParseTodoID(tok)
		if !ok {
			return ids, tok
		}
		ids = append(ids, id)
	}
	return ids, ""
}

// TodoCommand returns the /todo slash command.
func TodoCommand() *Command {
	return &Command{
		Name:        "todo",
		Description: "Manage todo items",
		Category:    "observability",
		// Declared for /help and the operator gate only (non_operator_commands
		// can name "todo new"); parseTodoArgs parses the verbs itself, so
		// Register does not auto-wire dispatch from this list. Keep the
		// names and aliases in step with parseTodoArgs (#2279). Listing is
		// the bare command and the list forms (/todo list done); "done"
		// itself always closes.
		Subcommands: []Subcommand{
			{Name: "new", Aliases: []string{"add", "create"}, Description: "Create a new item"},
			{Name: "done", Aliases: []string{"complete", "finish", "close"}, Description: "Mark items done (optional reason)"},
			{Name: "start", Aliases: []string{"begin"}, Description: "Mark items started"},
			{Name: "drop", Aliases: []string{"cancel", "abandon"}, Description: "Drop items"},
			{Name: "reopen", Description: "Reopen items"},
			{Name: "edit", Aliases: []string{"update", "modify"}, Description: "Edit an item"},
			{Name: "show", Aliases: []string{"info", "detail"}, Description: "Show an item in full"},
			{Name: "get", Description: "Filter and search"},
			{Name: "search", Aliases: []string{"find"}, Description: "Full-text search"},
			{Name: "rm", Aliases: []string{"remove", "delete", "del"}, Description: "Delete items"},
			{Name: "stats", Aliases: []string{"summary"}, Description: "Counts"},
		},
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			if cc.TodoStore == nil {
				return Response{Text: "Todo store not configured."}, nil
			}
			if strings.TrimSpace(req.Args) == "" {
				return Response{Text: todoUsage()}, nil
			}
			args := parseTodoArgs(req.Args)
			agentID := cc.AgentConfig.ID
			format := resolveTodoFormat(cc)

			// The one refusal ahead of the dispatch: the id-list verbs and
			// done flag the first word of their argument list that is not a
			// todo id (args.badID, from parseIDs or parseDoneArgs), and the
			// whole command refuses on it before any store call, so a
			// refused list changes nothing.
			if args.badID != "" {
				return todoIDRefusal(args)
			}

			switch args.subcommand {
			case "new":
				return todoNewCmd(cc.TodoStore, agentID, args)
			case "done":
				return todoBatchTransition(cc.TodoStore, agentID, args, "done")
			case "start":
				return todoBatchTransition(cc.TodoStore, agentID, args, "started")
			case "drop":
				return todoBatchTransition(cc.TodoStore, agentID, args, "dropped")
			case "reopen":
				return todoBatchTransition(cc.TodoStore, agentID, args, "open")
			case "edit":
				return todoEditCmd(cc.TodoStore, agentID, args)
			case "show":
				return todoShowCmd(cc.TodoStore, agentID, args)
			case "get":
				return todoGetCmd(cc.TodoStore, agentID, args, format)
			case "search":
				return todoSearchCmd(cc.TodoStore, agentID, args, format)
			case "rm":
				return todoRmCmd(cc.TodoStore, agentID, args)
			case "stats":
				return todoStatsCmd(cc.TodoStore, agentID, args)
			default:
				return todoListCmd(cc.TodoStore, agentID, args, format)
			}
		},
		KeyboardOptions: func(_ context.Context, _ CommandContext) []KeyboardOption {
			return []KeyboardOption{
				{Label: "list", Data: "list"},
				{Label: "new", Data: "new "},
				{Label: "stats", Data: "stats"},
				{Label: "all", Data: "all"},
				// "list done", not "done": the done button lists done todos,
				// and the bare done verb is the usage error since /todo done
				// means close and nothing else (Richard, 2026-10-17).
				{Label: "done", Data: "list done"},
			}
		},
	}
}

// todoUsage returns the help text listing all available /todo verbs.
func todoUsage() string {
	return "Usage: /todo <verb> [args]\n" +
		"\n" +
		"  new <text> [p:PRIO] [t:TAG]  Create a new item (aliases: add, create)\n" +
		"  done <id> [reason]           Mark done, recording the reason (aliases: complete, finish, close).\n" +
		"  done <id> <id>...            With a reason, only the first word is an id and the rest is the\n" +
		"                               reason, kept as typed (done 7 2 commits landed closes #7 only).\n" +
		"                               Ids only (done 7 8) closes them all. Done never lists (use\n" +
		"                               /todo list done)\n" +
		"  start <ids>                  Mark started (alias: begin)\n" +
		"  drop <ids>                   Drop (aliases: cancel, abandon)\n" +
		"  reopen <ids>                 Reopen\n" +
		"  edit <id> [fields] [text]    Edit (aliases: update, modify)\n" +
		"  show <id>                    Full detail (aliases: info, detail)\n" +
		"  get [filters] [search]       Filter + search\n" +
		"  search <query>               Full-text search (alias: find)\n" +
		"  rm <ids>                     Delete (aliases: remove, delete, del)\n" +
		"  stats [filters]              Counts (alias: summary)\n" +
		"  list [filters]               List items (aliases: all, active, open, closed, …)\n" +
		"\n" +
		"An <id> is a digits-only ticket number; get and edit read theirs\n" +
		"with the same grammar (a signed word like \"+7\" is search text or\n" +
		"the usage line, never ticket 7). In an id list (start, drop,\n" +
		"reopen, show, rm, and done's first word) a word that is not one is an error\n" +
		"naming it, and nothing is changed.\n" +
		"\n" +
		"Filters: all, open, active, done, started, dropped, t:TAG, p:PRIO, created, updated, priority, reverse, <N> (limit)"
}

// resolveTodoFormat returns the effective todo list format: "table" or "lines".
func resolveTodoFormat(cc CommandContext) string {
	if cc.ResolvedLive != nil {
		if f := cc.ResolvedLive.Load().Tools.TodoFormat; f != "" {
			return f
		}
	}
	return "lines"
}

// formatTodoList formats todo items using the configured format.
func formatTodoList(items []memory.TodoItem, format string) string {
	if format == "table" {
		return tools.FormatTodoTable(items)
	}
	return tools.FormatTodoLines(items)
}

// todoListCmd lists todos with the given filters.
func todoListCmd(store *memory.TodoStore, agentID string, args todoArgs, format string) (Response, error) {
	items, err := store.List(agentID, args.status, args.tags, args.priority, args.sort, args.reverse, args.limit)
	if err != nil {
		return Response{}, fmt.Errorf("list todos: %w", err)
	}
	if len(items) == 0 {
		label := "active"
		if args.status != "" && args.status != "active" {
			label = args.status
		} else if args.status == "" {
			label = ""
		}
		if label == "" {
			return Response{Text: "No todos."}, nil
		}
		return Response{Text: fmt.Sprintf("No %s todos.", label)}, nil
	}
	header := fmt.Sprintf("Todos (%d)", len(items))
	text := header + "\n\n" + formatTodoList(items, format)
	// See #1957: a capped result and a complete one otherwise look
	// identical, so flag it whenever the cap was actually hit.
	if args.limit > 0 && len(items) == args.limit {
		if total, cerr := store.CountList(agentID, args.status, args.tags, args.priority); cerr == nil && total > len(items) {
			text += fmt.Sprintf("\n\n— showing %d of %d matches; pass a higher limit (<N>) to see the rest", len(items), total)
		}
	}
	return Response{Text: text}, nil
}

// todoNewCmd creates a new todo item.
func todoNewCmd(store *memory.TodoStore, agentID string, args todoArgs) (Response, error) {
	if args.text == "" {
		return Response{Text: "Usage: /todo new <text>"}, nil
	}
	pri, err := memory.NormalizePriority(args.priority)
	if err != nil {
		return Response{Text: err.Error()}, nil
	}
	id, err := store.Add(agentID, args.text, pri, strings.Join(args.tags, ","))
	if err != nil {
		return Response{}, fmt.Errorf("add todo: %w", err)
	}
	if pri == "" {
		pri = "medium"
	}
	return Response{Text: fmt.Sprintf("Added #%d (%s): %s", id, pri, args.text)}, nil
}

// todoIDRefusal is the id-list verbs' answer when their arguments carry a
// word that is not a todo id (args.badID): one line naming the verb and
// the word. Asked once in TodoCommand's Execute, before the verb dispatch,
// so a refused list changes nothing.
func todoIDRefusal(args todoArgs) (Response, error) {
	return Response{Text: fmt.Sprintf("/todo %s: %q is not a todo id (ids are digits only); nothing was changed", args.subcommand, args.badID)}, nil
}

// todoBatchTransition transitions one or more todos to the target state.
// Command-initiated transitions use an empty reason — except "done", where
// the words after the first id (args.reason, set only by the done parser) are the
// recorded close reason. The no-ids usage line is bare `done`'s answer too
// (Richard, 2026-10-17): done means close and nothing else, so it names the
// reason form rather than pointing anywhere else.
func todoBatchTransition(store *memory.TodoStore, agentID string, args todoArgs, state string) (Response, error) {
	if len(args.ids) == 0 {
		if args.subcommand == "done" {
			return Response{Text: "Usage: /todo done <ids> [reason]"}, nil
		}
		return Response{Text: fmt.Sprintf("Usage: /todo %s <id> [id...]", args.subcommand)}, nil
	}
	var results []string
	for _, id := range args.ids {
		if err := store.Transition(agentID, id, state, args.reason); err != nil {
			results = append(results, fmt.Sprintf("#%d: error: %v", id, err))
		} else {
			results = append(results, fmt.Sprintf("#%d → %s", id, state))
		}
	}
	return Response{Text: strings.Join(results, "\n")}, nil
}

// todoEditCmd edits a todo item's fields.
func todoEditCmd(store *memory.TodoStore, agentID string, args todoArgs) (Response, error) {
	if len(args.ids) == 0 {
		return Response{Text: "Usage: /todo edit <id> [p:PRIORITY] [t:TAG] [new text]"}, nil
	}
	id := args.ids[0]
	if args.text == "" && args.priority == "" && !args.setTag {
		return Response{Text: "Nothing to change. Use p:PRIORITY, t:TAG, or provide new text."}, nil
	}
	if _, err := memory.NormalizePriority(args.priority); err != nil {
		return Response{Text: err.Error()}, nil
	}
	item, err := store.Edit(agentID, id, args.text, args.priority, strings.Join(args.tags, ","), args.setTag, false)
	if err != nil {
		return Response{}, fmt.Errorf("edit todo: %w", err)
	}
	tags := memory.FormatTags(item.Tags)
	return Response{Text: fmt.Sprintf("#%d updated: [%s]%s %s", item.ID, item.Priority, tags, item.Text)}, nil
}

// todoShowCmd shows detailed info for a single todo item.
func todoShowCmd(store *memory.TodoStore, agentID string, args todoArgs) (Response, error) {
	if len(args.ids) == 0 {
		return Response{Text: "Usage: /todo show <id> [id ...]"}, nil
	}
	var parts []string
	for _, id := range args.ids {
		item, err := store.Get(agentID, id)
		if err != nil {
			parts = append(parts, fmt.Sprintf("#%d: %v", id, err))
			continue
		}
		parts = append(parts, formatTodoDetail(item))
	}
	return Response{Text: strings.Join(parts, "\n\n")}, nil
}

// todoSearchCmd searches todos by text.
func todoSearchCmd(store *memory.TodoStore, agentID string, args todoArgs, format string) (Response, error) {
	if args.text == "" {
		return Response{Text: "Usage: /todo search <query>"}, nil
	}
	items, err := store.Search(agentID, args.text, &memory.TodoSearchOpts{
		Limit: args.limit,
	})
	if err != nil {
		return Response{}, fmt.Errorf("search todos: %w", err)
	}
	if len(items) == 0 {
		return Response{Text: fmt.Sprintf("No todos matching %q.", args.text)}, nil
	}
	text := fmt.Sprintf("Search: %q (%d)\n\n%s", args.text, len(items), formatTodoList(items, format))
	text += searchTruncationNotice(len(items), args.limit)
	return Response{Text: text}, nil
}

// searchTruncationNotice returns a trailing note when a search result may
// have been capped by --limit. Unlike List, Search has no cheap exact total
// (bleve doesn't index status/tags/priority, so matches are post-filtered
// after an overfetch) — so per #1957 this flags the cap without claiming to
// know the true count; a bare marker still turns the dangerous silence into
// a signal.
func searchTruncationNotice(returned, limit int) string {
	effectiveLimit := limit
	if effectiveLimit <= 0 {
		effectiveLimit = 10
	}
	if returned != effectiveLimit {
		return ""
	}
	return fmt.Sprintf("\n\n— result may be truncated at the limit of %d; pass a higher limit (<N>) to see more", effectiveLimit)
}

// todoGetCmd combines structured filters with optional full-text search.
// If a search query is present, uses Search with tag/priority/status/sort opts.
// If no search query, falls back to List (pure filter mode).
func todoGetCmd(store *memory.TodoStore, agentID string, args todoArgs, format string) (Response, error) {
	// #880: bare IDs (get 500) resolve to specific todos, shown in detail.
	if len(args.ids) > 0 {
		var parts []string
		for _, id := range args.ids {
			item, err := store.Get(agentID, id)
			if err != nil {
				parts = append(parts, fmt.Sprintf("#%d: %v", id, err))
				continue
			}
			parts = append(parts, formatTodoDetail(item))
		}
		return Response{Text: strings.Join(parts, "\n\n")}, nil
	}
	if args.text != "" {
		items, err := store.Search(agentID, args.text, &memory.TodoSearchOpts{
			Status:   args.status,
			Sort:     args.sort,
			Reverse:  args.reverse,
			Limit:    args.limit,
			Tags:     args.tags,
			Priority: args.priority,
		})
		if err != nil {
			return Response{}, fmt.Errorf("get todos: %w", err)
		}
		if len(items) == 0 {
			return Response{Text: fmt.Sprintf("No todos matching %q.", args.text)}, nil
		}
		text := fmt.Sprintf("Get: %q (%d)\n\n%s", args.text, len(items), formatTodoList(items, format))
		text += searchTruncationNotice(len(items), args.limit)
		return Response{Text: text}, nil
	}

	// No search query — pure filter mode via List.
	return todoListCmd(store, agentID, args, format)
}

// todoRmCmd hard-deletes one or more todos.
func todoRmCmd(store *memory.TodoStore, agentID string, args todoArgs) (Response, error) {
	ids := args.ids
	if len(ids) == 0 {
		return Response{Text: "Usage: /todo rm <id> [id...]"}, nil
	}
	var results []string
	for _, id := range ids {
		if err := store.Remove(agentID, id); err != nil {
			results = append(results, fmt.Sprintf("#%d: error: %v", id, err))
		} else {
			results = append(results, fmt.Sprintf("#%d removed", id))
		}
	}
	return Response{Text: strings.Join(results, "\n")}, nil
}

// todoStatsCmd shows counts by status and tag.
// Accepts filters via args: status (default "active" for tag counts), priority, tag.
// Use "/todo stats all" to include done/dropped in tag counts.
func todoStatsCmd(store *memory.TodoStore, agentID string, args todoArgs) (Response, error) {
	// Always fetch all items for the status breakdown.
	items, err := store.List(agentID, "", args.tags, args.priority, "priority", false, 0)
	if err != nil {
		return Response{}, fmt.Errorf("list todos: %w", err)
	}
	if len(items) == 0 {
		return Response{Text: "No todos."}, nil
	}

	statusCounts := map[string]int{}
	tagCounts := map[string]int{}
	tagStatus := args.status // default "active" from parser
	for _, item := range items {
		statusCounts[item.Status]++
		if !matchesStatus(item.Status, tagStatus) {
			continue
		}
		if item.Tags != "" {
			for _, t := range strings.Split(item.Tags, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					tagCounts[t]++
				}
			}
		}
	}

	// Status table.
	statusCols := []display.Column{
		{Header: "Status"},
		{Header: "Count", Align: display.AlignRight},
	}
	var statusRows [][]string
	for _, s := range []string{"open", "started", "done", "dropped"} {
		if c := statusCounts[s]; c > 0 {
			statusRows = append(statusRows, []string{s, strconv.Itoa(c)})
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Todos — %d total\n\n", len(items)))
	sb.WriteString(display.MarkdownTable(statusCols, statusRows))

	// Tag table.
	if len(tagCounts) > 0 {
		tagHeader := "Active"
		if tagStatus == "" {
			tagHeader = "Count"
		}
		tagCols := []display.Column{
			{Header: "Tag"},
			{Header: tagHeader, Align: display.AlignRight},
		}
		tags := make([]string, 0, len(tagCounts))
		for t := range tagCounts {
			tags = append(tags, t)
		}
		sort.Strings(tags)
		tagRows := make([][]string, 0, len(tags))
		for _, t := range tags {
			tagRows = append(tagRows, []string{t, strconv.Itoa(tagCounts[t])})
		}
		sb.WriteString("\n\n")
		sb.WriteString(display.MarkdownTable(tagCols, tagRows))
	}

	return Response{Text: strings.TrimRight(sb.String(), "\n")}, nil
}

// matchesStatus reports whether itemStatus passes the given filter.
// Empty filter matches all; "active" matches open and started;
// otherwise exact match.
func matchesStatus(itemStatus, filter string) bool {
	switch filter {
	case "":
		return true
	case "active":
		return itemStatus == "open" || itemStatus == "started"
	default:
		return itemStatus == filter
	}
}

// formatTodoDetail formats a single todo item with full detail.
func formatTodoDetail(item *memory.TodoItem) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("#%d %s\n", item.ID, item.Text))
	sb.WriteString(fmt.Sprintf("  Status:   %s\n", item.Status))
	sb.WriteString(fmt.Sprintf("  Priority: %s\n", item.Priority))
	tags := memory.FormatTags(item.Tags)
	if tags != "" {
		sb.WriteString(fmt.Sprintf("  Tags:    %s\n", strings.TrimSpace(tags)))
	}
	sb.WriteString(fmt.Sprintf("  Created:  %s (%s)\n", item.CreatedAt.Format(time.RFC3339), display.RelativeTime(item.CreatedAt)))
	if !item.UpdatedAt.IsZero() && !item.UpdatedAt.Equal(item.CreatedAt) {
		sb.WriteString(fmt.Sprintf("  Updated:  %s (%s)\n", item.UpdatedAt.Format(time.RFC3339), display.RelativeTime(item.UpdatedAt)))
	}
	if item.CompletedAt != nil {
		sb.WriteString(fmt.Sprintf("  Closed:   %s (%s)\n", item.CompletedAt.Format(time.RFC3339), display.RelativeTime(*item.CompletedAt)))
	}
	if item.CloseReason != "" {
		sb.WriteString(fmt.Sprintf("  Reason:   %s\n", item.CloseReason))
	}
	return strings.TrimRight(sb.String(), "\n")
}
