package askgw

import (
	"encoding/json"
	"fmt"
	"strings"
)
import "foci/internal/log"

var (
	askgwLog = log.NewComponentLogger("askgw")
)

const ProtocolVersion = "askgw/1"

type Frame struct {
	Protocol string          `json:"protocol"`
	Type     string          `json:"type"`
	ID       string          `json:"id,omitempty"`
	Raw      json.RawMessage `json:"-"`
}

const (
	TypeAsk    = "ask"
	TypeAnswer = "answer"
	TypeNotify = "notify"
	TypeCancel = "cancel"
	TypeAck    = "ack"
	TypeError  = "error"
)

type AskQuestion struct {
	Key         string      `json:"key"`
	Header      string      `json:"header,omitempty"`
	Question    string      `json:"question"`
	MultiSelect bool        `json:"multiSelect,omitempty"`
	Options     []AskOption `json:"options"`
}

type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type AskFrame struct {
	Protocol       string        `json:"protocol"`
	Type           string        `json:"type"`
	ID             string        `json:"id"`
	Source         string        `json:"source,omitempty"`
	Title          string        `json:"title,omitempty"`
	Urgency        string        `json:"urgency,omitempty"`
	TimeoutSeconds float64       `json:"timeout_seconds,omitempty"`
	Questions      []AskQuestion `json:"questions"`
	Agent          string        `json:"agent,omitempty"`
}

const (
	StatusAnswered    = "answered"
	StatusTimeout     = "timeout"
	StatusDismissed   = "dismissed"
	StatusUnavailable = "unavailable"
)

type AnswerFrame struct {
	Protocol string                     `json:"protocol"`
	Type     string                     `json:"type"`
	ID       string                     `json:"id"`
	Status   string                     `json:"status"`
	Answers  map[string]json.RawMessage `json:"answers,omitempty"`
}

type CancelFrame struct {
	Protocol string `json:"protocol"`
	Type     string `json:"type"`
	ID       string `json:"id"`
	Reason   string `json:"reason,omitempty"`
}

// NotifyFrame carries an out-of-band update about a previously *answered*
// ask — typically a client (e.g. aisudo) reporting the completion status of
// whatever the human's answer authorized (a command, an approval, ...). ID
// correlates back to the original AskFrame.ID; there is no reply frame for a
// notify (see docs/ASKGW-PROTOCOL.md).
//
// ExitCode is a pointer so 0 (success) is distinguishable from "not
// supplied" (a notify with no numeric outcome, just Message/Status text).
type NotifyFrame struct {
	Protocol string `json:"protocol"`
	Type     string `json:"type"`
	ID       string `json:"id"`
	Status   string `json:"status,omitempty"`    // free-form outcome label, e.g. "completed", "failed"
	ExitCode *int   `json:"exit_code,omitempty"` // process exit code, when the notify concerns a command
	Message  string `json:"message,omitempty"`   // optional human-readable detail appended to the rendered status
}

type AckFrame struct {
	Protocol string `json:"protocol"`
	Type     string `json:"type"`
	ID       string `json:"id"`
}

type ErrorFrame struct {
	Protocol string `json:"protocol"`
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	Code     string `json:"code"`
	Message  string `json:"message,omitempty"`
}

func Encode(f any) ([]byte, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("askgw encode: %w", err)
	}
	return append(b, '\n'), nil
}

func DecodeEnvelope(b []byte) (Protocol, Type, ID string, err error) {
	var env struct {
		Protocol string `json:"protocol"`
		Type     string `json:"type"`
		ID       string `json:"id"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return "", "", "", fmt.Errorf("askgw decode envelope: %w", err)
	}
	return env.Protocol, env.Type, env.ID, nil
}

func DecodeAsk(b []byte) (*AskFrame, error) {
	var f AskFrame
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func DecodeCancel(b []byte) (*CancelFrame, error) {
	var f CancelFrame
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func DecodeNotify(b []byte) (*NotifyFrame, error) {
	var f NotifyFrame
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func (f *AskFrame) Validate() error {
	if f.Protocol != ProtocolVersion {
		return fmt.Errorf("protocol mismatch: got %q want %q", f.Protocol, ProtocolVersion)
	}
	if f.Type != TypeAsk {
		return fmt.Errorf("expected ask frame, got type %q", f.Type)
	}
	if f.ID == "" {
		return fmt.Errorf("ask frame missing id")
	}
	if strings.Contains(f.ID, ":") {
		return fmt.Errorf("ask frame id %q must not contain ':'", f.ID)
	}
	if len(f.Questions) == 0 {
		return fmt.Errorf("ask frame %q: questions must be non-empty", f.ID)
	}
	seenKeys := make(map[string]bool, len(f.Questions))
	for i, q := range f.Questions {
		if q.Key == "" {
			return fmt.Errorf("ask frame %q: question %d missing key", f.ID, i)
		}
		if seenKeys[q.Key] {
			return fmt.Errorf("ask frame %q: duplicate question key %q", f.ID, q.Key)
		}
		seenKeys[q.Key] = true
		if q.Question == "" {
			return fmt.Errorf("ask frame %q: question %d (%s) missing question text", f.ID, i, q.Key)
		}
		if len(q.Options) == 0 {
			return fmt.Errorf("ask frame %q: question %d (%s) must have at least one option", f.ID, i, q.Key)
		}
		seenLabels := make(map[string]bool, len(q.Options))
		for j, opt := range q.Options {
			if opt.Label == "" {
				return fmt.Errorf("ask frame %q: question %d (%s) option %d has empty label", f.ID, i, q.Key, j)
			}
			if seenLabels[opt.Label] {
				return fmt.Errorf("ask frame %q: question %d (%s) has duplicate option label %q", f.ID, i, q.Key, opt.Label)
			}
			seenLabels[opt.Label] = true
		}
	}
	return nil
}

func singleAnswer(label string) json.RawMessage {
	b, _ := json.Marshal(label)
	return b
}
