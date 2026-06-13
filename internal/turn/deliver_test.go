package turn

import (
	"errors"
	"strings"
	"testing"

	"foci/internal/log"
)

// fakeChunkWriter is a recording ChunkWriter for exercising the shared delivery
// loop in isolation. ComposeBody passes the payload text through as the body and
// reports a button only in compact thinking mode; Split chops the body on "|" so
// tests control the chunk count; send/edit/delete record their calls and return
// synthetic IDs. Configurable failure fields drive the loop's error/skip paths.
type fakeChunkWriter struct {
	log *log.ComponentLogger

	sent         []string // chunks sent via SendChunk (only when ok)
	sentButton   []string // chunks sent via SendChunkWithButton
	edited       []string // "id:chunk" pairs via EditChunk
	editedButton []string // "id:chunk" pairs via EditChunkWithButton
	deleted      []string // msgIDs deleted as orphans

	sendFail      map[string]bool // chunks for which SendChunk returns ok=false
	sendButtonErr error           // returned from SendChunkWithButton
	editErr       error           // returned from EditChunk (logged by loop)
	editButtonErr error           // returned from EditChunkWithButton
	nextSendID    int
}

func newFakeChunkWriter() *fakeChunkWriter {
	return &fakeChunkWriter{log: log.NewComponentLogger("test"), sendFail: map[string]bool{}}
}

func (f *fakeChunkWriter) ComposeBody(p Payload) (string, bool, string) {
	return p.Text, p.ThinkingMode == "compact", p.ThinkingText
}

func (f *fakeChunkWriter) Split(body string) []string {
	return strings.Split(body, "|")
}

func (f *fakeChunkWriter) SendChunk(chunk string) (string, bool) {
	if f.sendFail[chunk] {
		return "", false
	}
	f.sent = append(f.sent, chunk)
	f.nextSendID++
	return "new" + string(rune('0'+f.nextSendID)), true
}

func (f *fakeChunkWriter) SendChunkWithButton(chunk, thinkingText string) (string, error) {
	if f.sendButtonErr != nil {
		return "", f.sendButtonErr
	}
	f.sentButton = append(f.sentButton, chunk)
	f.nextSendID++
	return "btn" + string(rune('0'+f.nextSendID)), nil
}

func (f *fakeChunkWriter) EditChunk(msgID, chunk string) error {
	f.edited = append(f.edited, msgID+":"+chunk)
	return f.editErr
}

func (f *fakeChunkWriter) EditChunkWithButton(msgID, chunk, thinkingText string) error {
	if f.editButtonErr != nil {
		return f.editButtonErr
	}
	f.editedButton = append(f.editedButton, msgID+":"+chunk)
	return nil
}

func (f *fakeChunkWriter) DeleteMsg(msgID string)       { f.deleted = append(f.deleted, msgID) }
func (f *fakeChunkWriter) Logger() *log.ComponentLogger { return f.log }

