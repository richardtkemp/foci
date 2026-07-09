package askgw

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEncodeDecodeEnvelope(t *testing.T) {
	ask := &AskFrame{
		Protocol: ProtocolVersion,
		Type:     TypeAsk,
		ID:       "test-123",
		Questions: []AskQuestion{{
			Key:      "k1",
			Question: "Approve?",
			Options:  []AskOption{{Label: "Yes"}, {Label: "No"}},
		}},
	}
	b, err := Encode(ask)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(b), "\n") {
		t.Fatal("encoded frame must end with newline")
	}

	proto, typ, id, err := DecodeEnvelope(bytes(t, b))
	if err != nil {
		t.Fatal(err)
	}
	if proto != ProtocolVersion {
		t.Errorf("proto = %q, want %q", proto, ProtocolVersion)
	}
	if typ != TypeAsk {
		t.Errorf("type = %q, want %q", typ, TypeAsk)
	}
	if id != "test-123" {
		t.Errorf("id = %q, want test-123", id)
	}
}

func TestDecodeAsk(t *testing.T) {
	raw := `{"protocol":"askgw/1","type":"ask","id":"abc","questions":[{"key":"sudo","question":"Run?","options":[{"label":"Approve"},{"label":"Deny"}]}]}`
	ask, err := DecodeAsk([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := ask.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(ask.Questions) != 1 {
		t.Fatalf("questions = %d, want 1", len(ask.Questions))
	}
	if ask.Questions[0].Key != "sudo" {
		t.Errorf("key = %q, want sudo", ask.Questions[0].Key)
	}
}

func TestValidateRejections(t *testing.T) {
	cases := []struct {
		name string
		frame *AskFrame
		errSub string
	}{
		{"bad protocol", &AskFrame{Protocol: "askgw/2", Type: TypeAsk, ID: "x", Questions: []AskQuestion{{Key: "k", Question: "q?", Options: []AskOption{{Label: "a"}}}}}, "protocol"},
		{"wrong type", &AskFrame{Protocol: ProtocolVersion, Type: TypeAnswer, ID: "x", Questions: []AskQuestion{{Key: "k", Question: "q?", Options: []AskOption{{Label: "a"}}}}}, "ask frame"},
		{"missing id", &AskFrame{Protocol: ProtocolVersion, Type: TypeAsk, Questions: []AskQuestion{{Key: "k", Question: "q?", Options: []AskOption{{Label: "a"}}}}}, "id"},
		{"empty questions", &AskFrame{Protocol: ProtocolVersion, Type: TypeAsk, ID: "x"}, "non-empty"},
		{"missing key", &AskFrame{Protocol: ProtocolVersion, Type: TypeAsk, ID: "x", Questions: []AskQuestion{{Question: "q?", Options: []AskOption{{Label: "a"}}}}}, "key"},
		{"missing question text", &AskFrame{Protocol: ProtocolVersion, Type: TypeAsk, ID: "x", Questions: []AskQuestion{{Key: "k", Options: []AskOption{{Label: "a"}}}}}, "question"},
		{"empty options", &AskFrame{Protocol: ProtocolVersion, Type: TypeAsk, ID: "x", Questions: []AskQuestion{{Key: "k", Question: "q?"}}}, "option"},
		{"empty label", &AskFrame{Protocol: ProtocolVersion, Type: TypeAsk, ID: "x", Questions: []AskQuestion{{Key: "k", Question: "q?", Options: []AskOption{{}}}}}, "label"},
		{"colon in id", &AskFrame{Protocol: ProtocolVersion, Type: TypeAsk, ID: "a:b", Questions: []AskQuestion{{Key: "k", Question: "q?", Options: []AskOption{{Label: "a"}}}}}, "must not contain"},
		{"duplicate key", &AskFrame{Protocol: ProtocolVersion, Type: TypeAsk, ID: "x", Questions: []AskQuestion{{Key: "k", Question: "q?", Options: []AskOption{{Label: "a"}}}, {Key: "k", Question: "q2?", Options: []AskOption{{Label: "b"}}}}}, "duplicate"},
		{"duplicate label", &AskFrame{Protocol: ProtocolVersion, Type: TypeAsk, ID: "x", Questions: []AskQuestion{{Key: "k", Question: "q?", Options: []AskOption{{Label: "dup"}, {Label: "dup"}}}}}, "duplicate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.frame.Validate()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.errSub)
			}
		})
	}
}

func TestAnswerFrameSerialization(t *testing.T) {
	af := AnswerFrame{
		Protocol: ProtocolVersion,
		Type:     TypeAnswer,
		ID:       "abc",
		Status:   StatusAnswered,
		Answers:  map[string]json.RawMessage{"sudo": singleAnswer("Approve")},
	}
	b, err := Encode(af)
	if err != nil {
		t.Fatal(err)
	}
	var decoded AnswerFrame
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Status != StatusAnswered {
		t.Errorf("status = %q", decoded.Status)
	}
	var label string
	_ = json.Unmarshal(decoded.Answers["sudo"], &label)
	if label != "Approve" {
		t.Errorf("label = %q, want Approve", label)
	}
}

func bytes(t *testing.T, b []byte) []byte {
	return b[:len(b)-1]
}
