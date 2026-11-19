package command

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"foci/internal/config"
	"foci/internal/configapprove"
	"foci/internal/provision"
)

// /agents new with arguments (#3122): create an agent in one command, with
// the answers the wizard would ask for given as flags:
//
//	/agents new <name> [--backend <b>] [--model <m>] [--backend-config '<inline table>'] [--chars defaults|openclaw|blank] [--copy <agent-id>] [--wait <duration>]
//
// --backend-config adds keys to the agent's backend_config (e.g. the Claude
// Code account: '{accounts = ["/home/x/.claude-stuart"]}').
// They are in the [[agents]] block the config check judges, the approval
// prompt shows, and the live start reads.
//
// It runs the wizard's own checks (name, backend, character files and the
// next start's config check) and the same creation (createAgent: workspace,
// character files, [[agents]] block, crontab, live start). Typed in chat or
// the app, it creates the agent at once, as the wizard's last answer does.
// Arriving over the HTTP API (`foci command`, an agent's shell), it never
// runs directly: it asks the owner to approve it with an Approve/Deny prompt
// (configapprove.SubmitAction), as `foci config set` does, and replies with
// the request id to follow with `foci config wait`. Bare /agents new still
// starts the interactive wizard (chat only).

// agentNewArgs is one parsed non-interactive /agents new.
type agentNewArgs struct {
	Name     string
	Backend  string // "" = the wizard's default backend
	Model    string // "" = the wizard's default (sonnet); unused for api
	Chars    string // "" = defaults
	CopyFrom string // --copy: copy another agent's character files
	// BackendConfig is --backend-config's TOML inline table text; checked
	// by parseAgentNew (decodeBackendConfig), "" = none.
	BackendConfig string
	Wait          time.Duration
}

// agentNewUsage is the reply to a malformed /agents new.
const agentNewUsage = "Usage: /agents new <name> [--backend <backend>] [--model <model>] [--backend-config '<TOML inline table>'] [--chars defaults|openclaw|blank] [--copy <agent-id>] [--wait <duration>]\n" +
	"Creates the agent in one step (bare /agents new starts the wizard instead). " +
	"Defaults: the default backend, model sonnet, character files from defaults. " +
	"--backend-config adds backend_config keys, e.g. '{accounts = [\"/path\"]}'. " +
	"Over the HTTP API (foci command) it asks the user to approve first; --wait is how long they have (default 10m)."

// parseAgentNew parses the arguments after "new". Words that are not flags
// form the name, so a name may hold spaces ("Greek Tutor").
func parseAgentNew(args string) (agentNewArgs, error) {
	var a agentNewArgs
	var name []string
	f, err := splitAgentArgs(args)
	if err != nil {
		return a, err
	}
	for i := 0; i < len(f); i++ {
		flag, val, attached := strings.Cut(f[i], "=")
		if !strings.HasPrefix(flag, "--") {
			name = append(name, f[i])
			continue
		}
		if !attached {
			if i+1 >= len(f) {
				return a, fmt.Errorf("%s needs a value", flag)
			}
			i++
			val = f[i]
		}
		switch flag {
		case "--backend":
			a.Backend = val
		case "--model":
			a.Model = val
		case "--chars":
			a.Chars = val
		case "--copy":
			a.CopyFrom = val
		case "--backend-config":
			if _, err := decodeBackendConfig(val); err != nil {
				return a, err
			}
			a.BackendConfig = val
		case "--wait":
			d, err := time.ParseDuration(val)
			if err != nil || d <= 0 {
				return a, fmt.Errorf("--wait wants a duration such as 30m (got %q)", val)
			}
			a.Wait = d
		default:
			return a, fmt.Errorf("unknown flag %s", flag)
		}
	}
	a.Name = strings.Join(name, " ")
	if a.Name == "" {
		return a, fmt.Errorf("a name is required")
	}
	if a.CopyFrom != "" {
		if a.Chars != "" && !strings.EqualFold(a.Chars, "copy") {
			return a, fmt.Errorf("--copy and --chars %s both choose the character files: give one", a.Chars)
		}
		a.Chars = "copy " + a.CopyFrom
	}
	return a, nil
}

// decodeBackendConfig decodes --backend-config: a non-empty TOML inline
// table without "model" (that is --model's).
func decodeBackendConfig(text string) (map[string]any, error) {
	v, err := config.DecodeTOMLValue(text)
	if err != nil {
		return nil, fmt.Errorf("--backend-config: %w", err)
	}
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil, fmt.Errorf("--backend-config wants a non-empty TOML inline table such as '{env = {KEY = \"value\"}}' (got %s)", text)
	}
	if _, ok := m["model"]; ok {
		return nil, fmt.Errorf("--backend-config: give the model with --model, not as backend_config.model")
	}
	return m, nil
}

// splitAgentArgs splits /agents new's arguments on whitespace, keeping a
// quoted run ('…' or "…") or a bracketed one ({…}, […]) in one word, so an
// inline table with spaces stays one value. A word that is wholly one
// quoted run loses its quotes ('{a = 1}' → {a = 1}; "Greek Tutor" → Greek
// Tutor).
func splitAgentArgs(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord, depth := false, 0
	var quote rune
	flush := func() {
		if inWord {
			words = append(words, unquoteWord(cur.String()))
		}
		cur.Reset()
		inWord = false
	}
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			cur.WriteRune(r)
			if r == '\\' && quote == '"' && i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
			} else if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
			cur.WriteRune(r)
		case r == '{' || r == '[':
			depth++
			inWord = true
			cur.WriteRune(r)
		case r == '}' || r == ']':
			depth--
			cur.WriteRune(r)
		case depth == 0 && (r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			flush()
		default:
			inWord = true
			cur.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unclosed %c quote", quote)
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced brackets")
	}
	flush()
	return words, nil
}

