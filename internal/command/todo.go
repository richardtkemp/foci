package command

import (
	"context"
	"fmt"
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
	tag        string   // t:TAG value
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

		// t:TAG
		if strings.HasPrefix(lower, "t:") {
			a.tag = tok[2:]
			a.setTag = true
			continue
		}
		// p:PRIORITY
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
			a.tag = tok[2:]
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
			a.tag = tok[2:]
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
			case "search":
				return todoSearchCmd(cc.TodoStore, agentID, args)
			case "rm":
				return todoRmCmd(cc.TodoStore, agentID, args.ids)
			case "stats":
				return todoStatsCmd(cc.TodoStore, agentID)
			default:
				return todoListCmd(cc.TodoStore, agentID, args)
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

// todoListCmd lists todos with the given filters.
func todoListCmd(store *memory.TodoStore, agentID string, args todoArgs) (Response, error) {
	items, err := store.List(agentID, args.status, args.tag, args.priority, args.sort, args.reverse, args.limit)
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
	return Response{Text: header + "\n\n" + tools.FormatTodoLines(items)}, nil
}

// todoNewCmd creates a new todo item.
func todoNewCmd(store *memory.TodoStore, agentID string, args todoArgs) (Response, error) {
	if args.text == "" {
		return Response{Text: "Usage: /todo new <text>"}, nil
	}
	id, err := store.Add(agentID, args.text, args.priority, args.tag)
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
	item, err := store.Edit(agentID, id, args.text, args.priority, args.tag, args.setTag)
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
func todoSearchCmd(store *memory.TodoStore, agentID string, args todoArgs) (Response, error) {
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
	return Response{Text: fmt.Sprintf("Search: %q (%d)\n\n%s", args.text, len(items), tools.FormatTodoLines(items))}, nil
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
func todoStatsCmd(store *memory.TodoStore, agentID string) (Response, error) {
	items, err := store.List(agentID, "", "", "", "priority", false, 0)
	if err != nil {
		return Response{}, fmt.Errorf("list todos: %w", err)
	}
	if len(items) == 0 {
		return Response{Text: "No todos."}, nil
	}

	statusCounts := map[string]int{}
	tagCounts := map[string]int{}
	for _, item := range items {
		statusCounts[item.Status]++
		if item.Tags != "" {
			for _, t := range strings.Split(item.Tags, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					tagCounts[t]++
				}
			}
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Total: %d\n\n", len(items)))

	sb.WriteString("By status:\n")
	for _, s := range []string{"open", "in_progress", "done", "dropped"} {
		if c := statusCounts[s]; c > 0 {
			sb.WriteString(fmt.Sprintf("  %-12s %d\n", s, c))
		}
	}

	if len(tagCounts) > 0 {
		sb.WriteString("\nBy tag:\n")
		for tag, count := range tagCounts {
			sb.WriteString(fmt.Sprintf("  %-12s %d\n", tag, count))
		}
	}

	return Response{Text: strings.TrimRight(sb.String(), "\n")}, nil
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
