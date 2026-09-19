package evals

import (
	"regexp"
	"strings"
	"testing"
)

func ptrF(v float64) *float64 { return &v }

// TestParse_Success covers one valid file per kind/type, plus the
// name-defaults-to-stem behaviour.
func TestParse_Success(t *testing.T) {
	cases := []struct {
		name    string
		stem    string
		content string
		check   func(t *testing.T, r *Rubric)
	}{
		{
			name: "numeric human, name defaults to stem",
			stem: "quality",
			content: "---\n" +
				"kind: human\n" +
				"type: numeric\n" +
				"min: 1\n" +
				"max: 5\n" +
				"---\n" +
				"Rate overall quality.\n",
			check: func(t *testing.T, r *Rubric) {
				if r.Name != "quality" {
					t.Errorf("Name = %q, want %q (defaulted from stem)", r.Name, "quality")
				}
				if r.Min == nil || *r.Min != 1 || r.Max == nil || *r.Max != 5 {
					t.Errorf("Min/Max = %v/%v, want 1/5", r.Min, r.Max)
				}
				if r.Body != "Rate overall quality." {
					t.Errorf("Body = %q, want trimmed markdown", r.Body)
				}
			},
		},
		{
			name: "boolean human",
			stem: "helpful",
			content: "---\n" +
				"kind: human\n" +
				"type: boolean\n" +
				"---\n",
			check: func(t *testing.T, r *Rubric) {
				if r.Type != TypeBoolean || r.Kind != KindHuman {
					t.Errorf("Kind/Type = %v/%v, want human/boolean", r.Kind, r.Type)
				}
			},
		},
		{
			name: "categorical human",
			stem: "tone",
			content: "---\n" +
				"kind: human\n" +
				"type: categorical\n" +
				"categories:\n" +
				"  - label: good\n" +
				"    value: 1\n" +
				"  - label: bad\n" +
				"    value: 0\n" +
				"---\n",
			check: func(t *testing.T, r *Rubric) {
				if len(r.Categories) != 2 {
					t.Fatalf("Categories = %v, want 2", r.Categories)
				}
			},
		},
		{
			name: "judge needs body and judge.model",
			stem: "honesty",
			content: "---\n" +
				"kind: judge\n" +
				"type: boolean\n" +
				"judge:\n" +
				"  model: gpt-4o\n" +
				"---\n" +
				"Is the reply honest and non-evasive?\n",
			check: func(t *testing.T, r *Rubric) {
				if r.Judge.Model != "gpt-4o" {
					t.Errorf("Judge.Model = %q, want gpt-4o", r.Judge.Model)
				}
				if r.Body != "Is the reply honest and non-evasive?" {
					t.Errorf("Body = %q", r.Body)
				}
			},
		},
		{
			name: "derive needs derive.expr",
			stem: "cost_divergence",
			content: "---\n" +
				"kind: derive\n" +
				"type: numeric\n" +
				"min: 0\n" +
				"max: 1\n" +
				"derive:\n" +
				"  expr: \"cost_usd > 0\"\n" +
				"---\n",
			check: func(t *testing.T, r *Rubric) {
				if r.Derive.Expr != "cost_usd > 0" {
					t.Errorf("Derive.Expr = %q", r.Derive.Expr)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Parse(tc.stem+".md", tc.stem, []byte(tc.content))
			if err != nil {
				t.Fatalf("Parse: unexpected error: %v", err)
			}
			tc.check(t, r)
		})
	}
}

// TestParse_Errors walks every validation error path Parse can return.
func TestParse_Errors(t *testing.T) {
	cases := []struct {
		name    string
		stem    string
		content string
		wantErr string
	}{
		{
			name: "unknown front matter key rejected",
			stem: "x",
			content: "---\n" +
				"kind: human\n" +
				"type: boolean\n" +
				"bogus: true\n" +
				"---\n",
			wantErr: "front matter",
		},
		{
			name: "name must equal stem",
			stem: "quality",
			content: "---\n" +
				"name: other\n" +
				"kind: human\n" +
				"type: boolean\n" +
				"---\n",
			wantErr: "must equal the file stem",
		},
		{
			name: "bad name characters",
			stem: "Bad-Name",
			content: "---\n" +
				"kind: human\n" +
				"type: boolean\n" +
				"---\n",
			wantErr: "lower-case letters",
		},
		{
			name: "missing kind",
			stem: "x",
			content: "---\n" +
				"type: boolean\n" +
				"---\n",
			wantErr: "kind is required",
		},
		{
			name: "bad kind",
			stem: "x",
			content: "---\n" +
				"kind: potato\n" +
				"type: boolean\n" +
				"---\n",
			wantErr: "want human, derive or judge",
		},
		{
			name: "missing type",
			stem: "x",
			content: "---\n" +
				"kind: human\n" +
				"---\n",
			wantErr: "type is required",
		},
		{
			name: "numeric without min/max",
			stem: "x",
			content: "---\n" +
				"kind: human\n" +
				"type: numeric\n" +
				"---\n",
			wantErr: "needs min and max",
		},
		{
			name: "min >= max",
			stem: "x",
			content: "---\n" +
				"kind: human\n" +
				"type: numeric\n" +
				"min: 5\n" +
				"max: 1\n" +
				"---\n",
			wantErr: "must be below",
		},
		{
			name: "boolean with min",
			stem: "x",
			content: "---\n" +
				"kind: human\n" +
				"type: boolean\n" +
				"min: 1\n" +
				"---\n",
			wantErr: "takes no min/max/categories",
		},
		{
			name: "categorical fewer than two categories",
			stem: "x",
			content: "---\n" +
				"kind: human\n" +
				"type: categorical\n" +
				"categories:\n" +
				"  - label: good\n" +
				"    value: 1\n" +
				"---\n",
			wantErr: "at least two categories",
		},
		{
			name: "categorical duplicate labels",
			stem: "x",
			content: "---\n" +
				"kind: human\n" +
				"type: categorical\n" +
				"categories:\n" +
				"  - label: good\n" +
				"    value: 1\n" +
				"  - label: good\n" +
				"    value: 0\n" +
				"---\n",
			wantErr: "duplicate category",
		},
		{
			name: "judge without body",
			stem: "x",
			content: "---\n" +
				"kind: judge\n" +
				"type: boolean\n" +
				"judge:\n" +
				"  model: gpt-4o\n" +
				"---\n",
			wantErr: "needs a prompt body",
		},
		{
			name: "judge without model",
			stem: "x",
			content: "---\n" +
				"kind: judge\n" +
				"type: boolean\n" +
				"---\n" +
				"Some prompt text.\n",
			wantErr: "needs judge.model",
		},
		{
			name: "judge.sample out of range",
			stem: "x",
			content: "---\n" +
				"kind: judge\n" +
				"type: boolean\n" +
				"judge:\n" +
				"  model: gpt-4o\n" +
				"  sample: 1.5\n" +
				"---\n" +
				"Prompt text.\n",
			wantErr: "judge.sample must be within 0..1",
		},
		{
			name: "derive without expr",
			stem: "x",
			content: "---\n" +
				"kind: derive\n" +
				"type: numeric\n" +
				"min: 0\n" +
				"max: 1\n" +
				"---\n",
			wantErr: "needs derive.expr",
		},
		{
			name: "human rubric rejects input_regex",
			stem: "x",
			content: "---\n" +
				"kind: human\n" +
				"type: boolean\n" +
				"select:\n" +
				"  input_regex: \"foo.*\"\n" +
				"---\n",
			wantErr: "cannot select by input_regex",
		},
		{
			name: "bad input_regex",
			stem: "x",
			content: "---\n" +
				"kind: derive\n" +
				"type: numeric\n" +
				"min: 0\n" +
				"max: 1\n" +
				"derive:\n" +
				"  expr: \"x\"\n" +
				"select:\n" +
				"  input_regex: \"(\"\n" +
				"---\n",
			wantErr: "select.input_regex:",
		},
		{
			name:    "no front matter",
			stem:    "x",
			content: "just some text\nno dashes here\n",
			wantErr: "no front matter",
		},
		{
			name: "unclosed front matter",
			stem: "x",
			content: "---\n" +
				"kind: human\n" +
				"type: boolean\n",
			wantErr: "front matter not closed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.stem+".md", tc.stem, []byte(tc.content))
			if err == nil {
				t.Fatalf("Parse: want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Parse error = %q, want to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestRubric_Validate_Boolean covers the accepted spellings and the
// rejection of anything else.
func TestRubric_Validate_Boolean(t *testing.T) {
	r := &Rubric{Name: "flag", Type: TypeBoolean}
	trueCases := []string{"1", "true", "TRUE", "True", "yes", "YES", "y", "Y"}
	for _, raw := range trueCases {
		v, _, err := r.Validate(raw)
		if err != nil || v != 1 {
			t.Errorf("Validate(%q) = %v, %v, want 1, nil", raw, v, err)
		}
	}
	falseCases := []string{"0", "false", "FALSE", "no", "NO", "n", "N"}
	for _, raw := range falseCases {
		v, _, err := r.Validate(raw)
		if err != nil || v != 0 {
			t.Errorf("Validate(%q) = %v, %v, want 0, nil", raw, v, err)
		}
	}
	if _, _, err := r.Validate("maybe"); err == nil {
		t.Error("Validate(\"maybe\") = nil error, want rejection")
	}
}

// TestRubric_Validate_Numeric covers parsing and range checking.
func TestRubric_Validate_Numeric(t *testing.T) {
	r := &Rubric{Name: "quality", Type: TypeNumeric, Min: ptrF(1), Max: ptrF(5)}
	if v, _, err := r.Validate("3"); err != nil || v != 3 {
		t.Errorf("Validate(3) = %v, %v, want 3, nil", v, err)
	}
	if v, _, err := r.Validate("1"); err != nil || v != 1 {
		t.Errorf("Validate(1, lower bound) = %v, %v, want 1, nil", v, err)
	}
	if v, _, err := r.Validate("5"); err != nil || v != 5 {
		t.Errorf("Validate(5, upper bound) = %v, %v, want 5, nil", v, err)
	}
	if _, _, err := r.Validate("0"); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("Validate(0) err = %v, want an 'outside' range error", err)
	}
	if _, _, err := r.Validate("9"); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("Validate(9) err = %v, want an 'outside' range error", err)
	}
	if _, _, err := r.Validate("abc"); err == nil || !strings.Contains(err.Error(), "not a number") {
		t.Errorf("Validate(abc) err = %v, want a 'not a number' error", err)
	}
}

// TestRubric_Validate_Categorical covers case-insensitive label matching and
// unknown-label rejection.
func TestRubric_Validate_Categorical(t *testing.T) {
	r := &Rubric{Name: "tone", Type: TypeCategorical, Categories: []Category{
		{Label: "Good", Value: 1},
		{Label: "Bad", Value: 0},
	}}
	v, label, err := r.Validate("good")
	if err != nil || v != 1 || label != "Good" {
		t.Errorf("Validate(good) = %v, %q, %v, want 1, Good, nil", v, label, err)
	}
	v, label, err = r.Validate("BAD")
	if err != nil || v != 0 || label != "Bad" {
		t.Errorf("Validate(BAD) = %v, %q, %v, want 0, Bad, nil", v, label, err)
	}
	if _, _, err := r.Validate("weird"); err == nil || !strings.Contains(err.Error(), "not one of") {
		t.Errorf("Validate(weird) err = %v, want a 'not one of' error", err)
	}
}

// TestSelect_Matches exercises every restriction independently and in
// combination with the input_regex gate (set directly since it's compiled
// only inside Rubric.validate).
func TestSelect_Matches(t *testing.T) {
	t.Run("empty select matches everything", func(t *testing.T) {
		s := Select{}
		if !s.Matches("any-agent", "any-trigger", "any-session", "any-backend", []string{"tag"}, "any input") {
			t.Error("empty Select rejected a facet set it should match")
		}
	})
	t.Run("agents restrict", func(t *testing.T) {
		s := Select{Agents: []string{"foo"}}
		if !s.Matches("foo", "", "", "", nil, "") {
			t.Error("agent foo should match")
		}
		if s.Matches("bar", "", "", "", nil, "") {
			t.Error("agent bar should not match")
		}
	})
	t.Run("triggers restrict", func(t *testing.T) {
		s := Select{Triggers: []string{"webhook"}}
		if !s.Matches("", "webhook", "", "", nil, "") {
			t.Error("trigger webhook should match")
		}
		if s.Matches("", "user", "", "", nil, "") {
			t.Error("trigger user should not match")
		}
	})
	t.Run("session_types restrict", func(t *testing.T) {
		s := Select{SessionTypes: []string{"chat"}}
		if !s.Matches("", "", "chat", "", nil, "") {
			t.Error("session type chat should match")
		}
		if s.Matches("", "", "cron", "", nil, "") {
			t.Error("session type cron should not match")
		}
	})
	t.Run("backends restrict", func(t *testing.T) {
		s := Select{Backends: []string{"claude-code"}}
		if !s.Matches("", "", "", "claude-code", nil, "") {
			t.Error("backend claude-code should match")
		}
		if s.Matches("", "", "", "codex", nil, "") {
			t.Error("backend codex should not match")
		}
	})
	t.Run("tags must all be present", func(t *testing.T) {
		s := Select{Tags: []string{"x", "y"}}
		if !s.Matches("", "", "", "", []string{"x", "y", "z"}, "") {
			t.Error("tags superset should match")
		}
		if s.Matches("", "", "", "", []string{"x"}, "") {
			t.Error("missing tag y should not match")
		}
	})
	t.Run("input_regex applies only when set", func(t *testing.T) {
		s := Select{}
		if !s.Matches("", "", "", "", nil, "anything at all") {
			t.Error("unset input_regex should match any input")
		}
		s.inputRe = regexp.MustCompile(`^err:`)
		if !s.Matches("", "", "", "", nil, "err: boom") {
			t.Error("input_regex should match a conforming input")
		}
		if s.Matches("", "", "", "", nil, "no match here") {
			t.Error("input_regex should reject a non-conforming input")
		}
	})
}

// TestRubric_Shape covers each type's rendering for listings.
func TestRubric_Shape(t *testing.T) {
	numeric := &Rubric{Type: TypeNumeric, Min: ptrF(1), Max: ptrF(5)}
	if got := numeric.Shape(); got != "1..5" {
		t.Errorf("numeric Shape() = %q, want %q", got, "1..5")
	}
	boolean := &Rubric{Type: TypeBoolean}
	if got := boolean.Shape(); got != "yes/no" {
		t.Errorf("boolean Shape() = %q, want %q", got, "yes/no")
	}
	categorical := &Rubric{Type: TypeCategorical, Categories: []Category{{Label: "a"}, {Label: "b"}}}
	if got := categorical.Shape(); got != "a|b" {
		t.Errorf("categorical Shape() = %q, want %q", got, "a|b")
	}
}
