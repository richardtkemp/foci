package tools

// A background spawn whose run ends in [[NO_RESPONSE]] (#3238).
//
// A spawn's final answer is its result: it goes to its spawner as the
// [SPAWN RESULT]. A character spawn still carries its chat habits, and one
// of them is to answer an automated message it needs to do nothing about with
// [[NO_RESPONSE]]. Seen on the work Mac (29 Oct, spawn-c379b9b85463add6): the
// spawn sent its report to its spawner with send_to_session, then ended its
// run with "[[NO_RESPONSE]]" twice. Claude Code's turn text joins every
// message of the turn, so its [SPAWN RESULT] read "Now the todo item." (a
// note written before a tool call) and the two markers: no report, and
// nothing to tell the spawner that the report was missing.
//
// The marker means nothing to a spawner: an agent's input is not a chat
// that a marker can silence. So the result never carries it:
//   - a run whose text is only markers reports that it gave no answer;
//   - a run whose LAST message is only a marker, after other messages,
//     reports the text before the marker with a note that it may be progress
//     notes and not a report;
//   - a marker added at the end of a message with real text (one message,
//     "the report. [[NO_RESPONSE]]") is removed, and the rest is the answer.

import (
	"strings"

	"foci/internal/platform"
)

// spawnNoAnswer is the result of a spawn run whose text was only
// [[NO_RESPONSE]] or nothing.
const spawnNoAnswer = "(No answer: the spawn ended its run with [[NO_RESPONSE]] and wrote no other text. " +
	"Any message it sent you while it ran came separately. spawn_status shows its activity log.)"

// spawnSilentLastNote follows the text of a spawn run whose last message was
// only [[NO_RESPONSE]].
const spawnSilentLastNote = "(The spawn ended its run with a message of only [[NO_RESPONSE]], not an answer. " +
	"The text above is what it wrote before that message. It can be progress notes and not a report. " +
	"Any message it sent you while it ran came separately. spawn_status shows its activity log.)"

// spawnAnswer is the result text of a spawn run whose turn text is final and
// whose streamed text blocks, in order, are blocks (none on the API path,
// whose answer arrives only in final). "" stays "": the caller reports an
// empty response.
func spawnAnswer(final string, blocks []string) string {
	stripped := platform.StripSilencingSuffix(final)
	if stripped == strings.TrimSpace(final) {
		return final // no marker at its end
	}
	if stripped == "" {
		return spawnNoAnswer
	}
	if n := len(blocks); n > 0 && platform.IsSilent(blocks[n-1]) {
		return stripped + "\n\n" + spawnSilentLastNote
	}
	return stripped
}

// answer is spawnAnswer for the run this sink saw. Read it only after the
// run's turn has completed.
func (s *spawnTextSink) answer() string {
	s.mu.Lock()
	blocks := append([]string(nil), s.blocks...)
	s.mu.Unlock()
	return spawnAnswer(s.FinalText(), blocks)
}

// adriftAnswer is answer for a run that ended without owning the spawn
// (releaseRun keeps it for a later result), except that a run with no answer
// gives "": it does not replace an answer an earlier such run kept.
func (s *spawnTextSink) adriftAnswer() string {
	if platform.IsSilent(s.FinalText()) {
		return ""
	}
	return s.answer()
}
