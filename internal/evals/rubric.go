// Package evals holds the rubric registry: the axes a trace can be scored on,
// read from files so a new axis is a file drop, not a release.
//
// A rubric is a markdown file with YAML front matter; the body is the judge
// prompt (for kind: judge). Three kinds:
//
//   - human   — graded by a person (/score, the app's score control, the
//     Langfuse annotation UI). The registry declares the axis's shape so the
//     UI can render it and the gateway can validate a value.
//   - derive  — computed from the trace's own metadata by an expression, no
//     model call (silent, errored, cost_divergence, ...).
//   - judge   — produced by an LLM reading the trace against the body prompt.
//
// Phase 2 (this package) loads, validates, watches and exposes rubrics and
// mirrors each as a Langfuse score config. Running derive/judge rubrics is
// later work; their fields are validated here so a file is never accepted
// that a runner would then reject.
package evals

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind is who produces a rubric's scores.
type Kind string

const (
	KindHuman  Kind = "human"
	KindDerive Kind = "derive"
	KindJudge  Kind = "judge"
)

// Type is the score's value shape (Langfuse data types, lower-cased).
type Type string

const (
	TypeNumeric     Type = "numeric"
	TypeBoolean     Type = "boolean"
	TypeCategorical Type = "categorical"
)

// Category is one label/value of a categorical rubric.
type Category struct {
	Label string  `yaml:"label"`
	Value float64 `yaml:"value"`
}

// Select restricts which traces a rubric applies to. Empty fields match
// everything. Human rubrics may use agents/session_types only — an input
// regex would make the app's score control change per message, so it is
// rejected for them at load.
type Select struct {
	Agents       []string `yaml:"agents"`
	Triggers     []string `yaml:"triggers"`
	SessionTypes []string `yaml:"session_types"`
	Backends     []string `yaml:"backends"`
	InputRegex   string   `yaml:"input_regex"`
	Tags         []string `yaml:"tags"`

	inputRe *regexp.Regexp
}

// Judge configures an LLM-judged rubric. The prompt is the file body.
type Judge struct {
	Model     string  `yaml:"model"`
	BudgetUSD float64 `yaml:"budget_usd_per_day"`
	Sample    float64 `yaml:"sample"` // 0 or 1 = every matching trace; 0.1 = 10 %
}

// Derive configures a metadata-derived rubric: an expression over the trace
// evaluated by the runner (phase 4). Validated for presence only here.
type Derive struct {
	Expr string `yaml:"expr"`
}

// Rubric is one scoring axis.
type Rubric struct {
	Name        string     `yaml:"name"`
	Version     int        `yaml:"version"`
	Kind        Kind       `yaml:"kind"`
	Type        Type       `yaml:"type"`
	Description string     `yaml:"description"`
	Min         *float64   `yaml:"min"`
	Max         *float64   `yaml:"max"`
	Categories  []Category `yaml:"categories"`
	Select      Select     `yaml:"select"`
	Judge       Judge      `yaml:"judge"`
	Derive      Derive     `yaml:"derive"`

	// Body is the markdown after the front matter — the judge prompt.
	Body string `yaml:"-"`
	// Path is the file the rubric came from.
	Path string `yaml:"-"`
	// ConfigID is the Langfuse score config id once mirrored (see Registry).
	ConfigID string `yaml:"-"`
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Parse reads one rubric file's contents. name is the file stem, which the
// front-matter name must equal — one file, one axis, no aliasing.
func Parse(path, stem string, content []byte) (*Rubric, error) {
	fm, body, err := splitFrontMatter(content)
	if err != nil {
		return nil, err
	}
	var r Rubric
	dec := yaml.NewDecoder(bytes.NewReader(fm))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("front matter: %w", err)
	}
	r.Body = strings.TrimSpace(string(body))
	r.Path = path
	if r.Name == "" {
		r.Name = stem
	}
	if err := r.validate(stem); err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *Rubric) validate(stem string) error {
	if r.Name != stem {
		return fmt.Errorf("name %q must equal the file stem %q", r.Name, stem)
	}
	if !nameRe.MatchString(r.Name) {
		return fmt.Errorf("name %q: lower-case letters, digits and _ only", r.Name)
	}
	if r.Version <= 0 {
		r.Version = 1
	}
	switch r.Kind {
	case KindHuman, KindDerive, KindJudge:
	case "":
		return fmt.Errorf("kind is required (human|derive|judge)")
	default:
		return fmt.Errorf("kind %q: want human, derive or judge", r.Kind)
	}
	switch r.Type {
	case TypeNumeric:
		if r.Min == nil || r.Max == nil {
			return fmt.Errorf("numeric rubric needs min and max")
		}
		if *r.Min >= *r.Max {
			return fmt.Errorf("min %v must be below max %v", *r.Min, *r.Max)
		}
	case TypeBoolean:
		if r.Min != nil || r.Max != nil || len(r.Categories) > 0 {
			return fmt.Errorf("boolean rubric takes no min/max/categories")
		}
	case TypeCategorical:
		if len(r.Categories) < 2 {
			return fmt.Errorf("categorical rubric needs at least two categories")
		}
		seen := map[string]bool{}
		for _, c := range r.Categories {
			if c.Label == "" {
				return fmt.Errorf("category with empty label")
			}
			if seen[c.Label] {
				return fmt.Errorf("duplicate category %q", c.Label)
			}
			seen[c.Label] = true
		}
	case "":
		return fmt.Errorf("type is required (numeric|boolean|categorical)")
	default:
		return fmt.Errorf("type %q: want numeric, boolean or categorical", r.Type)
	}
	switch r.Kind {
	case KindJudge:
		if r.Body == "" {
			return fmt.Errorf("judge rubric needs a prompt body after the front matter")
		}
		if r.Judge.Model == "" {
			return fmt.Errorf("judge rubric needs judge.model")
		}
		if r.Judge.Sample < 0 || r.Judge.Sample > 1 {
			return fmt.Errorf("judge.sample must be within 0..1")
		}
	case KindDerive:
		if strings.TrimSpace(r.Derive.Expr) == "" {
			return fmt.Errorf("derive rubric needs derive.expr")
		}
	case KindHuman:
		if r.Select.InputRegex != "" {
			return fmt.Errorf("human rubric cannot select by input_regex (the score control must be stable per agent)")
		}
	}
	if r.Select.InputRegex != "" {
		re, err := regexp.Compile(r.Select.InputRegex)
		if err != nil {
			return fmt.Errorf("select.input_regex: %w", err)
		}
		r.Select.inputRe = re
	}
	return nil
}

