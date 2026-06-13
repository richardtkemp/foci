package cctmux

import "foci/internal/delegator"

// testEvents is a watchEvents recorder for watcher-level tests. It mirrors the
// four callbacks cctmux's JSONL watcher dispatches; nil callbacks are no-ops.
// (Production routes these to SessionEvents/TurnEvents on the Backend; tests use
// this to assert the watcher's parsing/dispatch in isolation.)
type testEvents struct {
	OnText         func(text string)
	OnToolStart    func(id, name, input string)
	OnToolEnd      func(id, name, output string, isError bool)
	OnTurnComplete func(result *delegator.TurnResult)
}

func (e *testEvents) onText(text string) {
	if e.OnText != nil {
		e.OnText(text)
	}
}

func (e *testEvents) onToolStart(id, name, input string) {
	if e.OnToolStart != nil {
		e.OnToolStart(id, name, input)
	}
}

func (e *testEvents) onToolEnd(id, name, output string, isError bool) {
	if e.OnToolEnd != nil {
		e.OnToolEnd(id, name, output, isError)
	}
}

func (e *testEvents) onTurnComplete(result *delegator.TurnResult) {
	if e.OnTurnComplete != nil {
		e.OnTurnComplete(result)
	}
}
