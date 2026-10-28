package discord

import (
	"strings"
	"testing"

	"foci/internal/platform"
	"foci/internal/question"

	"github.com/bwmarrin/discordgo"
)

// Distinctive quiz secrets (see internal/question/quiz_test.go for why the
// correct option's LABEL cannot be one): they must appear only in a reveal.
const (
	quizTestAnswer      = "M0DEL-ANSWER-skylos"
	quizTestExplanation = "XPLAN-9-never-in-a-prompt"
)

// quizTestQuestion is the canonical multiple-choice quiz question.
func quizTestQuestion(correct int) *question.Question {
	return &question.Question{
		Question: "Capital of <Norway>?",
		Options:  []question.Option{{Label: "Bergen"}, {Label: "Oslo"}, {Label: "Trondheim"}},
		Quiz:     &question.Quiz{Correct: []int{correct}, Explanation: quizTestExplanation},
	}
}

// quizButtons mirrors what the ask presenter hands the platform layer:
// question.Choices mapped onto platform buttons.
func quizButtons(q *question.Question) []platform.ButtonChoice {
	cs := question.Choices(q)
	out := make([]platform.ButtonChoice, 0, len(cs))
	for _, c := range cs {
		out = append(out, platform.ButtonChoice{Label: c.Label, Data: c.Data})
	}
	return out
}

// buttonSurfaces flattens a sent message's component ids and labels — the
// button CUSTOM IDs carry routing data, so they are leak surfaces too.
func buttonSurfaces(comps []discordgo.MessageComponent) []string {
	var out []string
	for _, c := range comps {
		if row, ok := c.(discordgo.ActionsRow); ok {
			for _, b := range row.Components {
				if btn, ok := b.(discordgo.Button); ok {
					out = append(out, btn.Label, btn.CustomID)
				}
			}
		}
	}
	return out
}

// assertQuizNoLeak fails when any quiz secret appears in any surface.
func assertQuizNoLeak(t *testing.T, surfaces ...string) {
	t.Helper()
	for _, secret := range []string{quizTestAnswer, quizTestExplanation} {
		for i, s := range surfaces {
			if strings.Contains(s, secret) {
				t.Errorf("surface %d leaks quiz secret %q:\n%s", i, secret, s)
			}
		}
	}
}

// TestQuizPrompt_NoLeakOnDiscord pins requirement 3 on the real Discord send
// path: the prompt a quiz question produces — the whole message content and
// every button label/custom-id — contains NONE of the model answer or
// explanation, and does not reveal which option is correct (byte-identical
// prompt under every possible correct index). The ask presenter builds the
// prompt from question.FormatText + question.Choices and posts it through
// SendTextWithButtonsToSession, so that is the path driven here. Guard test;
// mutation arm: make the presenter or FormatText read q.Quiz and the secret or
// identical-prompt assertions go red.
func TestQuizPrompt_NoLeakOnDiscord(t *testing.T) {
	b, fs, idx := newTestBot(t, "main")
	if err := idx.SetDefaultChat("main", platformName, 999); err != nil {
		t.Fatal(err)
	}

	q := quizTestQuestion(1)
	if _, err := b.SendTextWithButtonsToSession("main/c222", question.FormatText(q, 0, 1), quizButtons(q), "im:"); err != nil {
		t.Fatal(err)
	}
	sent := fs.lastSend(t)
	if sent.channelID != "222" {
		t.Errorf("routed to channel %q, want the session's channel", sent.channelID)
	}
	surfaces := append([]string{sent.content}, buttonSurfaces(sent.components)...)
	assertQuizNoLeak(t, surfaces...)

	// The prompt is byte-identical whichever option is marked correct.
	for _, correct := range []int{0, 2} {
		bq := quizTestQuestion(correct)
		if _, err := b.SendTextWithButtonsToSession("main/c222", question.FormatText(bq, 0, 1), quizButtons(bq), "im:"); err != nil {
			t.Fatal(err)
		}
		if got := fs.lastSend(t).content; got != sent.content {
			t.Errorf("prompt differs when correct=%d — the prompt reveals the answer:\n%s\n%s", correct, sent.content, got)
		}
	}

	// Numbering pin (requirement 5): the options are shown numbered (Discord
	// renders FormatText's markdown as-is, so the bold markers stay).
	for _, want := range []string{"1. **Bergen**", "2. **Oslo**", "3. **Trondheim**"} {
		if !strings.Contains(sent.content, want) {
			t.Errorf("prompt must number the options (%q missing):\n%s", want, sent.content)
		}
	}
}

// TestQuizReveal_DeliveredVerbatimOnDiscord pins the reveal on Discord's
// notification path (#1316): SendNotificationToSession delivers
// question.FormatReveal verbatim to the asking session's channel — the model
// answer and explanation become visible only now, the verdict line included,
// exactly once (the ask layer fires the hook once per answered question).
// Fail-arm: reintroduce markdown into FormatReveal and the exact-equality
// assertion goes red.
func TestQuizReveal_DeliveredVerbatimOnDiscord(t *testing.T) {
	b, fs, idx := newTestBot(t, "main")
	if err := idx.SetDefaultChat("main", platformName, 999); err != nil {
		t.Fatal(err)
	}

	q := quizTestQuestion(1)
	q.Quiz.Explanation = "Oslo has been the capital since 1299."
	b.SendNotificationToSession("main/c222", question.FormatReveal(q, "Bergen", nil))

	sent := fs.lastSend(t)
	if sent.channelID != "222" {
		t.Errorf("routed to channel %q, want the session's channel", sent.channelID)
	}
	want := strings.Join([]string{
		"Capital of <Norway>?",
		"Answer: 2. Oslo",
		"❌ Incorrect — you answered Bergen.",
		"Explanation: Oslo has been the capital since 1299.",
	}, "\n")
	if sent.content != want {
		t.Errorf("Discord reveal:\ngot:  %q\nwant: %q", sent.content, want)
	}

	// Free recall on the same path: model answer, no verdict line.
	free := &question.Question{
		Question: `Translate "dog" into Greek`,
		Quiz:     &question.Quiz{ModelAnswer: quizTestAnswer, Explanation: quizTestExplanation},
	}
	b.SendNotificationToSession("main/c222", question.FormatReveal(free, "σκύλος", nil))
	sent = fs.lastSend(t)
	want = strings.Join([]string{
		`Translate "dog" into Greek`,
		"Model answer: " + quizTestAnswer,
		"Explanation: " + quizTestExplanation,
	}, "\n")
	if sent.content != want {
		t.Errorf("Discord free-recall reveal:\ngot:  %q\nwant: %q", sent.content, want)
	}
}
