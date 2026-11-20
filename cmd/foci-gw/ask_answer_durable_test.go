package main

import (
	"context"
	"testing"

	"foci/internal/agent"
)

// #3197: an ask answer had no stored form (sessionChatDurable refuses an ask
// id), so a restart while it waited at a gate lost it. Dick 2026-11-20: it
// survives at "Any gate" and comes back "As your answer".

// captureSessionChat runs deliver on a started inbox and returns the
// envelope it enqueued.
func captureSessionChat(t *testing.T, deliver func(ag *agent.Agent, capture injectOption)) agent.Envelope {
	t.Helper()
	ag := &agent.Agent{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ag.StartInbox(ctx)
	var captured agent.Envelope
	deliver(ag, func(env *agent.Envelope) { captured = *env })
	if captured.Inject == nil {
		t.Fatal("no envelope captured")
	}
	return captured
}

// TestDeliverToSessionChatThen_AskAnswerIsDurable: the user route of an ask
// answer carries a stored form marked as user input with its ask id; the
// system route with an ask id still has none.
func TestDeliverToSessionChatThen_AskAnswerIsDurable(t *testing.T) {
	user := captureSessionChat(t, func(ag *agent.Agent, capture injectOption) {
		ctx := agent.WithUserInput(context.Background())
		deliverToSessionChatThen(ag, ctx, "ask_grader", stubConnMgr{}, "scout", "scout/c42", "the answer", nil, "req-1", nil, capture)
	})
	d := user.Inject.Durable
	if d == nil {
		t.Fatal("ask answer: Durable = nil, want a stored form (a restart would lose it)")
	}
	if d.Kind != durableSessionChat || d.Trigger != "ask_grader" || d.Text != "the answer" {
		t.Errorf("Durable = %+v, want session_chat/ask_grader/the answer", *d)
	}
	if d.Args[durableUserInputArg] != "true" || d.Args[durableAskReqArg] != "req-1" {
		t.Errorf("Durable.Args = %v, want user_input=true ask_req_id=req-1", d.Args)
	}

	system := captureSessionChat(t, func(ag *agent.Agent, capture injectOption) {
		deliverToSessionChatThen(ag, context.Background(), "ask_grader", stubConnMgr{}, "scout", "scout/c42", "expired", nil, "req-1", nil, capture)
	})
	if system.Inject.Durable != nil {
		t.Errorf("system route with an ask id: Durable = %+v, want nil", *system.Inject.Durable)
	}
}

// TestRestoreSessionChat_AskAnswerComesBackAsUserInput: a stored ask answer
// is rebuilt as the user's input with its ask id; a plain record stays a
// system injection.
func TestRestoreSessionChat_AskAnswerComesBackAsUserInput(t *testing.T) {
	rec := *userInputDurable("ask_grader", "the answer", "req-1", nil)
	user := captureSessionChat(t, func(ag *agent.Agent, capture injectOption) {
		restoreSessionChat(ag, context.Background(), stubConnMgr{}, "scout", "scout/c42", rec, "uh-1-1", capture)
	})
	if !user.Inject.UserInput || user.Text != "the answer" {
		t.Errorf("restored answer: UserInput=%v Text=%q, want user input with the answer", user.Inject.UserInput, user.Text)
	}
	if user.Inject.AskReqID != "req-1" || user.Inject.Trigger != "ask_grader" {
		t.Errorf("restored answer: AskReqID=%q Trigger=%q, want req-1/ask_grader", user.Inject.AskReqID, user.Inject.Trigger)
	}

	plain := agent.DurableInject{Kind: durableSessionChat, Trigger: "agent", Text: "hello"}
	sys := captureSessionChat(t, func(ag *agent.Agent, capture injectOption) {
		restoreSessionChat(ag, context.Background(), stubConnMgr{}, "scout", "scout/c42", plain, "uh-1-2", capture)
	})
	if sys.Inject.UserInput || sys.Inject.AskReqID != "" {
		t.Errorf("restored system record: UserInput=%v AskReqID=%q, want a plain system injection", sys.Inject.UserInput, sys.Inject.AskReqID)
	}
}
