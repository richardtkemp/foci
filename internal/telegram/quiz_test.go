package telegram

import (
	"strings"
	"testing"

	"foci/internal/command"
	"foci/internal/platform"
	"foci/internal/question"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// quizTestSecrets are distinctive strings that must appear ONLY in a reveal,
// never in a prompt (see question.quizOptionQuestion for why the correct
// option's LABEL cannot be treated as secret — it is one of the displayed
// options — so the leak guarded here is the model answer, the explanation, and
// WHICH option is correct).
const (
	quizTestAnswer      = "M0DEL-ANSWER-skylos"
	quizTestExplanation = "XPLAN-9-never-in-a-prompt"
)

// quizTestQuestion is the F1-shaped quiz question: its text carries the angle
// brackets the failed attempt choked on, and its explanation is a distinctive
// secret.
func quizTestQuestion(correct int) *question.Question {
	return &question.Question{
		Question: "Capital of <Norway>?",
		Options:  []question.Option{{Label: "Bergen"}, {Label: "Oslo"}, {Label: "Trondheim"}},
		Quiz:     &question.Quiz{Correct: []int{correct}, Explanation: quizTestExplanation},
	}
}

// quizButtons mirrors what the ask presenter (newAskPresentFn) hands the
// platform layer: question.Choices mapped onto platform buttons.
func quizButtons(q *question.Question) []platform.ButtonChoice {
	cs := question.Choices(q)
	out := make([]platform.ButtonChoice, 0, len(cs))
	for _, c := range cs {
		out = append(out, platform.ButtonChoice{Label: c.Label, Data: c.Data})
	}
	return out
}

// assertQuizNoLeak fails when any of the quiz secrets appear in any of the
// prompt surfaces (the whole sent text, every button label and every button's
// callback data), or when one of the strings is empty (a vacuous guard).
func assertQuizNoLeak(t *testing.T, surfaces ...string) {
	t.Helper()
	for _, secret := range []string{quizTestAnswer, quizTestExplanation} {
		if secret == "" {
			t.Fatal("test bug: empty secret")
		}
		for i, s := range surfaces {
			if strings.Contains(s, secret) {
				t.Errorf("surface %d leaks quiz secret %q:\n%s", i, secret, s)
			}
		}
	}
}

// TestQuizPrompt_NoLeakOnTelegram pins requirement 3 on the real Telegram send
// path: the prompt a quiz question produces — the whole text the bot sends and
// every button label/callback-data — contains NONE of the model answer or
// explanation, and does not reveal which option is correct (byte-identical
// prompt under every possible correct index). The ask presenter builds the
// prompt from question.FormatText + question.Choices and posts it through
// SendTextWithButtonsToSession, so that is the path driven here. Guard test;
// mutation arm: make the presenter or FormatText read q.Quiz and the secret or
// identical-prompt assertions go red.
func TestQuizPrompt_NoLeakOnTelegram(t *testing.T) {
	b, mock := testBot(nil, command.NewRegistry())
	b.SetChatID(12345)

	q := quizTestQuestion(1)
	text := question.FormatText(q, 0, 1)
	if _, err := b.SendTextWithButtonsToSession("agent:test:main", text, quizButtons(q), "im:"); err != nil {
		t.Fatal(err)
	}

	if mock.lastSendOpts == nil {
		t.Fatal("prompt send did not carry opts")
	}
	markup, ok := mock.lastSendOpts.ReplyMarkup.(gotgbot.InlineKeyboardMarkup)
	if !ok || markup.InlineKeyboard == nil {
		t.Fatalf("prompt send did not carry buttons (markup %T)", mock.lastSendOpts.ReplyMarkup)
	}
	surfaces := []string{mock.lastSendInjected}
	for _, row := range markup.InlineKeyboard {
		for _, btn := range row {
			surfaces = append(surfaces, btn.Text, btn.CallbackData)
		}
	}
	assertQuizNoLeak(t, surfaces...)

	// The prompt is byte-identical whichever option is marked correct: the
	// whole sent text, not just the parts we control.
	first := mock.lastSendInjected
	for _, correct := range []int{0, 2} {
		bq := quizTestQuestion(correct)
		if _, err := b.SendTextWithButtonsToSession("agent:test:main",
			question.FormatText(bq, 0, 1), quizButtons(bq), "im:"); err != nil {
			t.Fatal(err)
		}
		if mock.lastSendInjected != first {
			t.Errorf("prompt differs when correct=%d — the prompt reveals the answer:\n%s\n%s", correct, first, mock.lastSendInjected)
		}
	}

	// Numbering pin (requirement 5): the options are shown numbered —
	// ConvertToTelegramHTML renders FormatText's "**label**" as <b>label</b>.
	for _, want := range []string{"1. <b>Bergen</b>", "2. <b>Oslo</b>", "3. <b>Trondheim</b>"} {
		if !strings.Contains(first, want) {
			t.Errorf("prompt must number the options (%q missing):\n%s", want, first)
		}
	}
}

// TestQuizReveal_PlainTextNoMarkdown is the round-3 finding F1 regression: the
// quiz reveal reaches Telegram through SendNotificationToSession →
// sendNotificationToChat → SendMessage(chatID, chunk, NIL opts) — no parse
// mode, no ConvertToTelegramHTML — so the reveal must be PLAIN TEXT with no
// markdown markers, and the question's `<`/`>`/`&` must pass through literally
// (no HTML-entity rewriting, nothing for Telegram's parser to reject). The
// exact text is asserted. Fail-arm (run once during implementation):
// reintroduce a `**<question>**` heading in FormatReveal and this goes red.
func TestQuizReveal_PlainTextNoMarkdown(t *testing.T) {
	b, mock := testBot(nil, command.NewRegistry())
	b.SetChatID(12345)

	q := quizTestQuestion(1)
	q.Quiz.Explanation = "Oslo has been the capital since 1299."
	reveal := question.FormatReveal(q, "Bergen", nil)

	if got := b.SendNotificationToSession("agent:test:main", reveal); got != "1" {
		t.Errorf("SendNotificationToSession id = %q, want the sent message id", got)
	}

	// No parse mode: the notification path must not opt into HTML/markdown.
	if mock.lastSendOpts != nil {
		t.Errorf("notification must send with nil opts (no parse mode), got %+v", mock.lastSendOpts)
	}

	want := strings.Join([]string{
		"Capital of <Norway>?",
		"Answer: 2. Oslo",
		"❌ Incorrect — you answered Bergen.",
		"Explanation: Oslo has been the capital since 1299.",
	}, "\n")
	if mock.lastSendInjected != want {
		t.Errorf("Telegram reveal text:\ngot:  %q\nwant: %q", mock.lastSendInjected, want)
	}
	for _, marker := range []string{"**", "<b>", "&lt;", "&gt;", "&amp;", "\\"} {
		if strings.Contains(mock.lastSendInjected, marker) {
			t.Errorf("reveal must not reach Telegram carrying %q (literal markdown/HTML): %q", marker, mock.lastSendInjected)
		}
	}
	if !strings.Contains(mock.lastSendInjected, "<Norway>") {
		t.Errorf("angle brackets must stay literal, got %q", mock.lastSendInjected)
	}
}

// TestQuizReveal_FreeRecallOnTelegram pins the free-recall reveal on the same
// path: the model answer and explanation become visible only NOW, verbatim,
// with no verdict line.
func TestQuizReveal_FreeRecallOnTelegram(t *testing.T) {
	b, mock := testBot(nil, command.NewRegistry())
	b.SetChatID(12345)

	q := &question.Question{
		Question: `Translate "dog" into Greek`,
		Quiz:     &question.Quiz{ModelAnswer: quizTestAnswer, Explanation: quizTestExplanation},
	}
	b.SendNotificationToSession("agent:test:main", question.FormatReveal(q, "σκύλος", nil))

	want := strings.Join([]string{
		`Translate "dog" into Greek`,
		"Model answer: " + quizTestAnswer,
		"Explanation: " + quizTestExplanation,
	}, "\n")
	if mock.lastSendInjected != want {
		t.Errorf("Telegram free-recall reveal:\ngot:  %q\nwant: %q", mock.lastSendInjected, want)
	}
}
