package command

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type TodoItem struct {
	ID       int64
	Text     string
	Status   string
	Priority string
	Tags     string
}

// NewTodoCommand returns a /todo command that lists open todo items.
// listFn returns open todos, optionally filtered by tag (empty = all).
// searchFn returns todos matching a query.
func NewTodoCommand(listFn func(tag string) ([]TodoItem, error), searchFn func(query string) ([]TodoItem, error)) *Command {
	return &Command{
		Name:        "todo",
		Description: "List active todo items",
		Category:    "observability",
		Execute: func(ctx context.Context, args string) (string, error) {
			args = strings.TrimSpace(args)

			if strings.ToLower(args) == "search" || strings.HasPrefix(strings.ToLower(args), "search ") {
				query := ""
				if len(args) > 7 {
					query = strings.TrimSpace(args[7:])
				}
				if query == "" {
					return "Usage: /todo search <query>", nil
				}
				items, err := searchFn(query)
				if err != nil {
					return "", fmt.Errorf("search todos: %w", err)
				}
				if len(items) == 0 {
					return fmt.Sprintf("No todos matching %q.", query), nil
				}
				var lines []string
				for _, item := range items {
					if item.Status != "open" && item.Status != "in_progress" {
						continue
					}
					lines = append(lines, formatTodoLine(item))
				}
				if len(lines) == 0 {
					return fmt.Sprintf("No open todos matching %q.", query), nil
				}
				return "📋 Search results\n\n" + strings.Join(lines, "\n"), nil
			}

			includeBackground := strings.ToLower(args) == "all"

			items, err := listFn("")
			if err != nil {
				return "", fmt.Errorf("list todos: %w", err)
			}

			var visible, background []TodoItem
			for _, item := range items {
				if hasBackgroundTag(item.Tags) {
					background = append(background, item)
				} else {
					visible = append(visible, item)
				}
			}

			total := len(items)
			backgroundCount := len(background)

			if !includeBackground {
				items = visible
			} else {
				items = append(visible, background...)
			}
			sortTodosByPriority(items)

			if len(items) == 0 {
				if backgroundCount > 0 {
					return fmt.Sprintf("No open todos (%d background items hidden).", backgroundCount), nil
				}
				return "No open todos.", nil
			}

			limit := 20
			showing := len(items)
			if len(items) > limit {
				items = items[:limit]
			}

			var header string
			if showing <= limit && backgroundCount == 0 {
				header = fmt.Sprintf("📋 Open todos (%d)", total)
			} else if backgroundCount > 0 && !includeBackground {
				header = fmt.Sprintf("📋 Open todos (showing %d of %d, hiding %d background items)", len(items), total, backgroundCount)
			} else if backgroundCount > 0 && includeBackground {
				header = fmt.Sprintf("📋 Open todos (showing %d of %d, including %d background)", len(items), total, backgroundCount)
			} else {
				header = fmt.Sprintf("📋 Open todos (showing %d of %d)", len(items), total)
			}

			var lines []string
			for _, item := range items {
				lines = append(lines, formatTodoLine(item))
			}

			return header + "\n\n" + strings.Join(lines, "\n"), nil
		},
	}
}

func hasBackgroundTag(tags string) bool {
	for _, t := range strings.Split(tags, ",") {
		if strings.TrimSpace(t) == "background" {
			return true
		}
	}
	return false
}

func sortTodosByPriority(items []TodoItem) {
	sort.Slice(items, func(i, j int) bool {
		pi := priorityOrder(items[i].Priority)
		pj := priorityOrder(items[j].Priority)
		if pi != pj {
			return pi < pj
		}
		return items[i].ID < items[j].ID
	})
}

func priorityOrder(p string) int {
	switch p {
	case "high":
		return 0
	case "medium":
		return 1
	case "low":
		return 2
	default:
		return 3
	}
}

func formatTodoLine(item TodoItem) string {
	emoji := "🟢"
	switch item.Priority {
	case "high":
		emoji = "🔴"
	case "medium":
		emoji = "🟡"
	}
	text := item.Text
	if len(text) > 77 {
		text = text[:74] + "..."
	}
	return fmt.Sprintf("%s #%d [%s] %s", emoji, item.ID, item.Priority, text)
}
