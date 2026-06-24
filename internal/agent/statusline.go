package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"foci/internal/display"
	"foci/internal/procx"
	"foci/internal/timeutil"
)

// The statusline is the configurable template that builds the per-message
// header an agent receives (the historical [meta] / [state] lines). It replaces
// the old hardcoded buildMetaPrefix / collectStateDashboard generators (#831).
//
// A template is a string with two interpolation forms:
//
//	{field}      — a built-in field from statuslineFields (below). Unknown names
//	               are left verbatim so typos are visible.
//	${command}   — run `sh -c command` and embed its stdout. Runs on EVERY turn,
//	               so it is bounded by a tight timeout + output cap and never
//	               blocks the turn: any failure embeds nothing.
//
// Rendering rules (these reproduce the old conditional behaviour without
// hardcoding it):
//  1. Conditional fields ({cost}, {tokens}, {mana}, {state}, {todos}, …) carry
//     their own label and self-render to "" when their value is absent.
//  2. Runs of spaces are collapsed to one and each line is trimmed, so omitted
//     fields don't leave gaps.
//  3. A line that contains at least one placeholder, all of which rendered
//     empty, is dropped entirely — that is what makes the "[state]" line vanish
//     when every store is empty.
//
// A line with no placeholders (pure literal) is always kept.

// DefaultStatuslineTemplate reproduces the historical two-line header. Used when
// an agent has no `statusline` configured. The labels live here (editable);
// the conditional fields self-omit, rule 2 cleans the spacing, and rule 3 drops
// the "[state]" line when empty.
const DefaultStatuslineTemplate = "[meta] time={time} gap={gap} model={model} via={via} {cost} {tokens} {mana}\n[state] {state}\n[ask] {ask}"

// Statusline command execution bounds. The command runs synchronously before
// every turn, so these are deliberately tight.
const (
	statuslineCmdTimeout  = 3 * time.Second
	statuslineCmdWaitKill = 2 * time.Second // force pipes closed if a child outlives the kill
	statuslineCmdMaxBytes = 4096
)

// statuslineInputs carries everything the field providers need for one render.
type statuslineInputs struct {
	now        time.Time
	model      string
	platform   string
	manaStr    string
	manaGood   bool
	sm         *sessionMeta
	agent      *Agent
	sessionKey string
}

// statuslineFirstMsg reports whether this is the first message of a session
// (no prior-turn cost/token data). The old buildMetaPrefix gated prev_cost AND
// prev_tokens on this single AND-condition; both fields key off it here so the
// behaviour is identical.
func statuslineFirstMsg(sm *sessionMeta) bool {
	return sm.prevCost == 0 && sm.prevInput == 0
}

// statuslineFields maps placeholder names to value providers. Composite fields
// (cost/tokens/mana/state, and the per-store todos/tasks/scratchpad) carry their
// own label and self-omit; the *_raw / *_pct / token-count fields are bare
// values that always render.
var statuslineFields = map[string]func(statuslineInputs) string{
	// Always-present meta fields (bare values; labels live in the template).
	"time":  func(in statuslineInputs) string { return timeutil.Format(in.now) },
	"model": func(in statuslineInputs) string { return in.model },
	"via":   func(in statuslineInputs) string { return in.platform },
	"gap": func(in statuslineInputs) string {
		if in.sm.lastMessageTime.IsZero() {
			return "none"
		}
		return display.FormatDuration(in.now.Sub(in.sm.lastMessageTime))
	},

	// Conditional composite fields (label baked in; empty on absence).
	"cost": func(in statuslineInputs) string {
		if statuslineFirstMsg(in.sm) {
			return ""
		}
		return "prev_cost=" + formatCost(in.sm.prevCost)
	},
	"tokens": func(in statuslineInputs) string {
		if statuslineFirstMsg(in.sm) {
			return ""
		}
		return fmt.Sprintf("prev_tokens=in:%d/out:%d/cR:%d/cW:%d",
			in.sm.prevInput, in.sm.prevOutput, in.sm.prevCacheRead, in.sm.prevCacheWrite)
	},
	"mana": func(in statuslineInputs) string {
		if in.manaStr == "" {
			return ""
		}
		return "mana=" + in.manaStr + " " + manaIndicator(in.manaGood)
	},
	"state": func(in statuslineInputs) string { return in.agent.stateDashboardBody(in.sessionKey) },
	// Paused-ask reminder (addressed to the agent, who reads the statusline):
	// renders only while an ask is paused, so rule 3 drops its template line
	// otherwise. Names the ask id so the agent knows what's still waiting.
	"ask": func(in statuslineInputs) string {
		if in.agent == nil || in.agent.AskRouter == nil || in.agent.AskRouter.IsPaused == nil {
			return ""
		}
		if !in.agent.AskRouter.IsPaused(in.sessionKey) {
			return ""
		}
		reqID := ""
		if in.agent.AskRouter.PendingForSession != nil {
			reqID = in.agent.AskRouter.PendingForSession(in.sessionKey)
		}
		return fmt.Sprintf("⏸ ask %s paused — user replies routing to you as normal turns, not answering it (/resume to restore)", reqID)
	},

	// Granular per-store fields (label baked in; self-omit individually).
	"todos":      func(in statuslineInputs) string { return in.agent.statusTodos(in.sessionKey) },
	"tasks":      func(in statuslineInputs) string { return in.agent.statusTasks(in.sessionKey) },
	"scratchpad": func(in statuslineInputs) string { return in.agent.statusScratchpad(in.sessionKey) },

	// Bare/raw fields (always render; no label, no self-omission).
	"cost_raw":    func(in statuslineInputs) string { return formatCost(in.sm.prevCost) },
	"tokens_in":   func(in statuslineInputs) string { return strconv.Itoa(in.sm.prevInput) },
	"tokens_out":  func(in statuslineInputs) string { return strconv.Itoa(in.sm.prevOutput) },
	"cache_read":  func(in statuslineInputs) string { return strconv.Itoa(in.sm.prevCacheRead) },
	"cache_write": func(in statuslineInputs) string { return strconv.Itoa(in.sm.prevCacheWrite) },
	"mana_pct":    func(in statuslineInputs) string { return in.manaStr },
	"mana_flag": func(in statuslineInputs) string {
		if in.manaStr == "" {
			return ""
		}
		return manaIndicator(in.manaGood)
	},
}

