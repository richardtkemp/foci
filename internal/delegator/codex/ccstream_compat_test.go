package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/delegator"
)

func applySessionAndTurn(b *Backend, session *delegator.SessionEvents, turn *delegator.TurnEvents) {
	b.AttachSessionEvents(session)
	b.turnMu.Lock()
	b.turnActive = true
	b.turnEvents = turn
	b.turnResultCh = make(chan *delegator.TurnResult, 1)
	b.turnText.Reset()
	b.turnTools = 0
	b.turnMu.Unlock()
}

func mustItem(t *testing.T, item itemEnvelope) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal item: %v", err)
	}
	return raw
}

func TestAttachSessionEvents_TextDelivery(t *testing.T) {
	t.Parallel()

	var texts []string
	b := &Backend{}
	applySessionAndTurn(b,
		&delegator.SessionEvents{OnText: func(s string) { texts = append(texts, s) }},
		&delegator.TurnEvents{},
	)

	b.onItemCompleted(&itemCompletedParams{Item: mustItem(t, itemEnvelope{Type: "agentMessage", ID: "m1", Text: "Hello "})})
	b.onItemCompleted(&itemCompletedParams{Item: mustItem(t, itemEnvelope{Type: "agentMessage", ID: "m2", Text: "world!"})})

	b.turnMu.Lock()
	got := b.turnText.String()
	b.turnMu.Unlock()
	if got != "Hello world!" {
		t.Errorf("turnText = %q, want %q", got, "Hello world!")
	}
	if len(texts) != 2 || texts[0] != "Hello " || texts[1] != "world!" {
		t.Errorf("OnText = %v, want [Hello  world!]", texts)
	}
}

func TestAttachSessionEvents_TextDelta(t *testing.T) {
	t.Parallel()

	var deltas []string
	b := &Backend{}
	applySessionAndTurn(b,
		&delegator.SessionEvents{OnTextDelta: func(d string) { deltas = append(deltas, d) }},
		&delegator.TurnEvents{},
	)

	b.onAgentMessageDelta(&agentMessageDeltaParams{Delta: "foo"})
	b.onAgentMessageDelta(&agentMessageDeltaParams{Delta: "bar"})

	if strings.Join(deltas, "") != "foobar" {
		t.Errorf("deltas = %v, want [foo bar]", deltas)
	}
}

func TestAttachSessionEvents_ToolDelivery(t *testing.T) {
	t.Parallel()

	var starts, ends int
	b := &Backend{}
	applySessionAndTurn(b,
		&delegator.SessionEvents{
			OnToolStart: func(id, name, input string) { starts++ },
			OnToolEnd:   func(id, name, output string, isError bool) { ends++ },
		},
		&delegator.TurnEvents{},
	)

	b.onItemStarted(&itemStartedParams{Item: mustItem(t, itemEnvelope{Type: "commandExecution", ID: "c1", Command: "ls"})})
	b.onItemCompleted(&itemCompletedParams{Item: mustItem(t, itemEnvelope{Type: "commandExecution", ID: "c1", Status: "completed"})})
	b.onItemStarted(&itemStartedParams{Item: mustItem(t, itemEnvelope{Type: "commandExecution", ID: "c2", Command: "pwd"})})
	b.onItemCompleted(&itemCompletedParams{Item: mustItem(t, itemEnvelope{Type: "commandExecution", ID: "c2", Status: "failed"})})

	if starts != 2 || ends != 2 {
		t.Errorf("starts=%d ends=%d, want 2/2", starts, ends)
	}
}

func TestAttachSessionEvents_NilSafe(t *testing.T) {
	t.Parallel()

	b := &Backend{}
	applySessionAndTurn(b, nil, &delegator.TurnEvents{})

	b.onItemCompleted(&itemCompletedParams{Item: mustItem(t, itemEnvelope{Type: "agentMessage", ID: "m1", Text: "x"})})
	b.onItemStarted(&itemStartedParams{Item: mustItem(t, itemEnvelope{Type: "commandExecution", ID: "c1"})})
	b.onAgentMessageDelta(&agentMessageDeltaParams{Delta: "y"})
}

func TestTextDelivery_TurnCleared_StillGoesToSessionEvents(t *testing.T) {
	t.Parallel()

	var sessionTexts []string
	var turnCompleted bool

	b := &Backend{}
	applySessionAndTurn(b,
		&delegator.SessionEvents{OnText: func(s string) { sessionTexts = append(sessionTexts, s) }},
		&delegator.TurnEvents{
			OnTurnComplete: func(*delegator.TurnResult) { turnCompleted = true },
		},
	)

	b.onItemCompleted(&itemCompletedParams{Item: mustItem(t, itemEnvelope{Type: "agentMessage", ID: "m1", Text: "first round"})})

	b.completeTurn(&delegator.TurnResult{Text: "first round"})
	if !turnCompleted {
		t.Fatal("OnTurnComplete did not fire")
	}

	b.turnMu.Lock()
	turnEventsAfter := b.turnEvents
	b.turnMu.Unlock()
	if turnEventsAfter != nil {
		t.Fatal("turnEvents not cleared after completeTurn")
	}

	b.onItemCompleted(&itemCompletedParams{Item: mustItem(t, itemEnvelope{Type: "agentMessage", ID: "m2", Text: "post-turn"})})

	if len(sessionTexts) != 2 || sessionTexts[1] != "post-turn" {
		t.Errorf("sessionTexts = %v, want [first round post-turn]", sessionTexts)
	}
}
