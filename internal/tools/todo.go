package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"foci/internal/display"
	"foci/internal/memory"
)

// NewTodoTool creates the todo management tool.
func NewTodoTool(store *memory.TodoStore, agentID string) *Tool {
	return &Tool{
		Name:        "todo",
		ExecExport:  true,
		Positional:  []string{"action"},
		Description: "Manage a persistent todo list. Supports adding, listing, searching, getting, completing, dropping, reopening, starting, editing, and removing items. Items have priority (high/medium/low) and optional tags. Items survive restarts.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {
					"type": "string",
					"enum": ["add", "list", "search", "get", "complete", "drop", "reopen", "start", "edit", "remove"],
					"description": "The operation to perform"
				},
				"text": {
					"type": "string",
					"description": "Text for the todo item — required on add. On complete/drop it aliases --reason"
				},
				"append": {
					"type": "boolean",
					"description": "Append 'text' to the item's existing text (newline-joined) instead of replacing it. Default false. No effect without 'text'."
				},
				"priority": {
					"type": "string",
					"enum": ["high", "medium", "low"],
					"description": "Priority level (default: medium)"
				},
				"tag": {
					"type": "string",
					"description": "Comma-separated tags, or repeat --tag per tag. Sets tags on add/edit, filters on list. E.g. 'background'"
				},
				"id": {
					"type": "integer",
					"description": "Todo item ID"
				},
				"ids": {
					"type": "array",
					"items": {"type": "integer"},
					"description": "Array of todo item IDs — batch alternative to 'id'"
				},
				"status": {
					"type": "string",
					"description": "Filter by status; default is active (excludes done/dropped). Values: 'open', 'started', 'done', 'dropped', 'active', 'all'"
				},
				"query": {
					"type": "string",
					"description": "Search query — full-text with stemming; may also be given positionally"
				},
				"limit": {
					"type": "integer",
					"description": "Max results (default: 10)"
				},
				"reason": {
					"type": "string",
					"description": "Reason for closing — required, e.g. 'implemented in abc1234', 'no longer relevant'"
				},
				"sort": {
					"type": "string",
					"enum": ["priority", "created", "updated", "closed", "id", "relevance"],
					"description": "Sort order. Default 'created' when listing, 'relevance' when searching ('relevance' is search-only and is REJECTED by list). 'id' is issue order, which is not the same as 'created' once an item is edited. An unrecognised value is rejected, not silently defaulted. All sort descending (newest/highest first) unless reversed"
				},
				"reverse": {
					"type": "boolean",
					"description": "Reverse the sort order (default: false). E.g. sorting by created date reversed returns oldest first"
				}
			},
			"required": ["action"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			p, err := UnmarshalParams[struct {
				Action   string  `json:"action"`
				Text     string  `json:"text"`
				Append   bool    `json:"append"`
				Priority string  `json:"priority"`
				Tag      string  `json:"tag"`
				ID       int64   `json:"id"`
				IDs      []int64 `json:"ids"`
				Status   string  `json:"status"`
				State    string  `json:"state"`
				Query    string  `json:"query"`
				Reason   string  `json:"reason"`
				Sort     string  `json:"sort"`
				Reverse  bool    `json:"reverse"`
				Limit    int     `json:"limit"`
			}](params)
			if err != nil {
				return ToolResult{}, err
			}

			// Normalize action aliases (e.g. "create" → "add") so direct
			// tool callers reach the canonical dispatch. The shell layer
			// applies the same normalization in bash; this is defense-in-
			// depth for CC tool calls. Source: todoActionAliases in
			// execbridge.go.
			p.Action = resolveTodoAction(p.Action)

			switch p.Action {
			case "add":
				return todoAdd(store, agentID, p.Text, p.Priority, p.Tag)
			case "list":
				status := normalizeStatusFilter(p.Status)
				return todoList(store, agentID, status, p.Tag, p.Priority, p.Sort, p.Reverse, p.Limit)
			case "search":
				return todoSearch(store, agentID, p.Query, p.Status, p.Sort, p.Reverse, p.Limit)
			case "get":
				return todoGet(store, agentID, p.ID)
			case "complete":
				return todoTransition(store, agentID, p.ID, p.IDs, "done", p.Reason)
			case "drop":
				return todoTransition(store, agentID, p.ID, p.IDs, "dropped", p.Reason)
			case "reopen":
				// No reason required: reopen isn't a close action, and
				// Transition clears completed_at/close_reason for "open".
				return todoTransition(store, agentID, p.ID, p.IDs, "open", "")
			case "start":
				return todoTransition(store, agentID, p.ID, p.IDs, "started", "")
			case "transition":
				// Back-compat: undocumented action for callers that send the
				// raw transition+state pair (older API agents, format tests).
				// New callers should use 'complete' or 'drop'.
				return todoTransition(store, agentID, p.ID, p.IDs, p.State, p.Reason)
			case "edit":
				return todoEdit(store, agentID, p.ID, p.IDs, p.Text, p.Append, p.Priority, p.Tag, params)
			case "remove":
				return todoRemove(store, agentID, p.ID, p.IDs)
			default:
				return ToolResult{}, fmt.Errorf("unknown action: %s (use add, list, search, get, complete, drop, reopen, start, edit, or remove)", p.Action)
			}
		},
	}
}

