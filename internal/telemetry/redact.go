package telemetry

import (
	"regexp"
	"sort"
	"strings"
)

// Redactor scrubs secret VALUES (known to the gateway from its secrets store)
// and generic credential shapes out of exported text. Values first, longest
// first, so a value that contains a shorter value is replaced whole.
type Redactor struct {
	values   []string
	patterns []*regexp.Regexp
}

// minSecretLen keeps short secrets (a numeric chat id, a one-word password)
// from redacting ordinary prose: anything shorter is not scrubbed by value.
const minSecretLen = 8

// genericPatterns are the credential shapes the backfill ETL also strips —
// kept in step with scripts/langfuse-etl/etl.py so the two halves of the
// Langfuse history agree on what never reaches the sink.
var genericPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(sk|pk)-lf-[A-Za-z0-9\-]{16,}`),                                    // Langfuse keys
	regexp.MustCompile(`\bsk-(?:ant-|proj-|or-v1-)?[A-Za-z0-9_\-]{16,}`),                     // Anthropic / OpenAI / OpenRouter
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),                                       // GitHub tokens
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),                                     // GitHub fine-grained
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9\-]{10,}`),                                    // Slack
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                                               // AWS access key id
	regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{30,}`),                                          // Google API key
	regexp.MustCompile(`\b\d{8,10}:[A-Za-z0-9_\-]{35}\b`),                                    // Telegram bot token
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`), // JWT
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9_\-\.=]{16,}`),
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
}

// NewRedactor builds a Redactor over the given secret values (empty and
// short ones are dropped) plus the generic patterns.
func NewRedactor(values []string) *Redactor {
	seen := make(map[string]bool, len(values))
	var vs []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < minSecretLen || seen[v] {
			continue
		}
		seen[v] = true
		vs = append(vs, v)
	}
	sort.Slice(vs, func(i, j int) bool { return len(vs[i]) > len(vs[j]) })
	return &Redactor{values: vs, patterns: genericPatterns}
}

// Redact returns s with every secret value and credential-shaped substring
// replaced by [REDACTED], and the number of replacements made.
func (r *Redactor) Redact(s string) (string, int) {
	if r == nil || s == "" {
		return s, 0
	}
	n := 0
	for _, v := range r.values {
		if c := strings.Count(s, v); c > 0 {
			s = strings.ReplaceAll(s, v, "[REDACTED]")
			n += c
		}
	}
	for _, p := range r.patterns {
		s = p.ReplaceAllStringFunc(s, func(string) string { n++; return "[REDACTED]" })
	}
	return s, n
}
