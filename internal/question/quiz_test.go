package question

import (
	"strings"
	"testing"
)

// Distinctive quiz secrets shared by the no-leak tests: they must never appear
// in a prompt, only in the reveal. Distinctive so a leak cannot hide inside a
// plausible option label.
const (
	quizSecretAnswer      = "M0DEL-ANSWER-skylos"
	quizSecretExplanation = "XPLAN-9-never-in-a-prompt"
)

// quizOptionQuestion is the canonical multiple-choice quiz question: the
// correct option (Oslo) is one of the displayed options, so a no-leak test
// cannot treat its LABEL as secret — what must stay hidden is WHICH option is
// correct, plus the model answer and explanation.
func quizOptionQuestion(correct ...int) *Question {
	return &Question{
		Question: "Capital of <Norway>?",
		Options:  []Option{{Label: "Bergen"}, {Label: "Oslo"}, {Label: "Trondheim"}},
		Quiz:     &Quiz{Correct: correct, Explanation: quizSecretExplanation},
	}
}

// TestValidateQuiz pins the quiz input contract (#1316): a quiz question needs
// exactly one arm — correct option indexes on an option question, or a model
// answer on a free-recall (option-less) question — and every index must name a
// real option exactly once. A non-quiz question validates clean. Mutation arm:
// delete any rule and a row below fails.
func TestValidateQuiz(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		q       *Question
		wantErr string // "" = valid
	}{
		{"no quiz", &Question{Question: "Q", Options: []Option{{Label: "A"}}}, ""},
		{"option question with correct", quizOptionQuestion(1), ""},
		{"multiple correct", quizOptionQuestion(0, 2), ""},
		{"correct out of range", quizOptionQuestion(3), "0-based index 3 is out of range: question has 3 options"},
		{"correct negative", quizOptionQuestion(-1), "0-based index -1 is out of range"},
		{"correct repeated", quizOptionQuestion(1, 1), "0-based index 1 is repeated"},
		{"option question, neither correct nor answer", &Question{
			Question: "Q", Options: []Option{{Label: "A"}}, Quiz: &Quiz{},
		}, "needs \"correct\""},
		{"free recall with options (model answer on an option question)", &Question{
			Question: "Q", Options: []Option{{Label: "A"}},
			Quiz: &Quiz{ModelAnswer: "skY"},
		}, "only for a free-recall question"},
		{"free recall with answer", &Question{Question: "Q", Quiz: &Quiz{ModelAnswer: "skY"}}, ""},
		{"free recall, no answer", &Question{Question: "Q", Quiz: &Quiz{}}, "needs the correct option(s)"},
		{"free recall carrying correct indexes", &Question{
			Question: "Q", Quiz: &Quiz{Correct: []int{0}, ModelAnswer: "skY"},
		}, "only for a question with options"},
	}
	for _, tc := range cases {
		err := ValidateQuiz(tc.q)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: ValidateQuiz = %v, want nil", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: ValidateQuiz = %v, want error containing %q", tc.name, err, tc.wantErr)
		}
	}
}

// TestQuizVerdict pins the mechanical verdict rule (#1316): a verdict exists
// only for a quiz OPTION question with a non-empty delivered answer; the
// verdict is true iff the answer equals one of the correct option labels —
// including typed text that happens to match. Free recall and non-quiz
// questions get no verdict (the agent judges).
func TestQuizVerdict(t *testing.T) {
	t.Parallel()
	correct := quizOptionQuestion(1)
	if c, ok := QuizVerdict(correct, "Oslo"); !ok || !c {
		t.Errorf("label match: got (%v,%v), want (true,true)", c, ok)
	}
	if c, ok := QuizVerdict(correct, "Bergen"); !ok || c {
		t.Errorf("wrong option: got (%v,%v), want (false,true)", c, ok)
	}
	if c, ok := QuizVerdict(correct, "Oslo"); !ok || !c {
		t.Errorf("typed label match: got (%v,%v), want (true,true)", c, ok)
	}
	if _, ok := QuizVerdict(correct, ""); ok {
		t.Error("empty answer must carry no verdict")
	}
	if _, ok := QuizVerdict(&Question{Question: "Q", Options: []Option{{Label: "A"}}}, "A"); ok {
		t.Error("non-quiz question must carry no verdict")
	}
	if _, ok := QuizVerdict(&Question{Question: "Q", Quiz: &Quiz{ModelAnswer: "A"}}, "A"); ok {
		t.Error("free-recall quiz question must carry no verdict")
	}
}