// unquoteWord strips the quotes of a word that is wholly one '…' or "…" run.
func unquoteWord(w string) string {
	if len(w) < 2 {
		return w
	}
	q := w[0]
	if (q != '\'' && q != '"') || w[len(w)-1] != q || strings.ContainsRune(w[1:len(w)-1], rune(q)) {
		return w
	}
	return w[1 : len(w)-1]
}

// planAgent runs every check the wizard runs on these answers and returns
// a wizard holding them, ready for createFn. Nothing is written.
func planAgent(deps AgentNewDeps, a agentNewArgs) (*agentWizard, error) {
	w := newAgentWizard(deps)
	id, err := w.checkName(strings.TrimSpace(a.Name))
	if err != nil {
		return nil, err
	}
	w.setName(strings.TrimSpace(a.Name), id)
	if w.backend, err = w.pickBackend(a.Backend); err != nil {
		return nil, err
	}
	if w.backend != apiBackend {
		w.setModel(a.Model)
	} else if a.Model != "" {
		return nil, fmt.Errorf("an `api` agent has no model of its own (it resolves via [groups]/[models]): drop --model")
	}
	if a.BackendConfig != "" {
		if w.backend == apiBackend {
			return nil, fmt.Errorf("an `api` agent has no backend_config: drop --backend-config")
		}
		if w.backendConfig, err = decodeBackendConfig(a.BackendConfig); err != nil {
			return nil, err
		}
	}
	if w.charMode, w.copyFrom, err = w.pickCharMode(a.Chars); err != nil {
		return nil, err
	}
	if err := w.configProblem(); err != nil {
		return nil, fmt.Errorf("with this agent, foci.toml would fail to load at the next start: %w", err)
	}
	w.step = stepCharMode
	return w, nil
}

// charModeText says where the character files come from.
func (w *agentWizard) charModeText() string {
	if w.charMode == "copy" {
		return "copied from " + w.copyFrom
	}
	return w.charMode
}

// planDetail is the approval prompt's body: what Approve creates.
func (w *agentWizard) planDetail() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Name: %s (id %s)\n", w.display, w.id)
	if w.backend == apiBackend {
		sb.WriteString("Backend: api (in-process; model via [groups]/[models])\n")
	} else {
		fmt.Fprintf(&sb, "Backend: %s (model: %s)\n", w.backend, w.modelRaw)
	}
	fmt.Fprintf(&sb, "Character files: %s\n", w.charModeText())
	fmt.Fprintf(&sb, "Workspace: %s\n", filepath.Join(w.deps.HomeDir, w.id))
	fmt.Fprintf(&sb, "\nAppended to %s:\n%s\n", w.deps.ConfigPath, configapprove.Fenced(strings.TrimSpace(provision.GenerateAgentBlock(w.spec())), "toml"))
	sb.WriteString("\nThen its crontab entries are added and the agent starts.")
	if w.preflight != "" {
		sb.WriteString("\n" + w.preflight)
	}
	return sb.String()
}

// agentsNewDirect handles "/agents new <args>" (args: the text after "new").
func agentsNewDirect(req Request, deps *AgentNewDeps, args string) Response {
	a, err := parseAgentNew(args)
	if err != nil {
		return Response{Text: err.Error() + "\n\n" + agentNewUsage}
	}
	w, err := planAgent(*deps, a)
	if err != nil {
		return Response{Text: "Agent not created, nothing written: " + err.Error()}
	}
	if !fromHTTP(req) {
		result, err := w.createFn(w)
		if err != nil {
			return Response{Text: "Creation failed: " + err.Error()}
		}
		return Response{Text: strings.TrimPrefix(w.preflight, "\n") + prefixNL(w.preflight) + result}
	}
	if deps.RequestApproval == nil {
		return Response{Text: "Agent not created: approval of agent creation over the HTTP API is not available."}
	}
	title := "new agent " + w.id
	d := *deps
	st := deps.RequestApproval(configapprove.ActionSubmission{
		SessionKey: req.SessionKey,
		Title:      title,
		Detail:     w.planDetail(),
		Timeout:    a.Wait,
		Run: func() (string, error) {
			// Re-check at approval: the config or the agent list may have
			// changed while the prompt waited (another agent by this name).
			w2, err := planAgent(d, a)
			if err != nil {
				return "", fmt.Errorf("nothing was written: %w", err)
			}
			return w2.createFn(w2)
		},
	})
	if st.Outcome == configapprove.Failed {
		return Response{Text: "Agent not created, nothing written: " + st.Reason}
	}
	return Response{Text: fmt.Sprintf("New agent %s sent to the user for approval (request %s). Nothing is created unless they approve. Follow it with: foci config wait %s", w.id, st.ID, st.ID)}
}

// prefixNL is a blank line after non-empty preflight warnings.
func prefixNL(preflight string) string {
	if preflight == "" {
		return ""
	}
	return "\n\n"
}