func TestDeliverChunks_FreshSend(t *testing.T) {
	// No stream surfaced (nil): every chunk takes the send path, in order, and
	// the returned MsgIDs reflect the new messages.
	w := newFakeChunkWriter()
	res, err := DeliverChunks(w, Payload{Text: "a|b|c"}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Join(w.sent, ",") != "a,b,c" {
		t.Errorf("sent = %v, want [a b c]", w.sent)
	}
	if len(res.MsgIDs) != 3 {
		t.Errorf("MsgIDs = %v, want 3 new ids", res.MsgIDs)
	}
	if len(w.edited) != 0 || len(w.deleted) != 0 {
		t.Errorf("expected no edits/deletes, got edited=%v deleted=%v", w.edited, w.deleted)
	}
}

func TestDeliverChunks_FreshSend_ButtonOnLast(t *testing.T) {
	// Compact thinking → only the final chunk carries the button (SendChunkWithButton);
	// the earlier chunks go through plain SendChunk.
	w := newFakeChunkWriter()
	_, err := DeliverChunks(w, Payload{Text: "a|b", ThinkingMode: "compact", ThinkingText: "t"}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Join(w.sent, ",") != "a" {
		t.Errorf("plain sent = %v, want [a]", w.sent)
	}
	if strings.Join(w.sentButton, ",") != "b" {
		t.Errorf("button sent = %v, want [b]", w.sentButton)
	}
}

func TestDeliverChunks_FinalizeInPlace(t *testing.T) {
	// Stream surfaced with exactly as many messages as chunks: each chunk edits
	// the matching live message; nothing is sent or deleted.
	w := newFakeChunkWriter()
	stream := &mockSink{msgIDsRet: []string{"100", "101"}}
	res, err := DeliverChunks(w, Payload{Text: "x|y"}, stream)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Join(w.edited, ",") != "100:x,101:y" {
		t.Errorf("edited = %v, want [100:x 101:y]", w.edited)
	}
	if len(w.sent) != 0 || len(w.deleted) != 0 {
		t.Errorf("expected no sends/deletes, got sent=%v deleted=%v", w.sent, w.deleted)
	}
	if strings.Join(res.MsgIDs, ",") != "100,101" {
		t.Errorf("MsgIDs = %v, want [100 101]", res.MsgIDs)
	}
}

func TestDeliverChunks_AppendBeyondSequence(t *testing.T) {
	// Final longer than the live stream: existing positions are edited and the
	// extra chunks are appended via send.
	w := newFakeChunkWriter()
	stream := &mockSink{msgIDsRet: []string{"100"}}
	_, err := DeliverChunks(w, Payload{Text: "x|y|z"}, stream)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Join(w.edited, ",") != "100:x" {
		t.Errorf("edited = %v, want [100:x]", w.edited)
	}
	if strings.Join(w.sent, ",") != "y,z" {
		t.Errorf("sent = %v, want [y z]", w.sent)
	}
}

func TestDeliverChunks_DeletesOrphans(t *testing.T) {
	// Final shorter than the live stream: the leftover live messages beyond the
	// chunk count are deleted.
	w := newFakeChunkWriter()
	stream := &mockSink{msgIDsRet: []string{"100", "101", "102"}}
	_, err := DeliverChunks(w, Payload{Text: "only"}, stream)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Join(w.edited, ",") != "100:only" {
		t.Errorf("edited = %v, want [100:only]", w.edited)
	}
	if strings.Join(w.deleted, ",") != "101,102" {
		t.Errorf("deleted = %v, want [101 102]", w.deleted)
	}
}

func TestDeliverChunks_SendFailureSkipsID(t *testing.T) {
	// A SendChunk that reports ok=false contributes no ID and is not fatal.
	w := newFakeChunkWriter()
	w.sendFail["b"] = true
	res, err := DeliverChunks(w, Payload{Text: "a|b|c"}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Join(w.sent, ",") != "a,c" {
		t.Errorf("sent = %v, want [a c]", w.sent)
	}
	if len(res.MsgIDs) != 2 {
		t.Errorf("MsgIDs = %v, want 2 (failed send omitted)", res.MsgIDs)
	}
}

func TestDeliverChunks_SendButtonErrorPropagates(t *testing.T) {
	// A SendChunkWithButton error aborts delivery and returns the IDs used so far.
	w := newFakeChunkWriter()
	w.sendButtonErr = errors.New("boom")
	_, err := DeliverChunks(w, Payload{Text: "a|b", ThinkingMode: "compact"}, nil)
	if err == nil {
		t.Fatal("expected error from SendChunkWithButton")
	}
	if strings.Join(w.sent, ",") != "a" {
		t.Errorf("sent = %v, want [a] before the failing button send", w.sent)
	}
}

func TestDeliverChunks_EditErrorIsLoggedNotFatal(t *testing.T) {
	// A plain EditChunk error is logged but delivery continues and still records
	// the message ID as used.
	w := newFakeChunkWriter()
	w.editErr = errors.New("not modified")
	stream := &mockSink{msgIDsRet: []string{"100", "101"}}
	res, err := DeliverChunks(w, Payload{Text: "x|y"}, stream)
	if err != nil {
		t.Fatalf("edit error must not be fatal, got %v", err)
	}
	if strings.Join(res.MsgIDs, ",") != "100,101" {
		t.Errorf("MsgIDs = %v, want [100 101] despite edit errors", res.MsgIDs)
	}
}

func TestEditChunksInPlace_SingleChunk(t *testing.T) {
	// A body that fits one chunk edits the target message in place.
	w := newFakeChunkWriter()
	if err := EditChunksInPlace(w, "55", Payload{Text: "short"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Join(w.edited, ",") != "55:short" {
		t.Errorf("edited = %v, want [55:short]", w.edited)
	}
}

func TestEditChunksInPlace_ButtonChunk(t *testing.T) {
	// Compact thinking on a single chunk edits with the button variant.
	w := newFakeChunkWriter()
	if err := EditChunksInPlace(w, "55", Payload{Text: "short", ThinkingMode: "compact", ThinkingText: "t"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Join(w.editedButton, ",") != "55:short" {
		t.Errorf("editedButton = %v, want [55:short]", w.editedButton)
	}
}

func TestEditChunksInPlace_TooLong(t *testing.T) {
	// A body that splits into more than one chunk cannot replace one message:
	// ErrTooLongForEdit, with no edit attempted.
	w := newFakeChunkWriter()
	err := EditChunksInPlace(w, "55", Payload{Text: "a|b"})
	if !errors.Is(err, ErrTooLongForEdit) {
		t.Fatalf("err = %v, want ErrTooLongForEdit", err)
	}
	if len(w.edited) != 0 || len(w.editedButton) != 0 {
		t.Errorf("expected no edit attempt, got edited=%v button=%v", w.edited, w.editedButton)
	}
}