// FormatTodoLine formats a single todo item as a markdown line.
func FormatTodoLine(item memory.TodoItem) string {
	return formatTodoLineOpts(item, true)
}

func formatTodoLineOpts(item memory.TodoItem, showMarker bool) string {
	pri := item.Priority
	if pri == "medium" {
		pri = "med"
	}
	// First line: metadata (ID, marker, priority, tags, age).
	var meta []string
	if showMarker {
		marker := "[ ]"
		switch item.Status {
		case "started":
			marker = "[>]"
		case "done":
			marker = "[x]"
		case "dropped":
			marker = "[-]"
		}
		meta = append(meta, fmt.Sprintf("**#%d** %s `%s`", item.ID, marker, pri))
	} else {
		meta = append(meta, fmt.Sprintf("**#%d** `%s`", item.ID, pri))
	}
	if item.Tags != "" {
		var tags []string
		for _, t := range strings.Split(item.Tags, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tags = append(tags, t)
			}
		}
		if len(tags) > 0 {
			meta = append(meta, fmt.Sprintf("`%s`", strings.Join(tags, ", ")))
		}
	}
	line := strings.Join(meta, " ")
	ts := formatTodoAge(item)
	if (item.Status == "done" || item.Status == "dropped") && item.CloseReason != "" {
		line += fmt.Sprintf(" — *%s — %s*", ts, item.CloseReason)
	} else {
		line += fmt.Sprintf(" — *%s*", ts)
	}
	// Second line: description text.
	line += "\n" + item.Text
	return line
}

// FormatTodoLines formats a slice of todo items, separated by divider lines.
// Status markers are omitted when all items share the same status.
func FormatTodoLines(items []memory.TodoItem) string {
	showMarker := !allSameStatus(items)
	lines := make([]string, len(items))
	for i, item := range items {
		lines[i] = formatTodoLineOpts(item, showMarker)
	}
	return strings.Join(lines, "\n---\n")
}

// allSameStatus returns true if all items have the same status.
func allSameStatus(items []memory.TodoItem) bool {
	if len(items) <= 1 {
		return true
	}
	s := items[0].Status
	for _, item := range items[1:] {
		if item.Status != s {
			return false
		}
	}
	return true
}

// FormatTodoTable formats a slice of todo items as a markdown table.
func FormatTodoTable(items []memory.TodoItem) string {
	// Check if any items have tags.
	hasTags := false
	for _, item := range items {
		if item.Tags != "" {
			hasTags = true
			break
		}
	}

	cols := []display.Column{
		{Header: "#", Align: display.AlignRight},
		{Header: ""},
		{Header: "Pri"},
	}
	if hasTags {
		cols = append(cols, display.Column{Header: "Tags"})
	}
	cols = append(cols, display.Column{Header: "Text"}, display.Column{Header: "Age"})

	rows := make([][]string, len(items))
	for i, item := range items {
		marker := "[ ]"
		switch item.Status {
		case "started":
			marker = "[>]"
		case "done":
			marker = "[x]"
		case "dropped":
			marker = "[-]"
		}
		text := item.Text
		if (item.Status == "done" || item.Status == "dropped") && item.CloseReason != "" {
			text += " — " + item.CloseReason
		}
		pri := item.Priority
		if pri == "medium" {
			pri = "med"
		}
		row := []string{
			fmt.Sprintf("%d", item.ID),
			marker,
			pri,
		}
		if hasTags {
			var parts []string
			for _, t := range strings.Split(item.Tags, ",") {
				t = strings.TrimSpace(t)
				if t == "" {
					continue
				}
				if len(t) > 8 {
					t = t[:8]
				}
				parts = append(parts, t)
			}
			row = append(row, strings.Join(parts, " "))
		}
		row = append(row, text, formatCompactAge(item))
		rows[i] = row
	}
	return display.MarkdownTable(cols, rows)
}

