package tools

// A direct spawn whose call died delivers what it produced to its spawner as
// a late report (#3168, Dick 2026-11-18 19:41: "Yes, deliver as late report").
//
// A direct spawn returns its result to the tool call that started it. When
// that call dies without a stop (its caller's ctx is cancelled: the calling
// shell hit its time limit or was killed), nobody reads what the call
// returns. Before #3168 everything the spawn produced was lost there: the
// send_to_spawner messages it kept for the head of its result, the text of
// its interrupted turn, and its whole result when the run completed after the
// call had died. Now they go to the spawner session through deliverChild,
// #3163's path: a chat gets an inject, an ended background spawn is woken
// with it, and a stopped one passes it up to its own spawner.
//
// Not delivered:
//   - a spawn stopped by spawn_stop or /stop (Dick 2026-11-18: "Drop on
//     stop"): runDirect returns before it gets here;
//   - an API one-shot that a restart resumes (endDirect's resumes): the
//     resumed run delivers its result and kept messages itself;
//   - a spawn whose call died and that produced nothing (no kept message,
//     no text): a report of nothing would only start a turn.
//
// Each piece goes once: the kept messages are taken from the tracker by
// endDirect, and the result is read once from the run's end. A run that
// outlived its grace (awaitDirectRun) sends its kept messages at once and its
// result in a second report when the run ends, so neither waits on the other
// and neither repeats.

import (
	"fmt"
	"strings"
)

// lateDirectNote heads a direct spawn's late report.
const lateDirectNote = "the call that started this spawn ended before the spawn did (its caller was gone, e.g. the calling shell hit its time limit), " +
	"so nothing read the call's result. What the spawn produced comes to you here instead."

// deliverLateDirect sends parent's direct spawn info's kept messages and the
// end of its run (res, runErr, with the text its tee saw) to parent as a late
// report. later is non-nil when the run had not ended (awaitDirectRun's
// grace passed): its end then arrives there, and is reported when it does.
func (s *Spawner) deliverLateDirect(parent string, info RunningSpawn, kept []SpawnLogEntry, res ToolResult, runErr error, tee *spawnTextSink, later <-chan directRunEnd) {
	if s.deps.Notifier == nil {
		spawnLog.Infof("direct spawn=%s: its call ended before it did, and this agent has no background delivery: its output is not delivered", info.ID)
		return
	}
	head := lateDirectHead(info)
	if later != nil {
		if len(kept) > 0 {
			s.sendLateDirect(parent, info, keptMessagesHead(info.ID, kept)+head+
				" Its run has not ended yet: what it returns follows in a separate report when it does.")
		}
		go func() {
			r := <-later
			if body, ok := lateDirectBody(r.res, r.err, tee.partialText()); ok {
				s.sendLateDirect(parent, info, head+" "+body)
			}
		}()
		return
	}
	body, ok := lateDirectBody(res, runErr, tee.partialText())
	if !ok && len(kept) == 0 {
		spawnLog.Infof("direct spawn=%s: its call ended before it did, and it produced nothing to deliver", info.ID)
		return
	}
	s.sendLateDirect(parent, info, keptMessagesHead(info.ID, kept)+head+" "+body)
}

// lateDirectHead opens a direct spawn's late report.
func lateDirectHead(info RunningSpawn) string {
	return fmt.Sprintf("[SPAWN RESULT, AFTER ITS CALL ENDED] %s (spawn %s): %s", info.Label(), info.ID, lateDirectNote)
}

// lateDirectBody says how a direct spawn's run ended: its result when it
// completed, else its error and the text it had written (partial). ok is
// false when it has nothing to show: it failed before writing any text.
func lateDirectBody(res ToolResult, runErr error, partial string) (body string, ok bool) {
	if runErr == nil {
		text := res.Text
		if strings.TrimSpace(text) == "" {
			text = partial
		}
		if strings.TrimSpace(text) == "" {
			return "It completed with an empty response.", true
		}
		return "It completed:\n\n" + text, true
	}
	if strings.TrimSpace(partial) == "" {
		return fmt.Sprintf("It was cut off (%v) before it wrote any text.", runErr), false
	}
	return fmt.Sprintf("It was cut off (%v). The text it had written:\n\n%s", runErr, partial), true
}

// sendLateDirect delivers msg, parent's direct spawn info's late report.
func (s *Spawner) sendLateDirect(parent string, info RunningSpawn, msg string) {
	spawnLog.Infof("direct spawn=%s: its call ended before it did: late report to %s (%d chars)", info.ID, parent, len(msg))
	s.deliverChild(parent, childReport{Kind: "result", Child: fmt.Sprintf("%s (%s)", info.Label(), info.ID), Text: msg})
}

// partialText is the assistant text the spawn's run wrote: its turn's
// FinalText when the turn completed (an interrupted turn's carries the text
// so far), else the text blocks it streamed. Read it only after the run has
// returned: the buffer is not safe while the run writes it.
func (s *spawnTextSink) partialText() string {
	if t := s.FinalText(); strings.TrimSpace(t) != "" {
		return t
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.blocks, "\n\n")
}
