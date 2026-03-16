package command

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"foci/internal/display"
	"foci/internal/memory"
	"foci/internal/tools"
)

// todoArgs holds parsed arguments for the /todo command.
type todoArgs struct {
	subcommand string   // "", "new", "done", "start", "drop", "reopen", "edit", "show", "search", "rm", "stats"
	ids        []int64  // target IDs for transitions/edit/show/rm
	text       string   // free text (new item text, search query, edit text)
	tags       []string // t:TAG values (multiple for AND filtering)
	setTag     bool     // true if t: was explicitly provided (allows clearing)
	priority   string   // p:PRIORITY value
	status     string   // status filter for list mode
	sort       string   // sort field for list mode
	reverse    bool     // reverse sort order
	limit      int      // max results
}

// parseTodoArgs parses the raw argument string into a todoArgs struct.
// Phase 1: detect subcommand from first token.
// Phase 2: parse remaining tokens based on mode.
func parseTodoArgs(raw string) todoArgs {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return todoArgs{status: "active", sort: "priority", limit: 15}
	}

	tokens := strings.Fields(raw)
	a := todoArgs{status: "active", sort: "priority", limit: 15}

	first := strings.ToLower(tokens[0])
	rest := tokens[1:]

	switch first {
	case "new":
		a.subcommand = "new"
		parseNewArgs(&a, rest)
		return a
	case "start":
		a.subcommand = "start"
		a.ids = parseIDs(rest)
		return a
	case "drop":
		a.subcommand = "drop"
		a.ids = parseIDs(rest)
		return a
	case "reopen":
		a.subcommand = "reopen"
		a.ids = parseIDs(rest)
		return a
	case "edit":
		a.subcommand = "edit"
		parseEditArgs(&a, rest)
		return a
	case "show":
		a.subcommand = "show"
		a.ids = parseIDs(rest)
		return a
	case "get":
		a.subcommand = "get"
		parseGetArgs(&a, rest)
		return a
	case "search":
		a.subcommand = "search"
		a.text = strings.Join(rest, " ")
		return a
	case "rm":
		a.subcommand = "rm"
		a.ids = parseIDs(rest)
		return a
	case "stats":
		a.subcommand = "stats"
		parseListArgs(&a, rest)
		return a
	case "done":
		// Ambiguity rule: if all remaining tokens are integers, it's a transition.
		if len(rest) > 0 && allIntegers(rest) {
			a.subcommand = "done"
			a.ids = parseIDs(rest)
			return a
		}
		// Otherwise it's a list status filter — fall through to list parsing
		// with all tokens intact so the list parser sees "done" as a status token.
	}

	// List mode: parse all tokens for status/sort/reverse/limit/t:TAG/p:PRIORITY
	parseListArgs(&a, tokens)
	return a
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
		case "done", "closed":
			a.status = "done"
		case "all":
			a.status = ""
		case "active":
			a.status = "active"
		case "dropped":
			a.status = "dropped"
		case "in_progress":
			a.status = "in_progress"

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
			// Bare number → limit
			if n, err := strconv.Atoi(tok); err == nil && n > 0 {
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

// parseEditArgs extracts the first token as an ID, then tag/priority/text from the rest.
func parseEditArgs(a *todoArgs, tokens []string) {
	if len(tokens) == 0 {
		return
	}
	// First token must be the ID
	if id, err := strconv.ParseInt(tokens[0], 10, 64); err == nil {
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
		case "open", "done", "closed", "all", "active", "dropped", "in_progress",
			"created", "updated", "priority",
			"reverse":
			// Re-use parseListArgs logic for this single token.
			parseListArgs(a, []string{tok})
		default:
			if n, err := strconv.Atoi(tok); err == nil && n > 0 {
				a.limit = n
			} else {
				searchParts = append(searchParts, tok)
			}
		}
	}
	a.text = strings.Join(searchParts, " ")
}