// formatCompactAge returns a compact age string for a todo item.
func formatCompactAge(item memory.TodoItem) string {
	if (item.Status == "done" || item.Status == "dropped") && item.CompletedAt != nil {
		return display.CompactRelativeTime(*item.CompletedAt)
	}
	if !item.UpdatedAt.IsZero() && !item.CreatedAt.IsZero() && !item.UpdatedAt.Equal(item.CreatedAt) {
		return display.CompactRelativeTime(item.UpdatedAt)
	}
	return display.CompactRelativeTime(item.CreatedAt)
}

func todoAdd(store *memory.TodoStore, agentID, text, priority, tag string) (ToolResult, error) {
	if text == "" {
		return ToolResult{}, fmt.Errorf("text is required for add")
	}
	id, err := store.Add(agentID, text, priority, tag)
	if err != nil {
		return ToolResult{}, fmt.Errorf("add todo: %w", err)
	}
	pri := priority
	if pri == "" {
		pri = "medium"
	}
	return TextResult(fmt.Sprintf("Added #%d (%s)", id, pri)), nil
}

// Sort orders each action accepts. Kept as explicit lists rather than derived from
// the schema enum because they DIFFER: "relevance" is meaningful only when there is
// a query to be relevant to.
//
// Validated rather than defaulted (#1773-adjacent, found via db6ee6cc): the store's
// ORDER BY switch falls through to PRIORITY for anything it does not recognise, so a
// typo — or `relevance` passed to list — quietly returned priority order. That is
// worse than a hard failure, because the caller gets a well-formed, plausibly-ordered
// list that answers a different question than the one they asked.
var (
	listSortOrders   = []string{"created", "updated", "closed", "priority", "id"}
	searchSortOrders = []string{"relevance", "created", "updated", "closed", "priority", "id"}
)

// checkSort rejects a sort value the action cannot honour. An empty value means
// "use the default" and is always allowed.
func checkSort(action, sort string, valid []string) error {
	if sort == "" {
		return nil
	}
	for _, v := range valid {
		if sort == v {
			return nil
		}
	}
	return fmt.Errorf("unknown sort %q for %s; valid: %s", sort, action, strings.Join(valid, ", "))
}

func todoList(store *memory.TodoStore, agentID, status, tag, priority, sort string, reverse bool, limit int) (ToolResult, error) {
	if err := checkSort("list", sort, listSortOrders); err != nil {
		return ToolResult{}, err
	}
	// Defaults: 10 most recently created active todos.
	if sort == "" {
		sort = "created"
	}
	if limit == 0 {
		limit = 10
	}
	var tags []string
	if tag != "" {
		tags = []string{tag}
	}
	items, err := store.List(agentID, status, tags, priority, sort, reverse, limit)
	if err != nil {
		return ToolResult{}, fmt.Errorf("list todos: %w", err)
	}
	if len(items) == 0 {
		switch status {
		case "":
			return TextResult("No todos."), nil
		case "active":
			return TextResult("No active todos."), nil
		default:
			return TextResult(fmt.Sprintf("No %s todos.", status)), nil
		}
	}
	return TextResult(FormatTodoLines(items)), nil
}