func manaIndicator(good bool) string {
	if good {
		return "🟢"
	}
	return "🔴"
}

// warnedStatuslineFields tracks unknown field names already logged, so an
// unknown placeholder in a static template warns once per process, not per turn.
var warnedStatuslineFields sync.Map

func (a *Agent) warnUnknownStatuslineField(name string) {
	if _, loaded := warnedStatuslineFields.LoadOrStore(name, true); !loaded {
		a.logger().Warnf("statusline: unknown field {%s} — left verbatim; check the statusline config", name)
	}
}

// renderStatusline renders a template against the turn inputs. See the package
// doc comment above for the syntax and the three rendering rules.
func (a *Agent) renderStatusline(ctx context.Context, tmpl string, in statuslineInputs) string {
	lines := strings.Split(tmpl, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		rendered, placeholders, nonEmpty := a.renderStatuslineLine(ctx, line, in)
		if placeholders > 0 && nonEmpty == 0 {
			continue // rule 3: every placeholder on this line rendered empty
		}
		out = append(out, collapseStatuslineSpaces(rendered)) // rule 2
	}
	return strings.Join(out, "\n")
}

// renderStatuslineLine renders one template line, returning the rendered text,
// the number of placeholders it contained, and how many rendered non-empty.
func (a *Agent) renderStatuslineLine(ctx context.Context, line string, in statuslineInputs) (string, int, int) {
	var b strings.Builder
	b.Grow(len(line))
	placeholders, nonEmpty := 0, 0

	for i := 0; i < len(line); {
		// ${command} — executable embedding.
		if line[i] == '$' && i+1 < len(line) && line[i+1] == '{' {
			rel := strings.IndexByte(line[i+2:], '}')
			if rel < 0 {
				b.WriteByte(line[i])
				i++
				continue
			}
			cmd := line[i+2 : i+2+rel]
			val := a.runStatuslineCommand(ctx, cmd)
			placeholders++
			if val != "" {
				nonEmpty++
			}
			b.WriteString(val)
			i = i + 2 + rel + 1
			continue
		}

		// {field} — built-in field.
		if line[i] == '{' {
			rel := strings.IndexByte(line[i+1:], '}')
			if rel < 0 {
				b.WriteByte(line[i])
				i++
				continue
			}
			name := line[i+1 : i+1+rel]
			if fn, ok := statuslineFields[name]; ok {
				val := fn(in)
				placeholders++
				if val != "" {
					nonEmpty++
				}
				b.WriteString(val)
			} else {
				a.warnUnknownStatuslineField(name)
				b.WriteString(line[i : i+1+rel+1]) // leave "{name}" verbatim
			}
			i = i + 1 + rel + 1
			continue
		}

		b.WriteByte(line[i])
		i++
	}
	return b.String(), placeholders, nonEmpty
}

// runStatuslineCommand runs `sh -c command` with a tight timeout and output cap.
// Any failure (non-zero exit, timeout, not-found) returns "" — the statusline
// degrades silently and never blocks the turn. Stdout newlines are flattened to
// spaces so a multi-line script can't break the single-line statusline.
func (a *Agent) runStatuslineCommand(ctx context.Context, command string) string {
	if strings.TrimSpace(command) == "" {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, statuslineCmdTimeout)
	defer cancel()

	// procx.Spawn strips the foci-secrets group from the child and puts it in
	// its own process group.
	cmd := procx.Spawn(cctx, "sh", "-c", command)
	// WaitDelay forces the pipes closed shortly after the context is cancelled,
	// so a grandchild that inherits stdout can't hang the turn (the backstop for
	// the case a per-turn timeout alone can't cover).
	cmd.WaitDelay = statuslineCmdWaitKill

	out, err := cmd.Output()
	if err != nil {
		a.logger().Debugf("statusline command %q failed: %v", command, err)
		return ""
	}
	s := strings.TrimRight(string(out), "\n")
	if len(s) > statuslineCmdMaxBytes {
		s = s[:statuslineCmdMaxBytes]
		// Don't split a multibyte rune at the cut.
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return strings.ReplaceAll(s, "\n", " ")
}

// collapseStatuslineSpaces collapses runs of spaces to a single space and trims
// the line (rule 2). It only touches ' ' so emoji and other content are intact.
func collapseStatuslineSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if r == ' ' {
			if prevSpace {
				continue
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
