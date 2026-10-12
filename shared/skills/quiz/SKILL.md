---
name: quiz
description: "Ask quiz questions with foci_ask: known-answer drills (SR, vocab, checks of understanding) where the user's client hides the answer until they reveal it. Input shape, what comes back, and the rules (never put the answer in the question text)."
owner: foci
seeded: true
---

> **This `SKILL.md` is yours to customise** (seed-if-missing — override it, add your own sibling files). Changes to it survive a restart, but they live only on this install: to change the skill for every agent, edit it in the foci repo (`shared/skills/quiz/`).

# Quiz questions with `foci_ask`

Use when you know the correct answer and want the user to recall it: spaced-repetition drills,
vocabulary, checks of understanding. Not for genuine questions — a plain `foci_ask` already covers
those.

## Input

Add a `quiz` object to any question in an `foci_ask` call:

```json
{"questions":[
  {"question":"Capital of Norway?","header":"Geography",
   "options":[{"label":"Bergen"},{"label":"Oslo"},{"label":"Trondheim"}],
   "quiz":{"correct":[1],"explanation":"Oslo has been the capital since 1299."}},
  {"question":"Translate \"dog\" into Greek",
   "quiz":{"answer":"σκύλος","explanation":"plural: σκύλοι"}}
]}
```

- `correct` — **0-based** indexes into that question's `options` array (the array you wrote; `[1]`
  = the second option — NOT the 1-based numbers the user sees).
- `answer` — a model answer, for a free-recall question (no `options` only).
- `explanation` — optional, shown on reveal.
- A quiz question needs `correct` (options) or `answer` (free recall); anything else is a tool
  error before anything reaches the user.

## What the user sees

The client **hides the answer until the user reveals it** — never leak it yourself. On the app the
whole quiz rides the form and reveal is local. On chat platforms (Telegram/Discord/plain text) the
options are numbered (`1. label`) and a typed reply that is just a number selects that option
(the number always means the position, even when a label is itself a number); after the user
answers, one message reveals the correct answer, the explanation and — for option questions —
whether they were right.

## What you get back

The normal answer batch, plus `quiz_results` (`{question text: true|false}`, and
`quiz_results_by_id` when you set `id`s) for quiz **option** questions — plain multiple choice
needs no grader. Free-recall answers carry no verdict: judge the text yourself (the model answer
is echoed in the reveal, not in the batch).

## Rules

- **Do not put the answer in the question text.** The whole point is the user recalls it.
- Like any `ask` it is async: post it and end your turn; answers arrive as a new message.
- A configured `--grader` also receives each question's quiz data.
