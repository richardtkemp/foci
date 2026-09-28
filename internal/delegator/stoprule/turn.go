package stoprule

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"unicode/utf8"
)

// ErrTurnNotFound means the transcript has no user entry carrying the Stop
// payload's prompt_id, so the turn's extent is unknown. Callers fail open.
var ErrTurnNotFound = errors.New("turn start not found in transcript")

// transcriptEntry is the part of a CC transcript JSONL line the scan reads.
// Shapes verified live on CC 2.1.280 (#2089): a turn's user entries (the
// prompt, its tool_results and any Stop-hook feedback) carry the Stop
// payload's prompt_id as promptId; assistant entries carry none.
type transcriptEntry struct {
	Type        string `json:"type"`
	PromptID    string `json:"promptId"`
	IsSidechain bool   `json:"isSidechain"`
	Message     struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
}

// backgroundResult is the toolUseResult of a call that started background
// work. Verified live on CC 2.1.280: a run_in_background Bash result has
// backgroundTaskId; an Agent call that backgrounded (the Agent tool's
// default, with no run_in_background in its input) has
// status "async_launched" and isAsync true.
type backgroundResult struct {
	BackgroundTaskID string `json:"backgroundTaskId"`
	Status           string `json:"status"`
	IsAsync          bool   `json:"isAsync"`
}

func (r backgroundResult) launched() bool {
	return r.BackgroundTaskID != "" || r.IsAsync || r.Status == "async_launched"
}

// TurnLaunches reads a CC transcript and returns the tool_use ids of the
// background jobs launched in the turn that began with promptID: a call
// whose input sets run_in_background, or whose result reports background
// work. It returns ErrTurnNotFound if no user entry carries promptID.
func TurnLaunches(r io.Reader, promptID string) ([]string, error) {
	if promptID == "" {
		return nil, ErrTurnNotFound
	}
	pid := []byte(promptID)
	br := bufio.NewReader(r)
	inTurn := false
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			// Lines before the turn only matter if they may be its start; the
			// transcript also holds multi-KB attachment lines not worth decoding.
			if inTurn || bytes.Contains(line, pid) {
				var e transcriptEntry
				if json.Unmarshal(line, &e) == nil && !e.IsSidechain {
					if !inTurn && e.Type == "user" && e.PromptID == promptID {
						inTurn = true
					}
					if inTurn {
						scanEntry(e, add)
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if !inTurn {
		return nil, ErrTurnNotFound
	}
	return ids, nil
}

func scanEntry(e transcriptEntry, add func(string)) {
	var blocks []contentBlock
	if json.Unmarshal(e.Message.Content, &blocks) != nil {
		return // string content: a prompt, no tool calls
	}
	switch e.Type {
	case "assistant":
		for _, b := range blocks {
			if b.Type != "tool_use" {
				continue
			}
			var in struct {
				RunInBackground bool `json:"run_in_background"`
			}
			if json.Unmarshal(b.Input, &in) == nil && in.RunInBackground {
				add(b.ID)
			}
		}
	case "user":
		var res backgroundResult
		if len(e.ToolUseResult) == 0 || json.Unmarshal(e.ToolUseResult, &res) != nil || !res.launched() {
			return
		}
		for _, b := range blocks {
			if b.Type == "tool_result" {
				add(b.ToolUseID)
			}
		}
	}
}

// Input is what the Stop hook knows when it evaluates.
type Input struct {
	// Text is the turn's final assistant text (the payload's
	// last_assistant_message).
	Text           string
	StopHookActive bool
	TranscriptPath string
	PromptID       string
}

// Result values for Verdict.Result, as logged.
const (
	ResultPass         = "pass"          // no rule matched
	ResultFire         = "fire"          // matched, nothing launched: block
	ResultPassLaunched = "pass_launched" // matched, but the turn launched background work
	ResultPassActive   = "pass_active"   // stop_hook_active: this turn was already blocked once
	ResultPassError    = "pass_error"    // matched, but the turn could not be read: fail open
)

// Verdict is Evaluate's decision.
type Verdict struct {
	Result string
	// Rule names the matching rule; empty on ResultPass.
	Rule string
	// Excerpt is the matched text, or on a pass the tail of the final text.
	Excerpt  string
	Launches int
	// Reason is the block reason; set only on ResultFire.
	Reason string
	// Err explains ResultPassError.
	Err string
}

// Block reports whether the stop must be blocked.
func (v Verdict) Block() bool { return v.Result == ResultFire }

// maxExcerpt caps the text a verdict carries for the log.
const maxExcerpt = 160

// Evaluate decides one Stop. The transcript is read only when a rule
// matches, so a turn that says nothing announcing costs no file read.
func Evaluate(rules []Rule, in Input) Verdict {
	hit := Match(rules, in.Text)
	if hit == nil {
		return Verdict{Result: ResultPass, Excerpt: tail(in.Text, maxExcerpt)}
	}
	v := Verdict{Rule: hit.Rule.Name, Excerpt: Excerpt(hit.Match, maxExcerpt)}
	if in.StopHookActive {
		v.Result = ResultPassActive
		return v
	}
	ids, err := readLaunches(in.TranscriptPath, in.PromptID)
	if err != nil {
		v.Result, v.Err = ResultPassError, err.Error()
		return v
	}
	v.Launches = len(ids)
	if len(ids) > 0 {
		v.Result = ResultPassLaunched
		return v
	}
	v.Result, v.Reason = ResultFire, hit.ReasonFor()
	return v
}

func readLaunches(path, promptID string) ([]string, error) {
	if path == "" {
		return nil, errors.New("no transcript_path in the Stop payload")
	}
	f, err := os.Open(path) // #nosec G304 -- CC's own transcript path from the hook payload
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return TurnLaunches(f, promptID)
}

// tail returns the last max bytes of s with whitespace collapsed, marking a
// cut at the front.
func tail(s string, max int) string {
	s = Excerpt(s, len(s))
	if len(s) <= max {
		return s
	}
	start := len(s) - max
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return "…" + s[start:]
}