// parseIDs extracts all integer tokens from the list.
func parseIDs(tokens []string) []int64 {
	var ids []int64
	for _, tok := range tokens {
		if id, err := strconv.ParseInt(tok, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// lastTag returns the last element of a tags slice, or "" if empty.
// Used by new/edit commands which set a single tag value.
func lastTag(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	return tags[len(tags)-1]
}

// allIntegers returns true if all tokens parse as integers.
func allIntegers(tokens []string) bool {
	for _, tok := range tokens {
		if _, err := strconv.ParseInt(tok, 10, 64); err != nil {
			return false
		}
	}
	return true
}

// TodoCommand returns the /todo slash command.
func TodoCommand() *Command {
	return &Command{
		Name:        "todo",
		Description: "Manage todo items",
		Category:    "observability",
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			if cc.TodoStore == nil {
				return Response{Text: "Todo store not configured."}, nil
			}
			args := parseTodoArgs(req.Args)
			agentID := cc.AgentConfig.ID
			format := resolveTodoFormat(cc)

			switch args.subcommand {
			case "new":
				return todoNewCmd(cc.TodoStore, agentID, args)
			case "done":
				return todoBatchTransition(cc.TodoStore, agentID, args.ids, "done")
			case "start":
				return todoBatchTransition(cc.TodoStore, agentID, args.ids, "in_progress")
			case "drop":
				return todoBatchTransition(cc.TodoStore, agentID, args.ids, "dropped")
			case "reopen":
				return todoBatchTransition(cc.TodoStore, agentID, args.ids, "open")
			case "edit":
				return todoEditCmd(cc.TodoStore, agentID, args)
			case "show":
				return todoShowCmd(cc.TodoStore, agentID, args)
			case "get":
				return todoGetCmd(cc.TodoStore, agentID, args, format)
			case "search":
				return todoSearchCmd(cc.TodoStore, agentID, args, format)
			case "rm":
				return todoRmCmd(cc.TodoStore, agentID, args.ids)
			case "stats":
				return todoStatsCmd(cc.TodoStore, agentID, args)
			default:
				return todoListCmd(cc.TodoStore, agentID, args, format)
			}
		},
		KeyboardOptions: func(_ context.Context, _ CommandContext) []KeyboardOption {
			return []KeyboardOption{
				{Label: "list", Data: ""},
				{Label: "new", Data: "new "},
				{Label: "stats", Data: "stats"},
				{Label: "all", Data: "all"},
				{Label: "done", Data: "done"},
			}
		},
	}
}

// resolveTodoFormat returns the effective todo list format: "table" or "lines".
// Per-agent overrides global; defaults to "lines".
func resolveTodoFormat(cc CommandContext) string {
	if f := cc.AgentConfig.TodoFormat; f != "" {
		return f
	}
	if cc.Config != nil {
		if f := cc.Config.Defaults.TodoFormat; f != "" {
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
	return Response{Text: header + "\n\n" + formatTodoList(items, format)}, nil
}

// todoNewCmd creates a new todo item.
func todoNewCmd(store *memory.TodoStore, agentID string, args todoArgs) (Response, error) {
	if args.text == "" {
		return Response{Text: "Usage: /todo new <text>"}, nil
	}
	id, err := store.Add(agentID, args.text, args.priority, lastTag(args.tags))
	if err != nil {
		return Response{}, fmt.Errorf("add todo: %w", err)
	}
	pri := args.priority
	if pri == "" {
		pri = "medium"
	}
	return Response{Text: fmt.Sprintf("Added #%d (%s): %s", id, pri, args.text)}, nil
}

// todoBatchTransition transitions one or more todos to the target state.
// Command-initiated transitions use an empty reason.
func todoBatchTransition(store *memory.TodoStore, agentID string, ids []int64, state string) (Response, error) {
	if len(ids) == 0 {
		return Response{Text: fmt.Sprintf("Usage: /todo %s <id> [id...]", state)}, nil
	}
	var results []string
	for _, id := range ids {
		if err := store.Transition(agentID, id, state, ""); err != nil {
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
	item, err := store.Edit(agentID, id, args.text, args.priority, lastTag(args.tags), args.setTag)
	if err != nil {
		return Response{}, fmt.Errorf("edit todo: %w", err)
	}
	tags := memory.FormatTags(item.Tags)
	return Response{Text: fmt.Sprintf("#%d updated: [%s]%s %s", item.ID, item.Priority, tags, item.Text)}, nil
}

// todoShowCmd shows detailed info for a single todo item.
func todoShowCmd(store *memory.TodoStore, agentID string, args todoArgs) (Response, error) {
	if len(args.ids) == 0 {
		return Response{Text: "Usage: /todo show <id>"}, nil
	}
	item, err := store.Get(agentID, args.ids[0])
	if err != nil {
		return Response{}, fmt.Errorf("get todo: %w", err)
	}
	return Response{Text: formatTodoDetail(item)}, nil
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
	return Response{Text: fmt.Sprintf("Search: %q (%d)\n\n%s", args.text, len(items), formatTodoList(items, format))}, nil
}

// todoGetCmd combines structured filters with optional full-text search.
// If a search query is present, uses Search with tag/priority/status/sort opts.
// If no search query, falls back to List (pure filter mode).
func todoGetCmd(store *memory.TodoStore, agentID string, args todoArgs, format string) (Response, error) {
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
		return Response{Text: fmt.Sprintf("Get: %q (%d)\n\n%s", args.text, len(items), formatTodoList(items, format))}, nil
	}

	// No search query — pure filter mode via List.
	return todoListCmd(store, agentID, args, format)
}

// todoRmCmd hard-deletes one or more todos.
func todoRmCmd(store *memory.TodoStore, agentID string, ids []int64) (Response, error) {
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
	for _, s := range []string{"open", "in_progress", "done", "dropped"} {
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
// Empty filter matches all; "active" matches open and in_progress;
// otherwise exact match.
func matchesStatus(itemStatus, filter string) bool {
	switch filter {
	case "":
		return true
	case "active":
		return itemStatus == "open" || itemStatus == "in_progress"
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