func todoSearch(store *memory.TodoStore, agentID, query, status, sort string, reverse bool, limit int) (ToolResult, error) {
	if query == "" {
		return ToolResult{}, fmt.Errorf("query is required for search")
	}
	// Normalize status filter for search (same aliases as list).
	statusFilter := normalizeStatusFilter(status)
	// normalizeStatusFilter returns "active" for empty — but for search
	// we default to no filter (search all statuses) unless explicitly set.
	if status == "" {
		statusFilter = ""
	}
	if err := checkSort("search", sort, searchSortOrders); err != nil {
		return ToolResult{}, err
	}
	if sort == "" {
		sort = "relevance"
	}
	items, err := store.Search(agentID, query, &memory.TodoSearchOpts{
		Status:  statusFilter,
		Sort:    sort,
		Reverse: reverse,
		Limit:   limit,
	})
	if err != nil {
		return ToolResult{}, fmt.Errorf("search todos: %w", err)
	}
	if len(items) == 0 {
		return TextResult(fmt.Sprintf("No todos matching %q.", query)), nil
	}
	return TextResult(FormatTodoLines(items)), nil
}

func todoGet(store *memory.TodoStore, agentID string, id int64) (ToolResult, error) {
	if id == 0 {
		return ToolResult{}, fmt.Errorf("id is required for get")
	}
	item, err := store.Get(agentID, id)
	if err != nil {
		return ToolResult{}, fmt.Errorf("get todo: %w", err)
	}
	return TextResult(FormatTodoLine(*item)), nil
}

// normalizeStatusFilter maps status filter aliases to canonical values for list.
// Returns "active" for empty/unrecognized (excludes done/dropped).
// Returns empty string for "all" (show everything).
// "closed" is deliberately not mapped — it's ambiguous (could mean done or dropped).
func normalizeStatusFilter(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "all":
		return ""
	case "active":
		return "active"
	case "open", "reopen", "reopened":
		return "open"
	case "started", "in_progress", "in-progress", "wip", "working":
		return "started"
	case "done", "complete", "completed":
		return "done"
	case "dropped", "drop", "cancelled", "canceled":
		return "dropped"
	case "":
		return "active"
	default:
		return s
	}
}

// normalizeState maps state aliases to canonical values.
// "closed" is deliberately not mapped — it's ambiguous (could mean done or dropped).
func normalizeState(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "open", "reopen", "reopened":
		return "open", nil
	case "started", "in_progress", "in-progress", "wip", "working":
		return "started", nil
	case "done", "complete", "completed":
		return "done", nil
	case "dropped", "drop", "cancelled", "canceled":
		return "dropped", nil
	case "":
		return "", fmt.Errorf("state is required for transition (use 'open', 'started', 'done', or 'dropped')")
	default:
		return "", fmt.Errorf("unknown state %q (use 'open', 'started', 'done', or 'dropped')", s)
	}
}

func todoTransition(store *memory.TodoStore, agentID string, id int64, ids []int64, state, reason string) (ToolResult, error) {
	target, err := normalizeState(state)
	if err != nil {
		return ToolResult{}, err
	}
	if (target == "done" || target == "dropped") && reason == "" {
		return ToolResult{}, fmt.Errorf("reason is required when transitioning to %s (e.g. 'implemented in abc1234', 'no longer relevant')", target)
	}
	resolved, err := resolveIDs(id, ids)
	if err != nil {
		return ToolResult{}, err
	}
	var results []string
	for _, rid := range resolved {
		if err := store.Transition(agentID, rid, target, reason); err != nil {
			results = append(results, fmt.Sprintf("#%d: error: %v", rid, err))
		} else {
			results = append(results, fmt.Sprintf("#%d: %s", rid, target))
		}
	}
	return TextResult(strings.Join(results, "\n")), nil
}

