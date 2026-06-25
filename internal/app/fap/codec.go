package fap

import (
	"encoding/json"
	"fmt"
	"time"
)

// Encode serializes a ServerFrame into a complete wire string, stamping the
// envelope metadata around the type-specific payload. seq/ack are the
// reliability fields (0 = omitted); id defaults to a fresh ULID and ts to the
// current instant (RFC3339) when empty.
func Encode(frame ServerFrame, seq, ack int64, id, ts string) (string, error) {
	if id == "" {
		id = NewULID()
	}
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339Nano)
	}
	payload, err := json.Marshal(frame)
	if err != nil {
		return "", fmt.Errorf("fap: marshal payload %s: %w", frame.Type(), err)
	}
	env := Envelope{
		T:   frame.Type(),
		ID:  id,
		Seq: seq,
		Ack: ack,
		TS:  ts,
		V:   ProtocolVersion,
		D:   json.RawMessage(payload),
	}
	out, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("fap: marshal envelope %s: %w", frame.Type(), err)
	}
	return string(out), nil
}

// Inbound is a decoded app->server frame plus its envelope metadata. Frame is
// one of the concrete *Client* payload types, or nil for an unknown `t`
// (forward-compatibility — unknown inbound frames are ignored, not fatal).
type Inbound struct {
	T     string
	ID    string
	Seq   int64
	Ack   int64
	TS    string
	V     int
	Frame any
}

// Decode parses a wire string into envelope metadata + a typed client frame.
// Unknown envelope `t` values return an Inbound with a nil Frame and no error
// (the caller skips them). A malformed envelope or payload returns an error.
func Decode(text string) (Inbound, error) {
	var env Envelope
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		return Inbound{}, fmt.Errorf("fap: decode envelope: %w", err)
	}
	in := Inbound{T: env.T, ID: env.ID, Seq: env.Seq, Ack: env.Ack, TS: env.TS, V: env.V}
	frame, err := decodeClient(env.T, env.D)
	if err != nil {
		return Inbound{}, err
	}
	in.Frame = frame
	return in, nil
}

// decodeClient unmarshals the payload `d` into the concrete client frame for
// envelope type t. Returns (nil, nil) for an unknown/forward-compat type.
func decodeClient(t string, d json.RawMessage) (any, error) {
	// Payload-less frames: tolerate absent/empty `d`.
	switch t {
	case TypeConversationList:
		return ConversationList{}, nil
	case TypePing:
		return Ping{}, nil
	}

	var (
		dst any
	)
	switch t {
	case TypeHello:
		dst = &ClientHello{}
	case TypeMessage:
		dst = &ClientMessage{}
	case TypeCommand:
		dst = &Command{}
	case TypeInteractiveResponse:
		dst = &InteractiveResponse{}
	case TypeConversationOpen:
		dst = &ConversationOpen{}
	case TypeTyping:
		dst = &ClientTyping{}
	case TypeRead:
		dst = &Read{}
	default:
		return nil, nil // unknown inbound frame — ignored upstream
	}

	if len(d) == 0 {
		// Required payload absent — treat as the zero value rather than erroring,
		// matching the peer's default-filling decoder.
		return derefClient(dst), nil
	}
	if err := json.Unmarshal(d, dst); err != nil {
		return nil, fmt.Errorf("fap: decode %s payload: %w", t, err)
	}
	return derefClient(dst), nil
}

// derefClient returns the concrete value (not a pointer) for a decoded client
// frame, so callers type-switch on value types (fap.ClientMessage, not
// *fap.ClientMessage).
func derefClient(dst any) any {
	switch v := dst.(type) {
	case *ClientHello:
		return *v
	case *ClientMessage:
		return *v
	case *Command:
		return *v
	case *InteractiveResponse:
		return *v
	case *ConversationOpen:
		return *v
	case *ClientTyping:
		return *v
	case *Read:
		return *v
	default:
		return nil
	}
}
