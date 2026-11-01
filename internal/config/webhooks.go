package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Webhook session settings (#1134). A hook's `session` value is one of these
// keywords or, otherwise, a fixed session selector resolved exactly like a
// ?session= query value.
const (
	// WebhookSessionPersistent is the default: every call of the hook revives
	// the per-hook independent session agent/i<hookID>.
	WebhookSessionPersistent = "persistent"
	// WebhookSessionNewPerCall gives every call its own fresh independent
	// session.
	WebhookSessionNewPerCall = "new-per-call"
)

// WebhookConfig is one entry of [system].webhooks (or [[agents]].system.webhooks),
// keyed by hook ID. Two TOML forms decode into it:
//
//	deploy = "deploy.md"                                        # old form: prompt only
//	deploy = { prompt = "deploy.md", session = "new-per-call" } # table form
//
// The table form may also be written as a [system.webhooks.deploy] table. An
// agent's entry for a hook ID replaces the global entry for that ID whole: the
// two are never merged field by field.
//
// Decoding never fails on a malformed entry; the problem is kept in invalid
// and reported by Validate, so the error can name the hook.
type WebhookConfig struct {
	Prompt  string // prompt file path (absolute, or a bare name searched in the prompt dirs)
	Session string // "" (= persistent), a keyword above, or a fixed session selector

	tableForm bool   // decoded from a table, so it re-encodes as one
	invalid   string // load-time problem, reported by Validate
}

// UnmarshalTOML accepts either a prompt path string or a table with the keys
// prompt and session.
func (w *WebhookConfig) UnmarshalTOML(v any) error {
	*w = WebhookConfig{}
	switch t := v.(type) {
	case string:
		w.Prompt = t
	case map[string]any:
		w.tableForm = true
		var problems []string
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s, ok := t[k].(string)
			switch {
			case k != "prompt" && k != "session":
				problems = append(problems, fmt.Sprintf("unknown setting %q (want prompt, session)", k))
			case !ok:
				problems = append(problems, fmt.Sprintf("%s must be a string, got %T", k, t[k]))
			case k == "prompt":
				w.Prompt = s
			default:
				if strings.TrimSpace(s) == "" {
					problems = append(problems, `session must not be empty (omit it, or use "persistent", "new-per-call" or a session name)`)
				}
				w.Session = s
			}
		}
		if _, ok := t["prompt"]; !ok {
			problems = append(problems, "missing prompt")
		}
		w.invalid = strings.Join(problems, "; ")
	default:
		w.invalid = fmt.Sprintf("must be a prompt path string or a table {prompt, session}, got %T", v)
	}
	if w.invalid == "" && strings.TrimSpace(w.Prompt) == "" {
		w.invalid = "prompt must not be empty"
	}
	return nil
}

// MarshalTOML re-encodes the entry in the form it was written in (used by the
// config display dump).
func (w WebhookConfig) MarshalTOML() ([]byte, error) {
	if !w.tableForm && w.Session == "" {
		return []byte(strconv.Quote(w.Prompt)), nil
	}
	s := "{ prompt = " + strconv.Quote(w.Prompt)
	if w.Session != "" {
		s += ", session = " + strconv.Quote(w.Session)
	}
	return []byte(s + " }"), nil
}

// validate reports a malformed entry. It is called from Config.Validate.
func (w WebhookConfig) validate() error {
	if w.invalid != "" {
		return fmt.Errorf("%s", w.invalid)
	}
	return nil
}

// SessionPolicy returns how the hook picks its session: newPerCall, or the
// selector to resolve. selector is "" for the persistent default (the caller
// resolves it as the hook ID, i.e. agent/i<hookID>), else a fixed selector.
func (w WebhookConfig) SessionPolicy() (selector string, newPerCall bool) {
	switch w.Session {
	case "", WebhookSessionPersistent:
		return "", false
	case WebhookSessionNewPerCall:
		return "", true
	default:
		return w.Session, false
	}
}

// validateWebhooks checks every entry of one webhooks map. where names the
// scope for the error ("[system] webhooks" or `agent "x" webhooks`).
func validateWebhooks(where string, hooks map[string]WebhookConfig) error {
	ids := make([]string, 0, len(hooks))
	for k := range hooks {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	for _, k := range ids {
		// Defense in depth: the hook ID is a URL path segment.
		if strings.ContainsAny(k, "/\\") {
			return fmt.Errorf("%s: key %q must not contain path separators", where, k)
		}
		if err := hooks[k].validate(); err != nil {
			return fmt.Errorf("%s: hook %q: %w", where, k, err)
		}
	}
	return nil
}