// Validate checks a proposed score value against the rubric's shape,
// returning the canonical numeric value and, for categoricals, the label.
func (r *Rubric) Validate(raw string) (value float64, label string, err error) {
	raw = strings.TrimSpace(raw)
	switch r.Type {
	case TypeBoolean:
		switch strings.ToLower(raw) {
		case "1", "true", "yes", "y":
			return 1, "", nil
		case "0", "false", "no", "n":
			return 0, "", nil
		}
		return 0, "", fmt.Errorf("%s is boolean: want 1/0, true/false, yes/no", r.Name)
	case TypeNumeric:
		var v float64
		if _, err := fmt.Sscanf(raw, "%g", &v); err != nil {
			return 0, "", fmt.Errorf("%s is numeric: %q is not a number", r.Name, raw)
		}
		if v < *r.Min || v > *r.Max {
			return 0, "", fmt.Errorf("%s: %g is outside %g..%g", r.Name, v, *r.Min, *r.Max)
		}
		return v, "", nil
	case TypeCategorical:
		for _, c := range r.Categories {
			if strings.EqualFold(c.Label, raw) {
				return c.Value, c.Label, nil
			}
		}
		return 0, "", fmt.Errorf("%s: %q is not one of %s", r.Name, raw, strings.Join(r.labels(), ", "))
	}
	return 0, "", fmt.Errorf("rubric %s has no type", r.Name)
}

func (r *Rubric) labels() []string {
	out := make([]string, len(r.Categories))
	for i, c := range r.Categories {
		out[i] = c.Label
	}
	return out
}

// Matches reports whether a trace with these facets is in the rubric's
// selection. input is only consulted when input_regex is set.
func (s *Select) Matches(agent, trigger, sessionType, backend string, tags []string, input string) bool {
	if !in(s.Agents, agent) || !in(s.Triggers, trigger) || !in(s.SessionTypes, sessionType) || !in(s.Backends, backend) {
		return false
	}
	for _, want := range s.Tags {
		if !contains(tags, want) {
			return false
		}
	}
	if s.inputRe != nil && !s.inputRe.MatchString(input) {
		return false
	}
	return true
}

func in(set []string, v string) bool {
	if len(set) == 0 {
		return true
	}
	return contains(set, v)
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// Shape renders the value shape for listings: "1..5", "yes/no", "a|b|c".
func (r *Rubric) Shape() string {
	switch r.Type {
	case TypeNumeric:
		return fmt.Sprintf("%g..%g", *r.Min, *r.Max)
	case TypeBoolean:
		return "yes/no"
	default:
		return strings.Join(r.labels(), "|")
	}
}

// splitFrontMatter separates the YAML block between leading --- markers from
// the markdown body.
func splitFrontMatter(content []byte) (fm, body []byte, err error) {
	s := string(content)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return nil, nil, fmt.Errorf("no front matter (file must start with ---)")
	}
	rest := s[strings.Index(s, "\n")+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, nil, fmt.Errorf("front matter not closed with ---")
	}
	fm = []byte(rest[:end])
	after := rest[end+4:]
	if i := strings.Index(after, "\n"); i >= 0 {
		after = after[i+1:]
	} else {
		after = ""
	}
	return fm, []byte(after), nil
}

// sortRubrics orders by name for stable listings.
func sortRubrics(rs []*Rubric) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
}