func todoEdit(store *memory.TodoStore, agentID string, id int64, ids []int64, text string, appendText bool, priority, tag string, params json.RawMessage) (ToolResult, error) {
	resolved, err := resolveIDs(id, ids)
	if err != nil {
		return ToolResult{}, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(params, &raw); err != nil {
		return ToolResult{}, fmt.Errorf("unmarshal params: %w", err)
	}
	_, setTags := raw["tag"]

	if text == "" && priority == "" && !setTags {
		return ToolResult{}, fmt.Errorf("edit requires at least one of: text, priority, tag")
	}
	if appendText && text == "" {
		return ToolResult{}, fmt.Errorf("edit: append requires text")
	}
	var results []string
	for _, rid := range resolved {
		oldItem, getErr := store.Get(agentID, rid)
		item, err := store.Edit(agentID, rid, text, priority, tag, setTags, appendText)
		if err != nil {
			results = append(results, fmt.Sprintf("#%d: error: %v", rid, err))
			continue
		}
		if getErr != nil {
			tags := memory.FormatTags(item.Tags)
			results = append(results, fmt.Sprintf("#%d: updated [%s]%s %s", item.ID, item.Priority, tags, item.Text))
			continue
		}
		var changes []string
		if text != "" && oldItem.Text != item.Text {
			changes = append(changes, describeTextChange(oldItem.Text, item.Text, appendText))
		}
		if priority != "" && oldItem.Priority != item.Priority {
			changes = append(changes, fmt.Sprintf("priority: %s → %s", oldItem.Priority, item.Priority))
		}
		if setTags && oldItem.Tags != item.Tags {
			oldTags := memory.FormatTags(oldItem.Tags)
			newTags := memory.FormatTags(item.Tags)
			if oldTags == "" {
				oldTags = "(none)"
			}
			if newTags == "" {
				newTags = "(none)"
			}
			changes = append(changes, fmt.Sprintf("tags: %s → %s", oldTags, newTags))
		}
		if len(changes) == 0 {
			results = append(results, fmt.Sprintf("#%d: no changes", rid))
		} else {
			results = append(results, fmt.Sprintf("#%d: %s", rid, strings.Join(changes, ", ")))
		}
	}
	return TextResult(strings.Join(results, "\n")), nil
}

// editSummaryTextBudget is the combined old+new length below which an edit
// summary still quotes both bodies verbatim. Small edits are the interactive
// case where seeing the exact before and after IS the confirmation; the terse
// form exists to stop a LARGE body being echoed, not to withhold information.
const editSummaryTextBudget = 200

// describeTextChange renders an edit's text change without echoing a large body
// back at the caller (#1764).
//
// The old summary printed `text: <old> → <new>` unconditionally, so appending a
// ~900-char note to a 50KB ticket returned ~100KB — enough to trip the
// oversized-tool-result guard, which persists the result to a file and leaves
// the one fact the caller wanted (did the note land?) out of the preview. The
// cost scales with the ticket's existing size, so the long-running investigation
// tickets that get annotated most are precisely the ones that cost most to
// annotate. #227 fixed this class for priority and tags and never reached text.
//
// On an APPEND the echo is doubly redundant: the new body is the old body plus
// the exact string the caller just passed in. What they cannot compute is the
// size, so that is what this reports.
func describeTextChange(oldText, newText string, appended bool) string {
	oldLen, newLen := utf8.RuneCountInString(oldText), utf8.RuneCountInString(newText)
	if appended {
		return fmt.Sprintf("text: appended %d chars (now %d)", newLen-oldLen, newLen)
	}
	if len(oldText)+len(newText) <= editSummaryTextBudget {
		return fmt.Sprintf("text: %s → %s", oldText, newText)
	}
	return fmt.Sprintf("text: replaced %d chars with %d chars", oldLen, newLen)
}

func todoRemove(store *memory.TodoStore, agentID string, id int64, ids []int64) (ToolResult, error) {
	resolved, err := resolveIDs(id, ids)
	if err != nil {
		return ToolResult{}, err
	}
	var results []string
	for _, rid := range resolved {
		if err := store.Remove(agentID, rid); err != nil {
			results = append(results, fmt.Sprintf("#%d: error: %v", rid, err))
		} else {
			results = append(results, fmt.Sprintf("#%d: removed", rid))
		}
	}
	return TextResult(strings.Join(results, "\n")), nil
}

// formatTodoAge returns a concise age string for a todo item (no parens/formatting).
func formatTodoAge(item memory.TodoItem) string {
	if (item.Status == "done" || item.Status == "dropped") && item.CompletedAt != nil {
		return item.Status + " " + display.RelativeTime(*item.CompletedAt)
	}
	if !item.UpdatedAt.IsZero() && !item.CreatedAt.IsZero() && !item.UpdatedAt.Equal(item.CreatedAt) {
		return "updated " + display.RelativeTime(item.UpdatedAt)
	}
	return display.RelativeTime(item.CreatedAt)
}

func resolveIDs(id int64, ids []int64) ([]int64, error) {
	if id != 0 && len(ids) > 0 {
		return nil, fmt.Errorf("use id or ids, not both")
	}
	if id != 0 {
		return []int64{id}, nil
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("id is required")
	}
	return ids, nil
}