// TestResolveAnswer_TypedNumberSelectsQuizOption pins the quiz typed-number
// rule (#1316): on a quiz option question a typed reply that is just a number
// selects that option by its DISPLAYED position (1-based, matching FormatText's
// "N. label" list). The number always means the position — even when a label is
// itself numeric (row below) — and out-of-range or non-numeric replies stay
// verbatim, as does everything on the button paths.
func TestResolveAnswer_TypedNumberSelectsQuizOption(t *testing.T) {
	t.Parallel()
	q := quizOptionQuestion(1)
	cases := []struct {
		choice, want string
	}{
		{"2", "Oslo"},   // the displayed number of the correct option
		{"1", "Bergen"}, // any position works, not just the correct one
		{" 3", "Trondheim"},
		{"0", "0"},       // below the list → verbatim typed text
		{"7", "7"},       // beyond the list → verbatim typed text
		{"2x", "2x"},     // not just a number → verbatim
		{"-1", "-1"},     // negative → verbatim
		{"Oslo", "Oslo"}, // the label itself still works
	}
	for _, tc := range cases {
		got, cancelled, err := ResolveAnswer(q, tc.choice)
		if err != nil || cancelled || got != tc.want {
			t.Errorf("ResolveAnswer(%q) = (%q,%v,%v), want (%q,false,nil)", tc.choice, got, cancelled, err, tc.want)
		}
	}

	// Numeric labels: a typed "4" is the FOURTH option (label "6"), never the
	// option labelled "4" — the position is the number's only meaning.
	numeric := &Question{
		Question: "Pick",
		Options:  []Option{{Label: "3"}, {Label: "4"}, {Label: "5"}, {Label: "6"}},
		Quiz:     &Quiz{Correct: []int{3}},
	}
	if got, _, _ := ResolveAnswer(numeric, "4"); got != "6" {
		t.Errorf("typed 4 with numeric labels = %q, want %q (position 4, not the label \"4\")", got, "6")
	}

	// The button path is unchanged by the quiz flag: "qa:2" is still index 2.
	if got, _, err := ResolveAnswer(q, "qa:2"); err != nil || got != "Trondheim" {
		t.Errorf("ResolveAnswer(qa:2) = (%q,%v), want Trondheim", got, err)
	}
}

// TestResolveAnswer_TypedNumberNonQuizVerbatim is a CHARACTERISATION test
// (exempt in .factory/red-gate-exempt.txt): it pins that a typed number on a
// NON-quiz question stays a verbatim answer — requirement 1/5's "non-quiz
// questions behave exactly as today". It passes on the pre-change tree by
// design.
func TestResolveAnswer_TypedNumberNonQuizVerbatim(t *testing.T) {
	t.Parallel()
	q := &Question{Question: "Pick", Options: []Option{{Label: "Red"}, {Label: "Blue"}}}
	got, cancelled, err := ResolveAnswer(q, "2")
	if err != nil || cancelled || got != "2" {
		t.Errorf("ResolveAnswer(non-quiz, \"2\") = (%q,%v,%v), want verbatim \"2\"", got, cancelled, err)
	}
}

// TestFormatReveal pins the exact reveal text (#1316): the question verbatim
// (angle brackets literal), the correct option(s) with 1-based display numbers,
// a verdict line for option questions, the model answer for free recall (no
// verdict), and the explanation only when set. PLAIN TEXT ONLY — no markdown
// markers — because chat notification paths send without a parse mode
// (round-3 finding F1); mutation arm: reintroduce a `**` heading and the
// exact-equality assertions go red.
func TestFormatReveal(t *testing.T) {
	t.Parallel()
	q := quizOptionQuestion(1)
	q.Quiz.Explanation = "Oslo has been the capital since 1299."
	want := strings.Join([]string{
		"Capital of <Norway>?",
		"Answer: 2. Oslo",
		"✅ Correct.",
		"Explanation: Oslo has been the capital since 1299.",
	}, "\n")
	if got := FormatReveal(q, "Oslo"); got != want {
		t.Errorf("correct reveal:\ngot:  %q\nwant: %q", got, want)
	}
	if got := FormatReveal(q, "Bergen"); got != strings.Join([]string{
		"Capital of <Norway>?",
		"Answer: 2. Oslo",
		"❌ Incorrect — you answered Bergen.",
		"Explanation: Oslo has been the capital since 1299.",
	}, "\n") {
		t.Errorf("incorrect reveal:\ngot: %q", got)
	}

	// Multiple correct options: comma-joined with their display numbers.
	multi := quizOptionQuestion(0, 2)
	multi.Quiz.Explanation = ""
	got := FormatReveal(multi, "Bergen")
	if !strings.Contains(got, "Answer: 1. Bergen, 3. Trondheim") {
		t.Errorf("multi-correct reveal missing joined answer: %q", got)
	}
	if strings.Contains(got, "Explanation:") {
		t.Errorf("empty explanation must be omitted: %q", got)
	}

	// Free recall: model answer, and NO verdict line (the agent judges).
	free := &Question{Question: `Translate "dog" into Greek`, Quiz: &Quiz{ModelAnswer: quizSecretAnswer, Explanation: quizSecretExplanation}}
	if got := FormatReveal(free, "σκύλος"); got != strings.Join([]string{
		`Translate "dog" into Greek`,
		"Model answer: " + quizSecretAnswer,
		"Explanation: " + quizSecretExplanation,
	}, "\n") {
		t.Errorf("free-recall reveal:\ngot:  %q", got)
	}

	// No markdown anywhere, in any variant.
	for name, g := range map[string]string{
		"correct":   FormatReveal(q, "Oslo"),
		"incorrect": FormatReveal(q, "Bergen"),
		"free":      FormatReveal(free, "x"),
	} {
		for _, marker := range []string{"**", "_", "`", "~~"} {
			if strings.Contains(g, marker) {
				t.Errorf("%s reveal contains markdown marker %q: %q", name, marker, g)
			}
		}
	}
}

// TestQuizNoLeakInPrompt guards the no-leak rule (#1316) at the source: the
// prompt surface (FormatText + Choices) never carries the quiz secrets — the
// model answer or explanation strings — and does not reveal WHICH option is
// correct: the rendering is byte-identical no matter which option is marked.
// The sequential presenter builds every chat prompt from exactly these two
// functions, so this is the single upstream pin for Telegram, Discord and the
// plain-text fallback alike (their own tests drive it through their real send
// paths). Mutation arm: make FormatText or Choices read q.Quiz and either the
// secret-substring or the identical-rendering check goes red.
func TestQuizNoLeakInPrompt(t *testing.T) {
	t.Parallel()
	q := quizOptionQuestion(1)
	q.Quiz.ModelAnswer = quizSecretAnswer // over-marked on purpose: even a
	// wrongly-validated shape must not leak into the prompt.

	text := FormatText(q, 0, 1)
	for _, secret := range []string{quizSecretAnswer, quizSecretExplanation} {
		if strings.Contains(text, secret) {
			t.Errorf("prompt text leaks %q:\n%s", secret, text)
		}
	}
	for _, c := range Choices(q) {
		if strings.Contains(c.Label, quizSecretAnswer) || strings.Contains(c.Label, quizSecretExplanation) {
			t.Errorf("choice label leaks a secret: %+v", c)
		}
		if strings.HasPrefix(c.Data, "correct") {
			t.Errorf("choice data marks correctness: %+v", c)
		}
	}

	// Which option is correct must not change the rendering: build the same
	// question with each possible correct index and demand identical output.
	var base string
	for i := range q.Options {
		variant := quizOptionQuestion(i)
		variant.Quiz.ModelAnswer = quizSecretAnswer
		if v := FormatText(variant, 0, 1) + choicesKey(Choices(variant)); base == "" {
			base = v
		} else if v != base {
			t.Errorf("prompt differs when correct=%d — the prompt reveals the answer:\n%s\n%s", i, base, v)
		}
	}

	// Positive control (guards a vacuous pass): the secrets DO live on the
	// question, and FormatText does render the numbered options.
	if !strings.Contains(q.Quiz.Explanation, "XPLAN-9") {
		t.Fatal("test bug: explanation secret not on the question")
	}
	for _, want := range []string{"1. **Bergen**", "2. **Oslo**", "3. **Trondheim**"} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt must number the options (%q missing):\n%s", want, text)
		}
	}
}

// choicesKey flattens choices for byte-comparison in TestQuizNoLeakInPrompt.
func choicesKey(cs []Choice) string {
	var b strings.Builder
	for _, c := range cs {
		b.WriteString(c.Label)
		b.WriteByte('|')
		b.WriteString(c.Data)
		b.WriteByte(';')
	}
	return b.String()
}
