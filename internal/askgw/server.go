package askgw

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"foci/internal/peercred"
	"foci/internal/question"
)

// PresentFn presents a question to the human. Returns the platform-native
// message ID of the posted message (empty if unknown, e.g. a plain-text
// fallback send) and whether presentation succeeded — the platformMsgID is
// retained (see Registry.SetPresentedMsgID) so a later `notify` frame can
// edit the same message in place once the ask is answered.
type PresentFn func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) (platformMsgID string, ok bool)

type CancelFn func(msgID, finalText string)

type ResolveSessionFn func(frameAgent string) (agentID, sessionKey string)

// EditFn edits an already-sent chat message in place — used to render a
// `notify` frame onto the answered ask's own message. Returns false if the
// edit could not be performed (no live connection, platform doesn't support
// editing that message, ...), signalling the caller to fall back to
// NotifyFn instead.
type EditFn func(agentID, sessionKey, msgID, text string) bool

// NotifyFn posts a standalone chat message — the fallback used to render a
// `notify` frame when EditFn is unavailable or fails (e.g. the original
// message's platform ID was never captured).
type NotifyFn func(agentID, sessionKey, text string)

type Server struct {
	socketPath     string
	allowedUIDs    map[uint32]bool
	maxFrameBytes  int
	defaultTimeout time.Duration
	group          string

	registry *Registry

	present        PresentFn
	cancelPrompt   CancelFn
	resolveSession ResolveSessionFn
	editMessage    EditFn
	notifyFallback NotifyFn

	listener net.Listener
	closeMu  sync.Mutex
	closed   bool
}

type ServerDeps struct {
	SocketPath     string
	AllowedUIDs    []string
	MaxFrameBytes  int
	DefaultTimeout time.Duration
	Group          string

	Present        PresentFn
	CancelPrompt   CancelFn
	ResolveSession ResolveSessionFn

	// EditMessage / NotifyFallback render a `notify` frame (see
	// handleNotify). Both may be nil (notify then becomes a no-op beyond
	// logging) — callers that don't care about notify rendering, e.g. a
	// minimal test harness, aren't required to wire them.
	EditMessage    EditFn
	NotifyFallback NotifyFn
}

func NewServer(deps ServerDeps) (*Server, error) {
	uidSet := make(map[uint32]bool, len(deps.AllowedUIDs))
	for _, s := range deps.AllowedUIDs {
		uid, err := resolveUID(s)
		if err != nil {
			return nil, fmt.Errorf("askgw: resolve allowed_uid %q: %w", s, err)
		}
		uidSet[uid] = true
	}
	maxBytes := deps.MaxFrameBytes
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	return &Server{
		socketPath:     deps.SocketPath,
		allowedUIDs:    uidSet,
		maxFrameBytes:  maxBytes,
		defaultTimeout: deps.DefaultTimeout,
		group:          deps.Group,
		registry:       NewRegistry(),
		present:        deps.Present,
		cancelPrompt:   deps.CancelPrompt,
		resolveSession: deps.ResolveSession,
		editMessage:    deps.EditMessage,
		notifyFallback: deps.NotifyFallback,
	}, nil
}

func (s *Server) Start() error {
	if err := s.checkParentDir(); err != nil {
		return fmt.Errorf("askgw: %w", err)
	}
	if err := s.removeStaleSocket(); err != nil {
		return fmt.Errorf("askgw: remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("askgw: listen %s: %w", s.socketPath, err)
	}
	if s.group != "" {
		if err := s.setGroup(s.group); err != nil {
			_ = ln.Close()
			_ = os.Remove(s.socketPath)
			return fmt.Errorf("askgw: set group: %w", err)
		}
	}
	if err := os.Chmod(s.socketPath, 0660); err != nil { //nolint:gosec // G302: intentional — socket must be group-accessible
		_ = ln.Close()
		_ = os.Remove(s.socketPath)
		return fmt.Errorf("askgw: chmod socket: %w", err)
	}
	s.listener = ln
	go s.acceptLoop()
	uids := make([]uint32, 0, len(s.allowedUIDs))
	for uid := range s.allowedUIDs {
		uids = append(uids, uid)
	}
	askgwLog.Infof("listening on %s (allowed UIDs: %v)", s.socketPath, uids)
	return nil
}

func (s *Server) Close() {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.listener != nil {
		_ = s.listener.Close()
	}
	_ = os.Remove(s.socketPath)
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.closeMu.Lock()
			closed := s.closed
			s.closeMu.Unlock()
			if closed {
				return
			}
			askgwLog.Errorf("accept error: %v", err)
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return
	}
	uid, err := peercred.UID(uc)
	if err != nil {
		askgwLog.Warnf("peercred error: %v", err)
		return
	}
	if !s.allowedUIDs[uid] {
		askgwLog.Warnf("rejecting connection from uid %d (not in allow-list)", uid)
		return
	}

	connID := s.registry.RegisterConn()
	defer func() {
		for _, e := range s.registry.UnregisterConn(connID) {
			if e.cancelFn != nil {
				e.cancelFn()
			}
		}
	}()

	cw := &syncConnWriter{conn: conn}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, s.maxFrameBytes), s.maxFrameBytes)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := s.handleFrame(connID, cw, line); err != nil {
			askgwLog.Warnf("conn=%d: %v", connID, err)
			if ef, ok := err.(*frameError); ok {
				_ = cw.WriteFrame(ErrorFrame{
					Protocol: ProtocolVersion,
					Type:     TypeError,
					ID:       ef.id,
					Code:     ef.code,
					Message:  ef.message,
				})
				if ef.fatal {
					return
				}
			}
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		if errors.Is(err, bufio.ErrTooLong) {
			_ = cw.WriteFrame(ErrorFrame{
				Protocol: ProtocolVersion,
				Type:     TypeError,
				Code:     "too_large",
				Message:  fmt.Sprintf("frame exceeds %d bytes", s.maxFrameBytes),
			})
		}
		askgwLog.Debugf("conn=%d scanner: %v", connID, err)
	}
}

type frameError struct {
	id      string
	code    string
	message string
	fatal   bool
}

func (e *frameError) Error() string { return e.message }

func errFrame(id, code, msg string) *frameError {
	return &frameError{id: id, code: code, message: msg}
}

func (e *frameError) Fatal() *frameError { e.fatal = true; return e }

func (s *Server) handleFrame(connID uint64, cw connWriter, line []byte) error {
	proto, typ, id, err := DecodeEnvelope(line)
	if err != nil {
		return errFrame("", "malformed", err.Error()).Fatal()
	}
	if proto != ProtocolVersion {
		return errFrame(id, "bad_protocol", fmt.Sprintf("protocol %q; want %q", proto, ProtocolVersion)).Fatal()
	}

	switch typ {
	case TypeAsk:
		return s.handleAsk(connID, cw, id, line)
	case TypeCancel:
		return s.handleCancel(connID, id, line)
	case TypeNotify:
		return s.handleNotify(id, line)
	case TypeError:
		return nil
	default:
		return errFrame(id, "unknown_type", fmt.Sprintf("unexpected frame type %q", typ))
	}
}

func (s *Server) handleAsk(connID uint64, cw connWriter, id string, line []byte) error {
	ask, err := DecodeAsk(line)
	if err != nil {
		return errFrame(id, "malformed", err.Error())
	}
	if err := ask.Validate(); err != nil {
		return errFrame(id, "malformed", err.Error())
	}

	agentID, sessionKey := s.resolveSession(ask.Agent)
	if sessionKey == "" {
		s.registry.ResolveUnavailable(cw, id)
		return nil
	}

	qs := askFrameToQuestions(ask)
	firstIdx := 0
	msgID := askgwMsgID(id, firstIdx)
	e, err := s.registry.Add(connID, id, agentID, sessionKey, cw, qs, msgID, s.frameTimeout(ask), func() {
		s.cancelPrompt(askgwMsgID(id, firstIdx), "⌛ Cancelled by askgw")
	})
	if err != nil {
		return errFrame(id, "rejected", err.Error())
	}

	ok := s.presentQuestion(e, connID, id, firstIdx)
	if !ok {
		s.registry.Remove(connID, id)
		s.registry.ResolveUnavailable(cw, id)
		return nil
	}

	_ = cw.WriteFrame(AckFrame{
		Protocol: ProtocolVersion,
		Type:     TypeAck,
		ID:       id,
	})
	return nil
}

func (s *Server) presentQuestion(e *entry, connID uint64, askID string, idx int) bool {
	q := &e.questions[idx]
	choices := buildChoices(q)
	msgID := askgwMsgID(askID, idx)
	text := question.FormatText(questionFromAsk(q), idx, len(e.questions))
	summary := q.Header
	if summary == "" {
		summary = "Question"
	}

	platformMsgID, ok := s.present(e.agentID, e.sessionKey, msgID, text, summary, choices, func(data string) {
		s.onAnswer(connID, askID, data)
	})
	if ok {
		s.registry.SetPresentedMsgID(connID, askID, platformMsgID)
	}
	return ok
}

func (s *Server) onAnswer(connID uint64, askID, data string) {
	e := s.registry.Get(connID, askID)
	if e == nil {
		return
	}
	q := e.currentQuestion()
	if q == nil {
		return
	}

	label, cancelled, err := question.ResolveAnswer(questionFromAsk(q), data)
	if err != nil {
		askgwLog.Warnf("resolve conn=%d id=%s: %v", connID, askID, err)
		return
	}
	if cancelled {
		s.registry.resolveDismissed(connID, askID)
		return
	}

	done := s.registry.recordAnswer(connID, askID, q.Key, singleAnswer(label))
	if done {
		s.registry.sendAnswer(connID, askID)
		return
	}

	e2 := s.registry.Get(connID, askID)
	if e2 == nil {
		return
	}
	nextIdx := e2.current
	_ = s.presentQuestion(e2, connID, askID, nextIdx)
}

func (s *Server) handleCancel(connID uint64, id string, line []byte) error {
	cancel, err := DecodeCancel(line)
	if err != nil {
		return errFrame(id, "malformed", err.Error())
	}
	e := s.registry.Get(connID, cancel.ID)
	if e != nil {
		cancelMsgID := askgwMsgID(cancel.ID, e.current)
		s.registry.Cancel(connID, cancel.ID)
		s.cancelPrompt(cancelMsgID, "❌ Cancelled by App")
	}
	return nil
}

// handleNotify decodes an inbound `notify` frame and renders it. Unlike ask/
// cancel there is no reply frame — notify is fire-and-forget from the
// client's perspective (see docs/ASKGW-PROTOCOL.md).
func (s *Server) handleNotify(id string, line []byte) error {
	nf, err := DecodeNotify(line)
	if err != nil {
		return errFrame(id, "malformed", err.Error())
	}
	if nf.ID == "" {
		return errFrame(id, "malformed", "notify frame missing id")
	}
	s.renderNotify(nf)
	return nil
}

// HandleNotifyFrame is handleNotify's entry point for transports that have
// no connWriter/connID of their own — currently the HTTP `POST
// /askgw/notify` endpoint (cmd/foci-gw/askgw_http.go). It validates the
// envelope itself (handleFrame normally does this) since callers here never
// go through handleFrame's dispatch.
func (s *Server) HandleNotifyFrame(body []byte) (id string, ok bool, code, msg string) {
	proto, typ, envID, err := DecodeEnvelope(body)
	if err != nil {
		return "", false, "malformed", err.Error()
	}
	if proto != ProtocolVersion {
		return envID, false, "bad_protocol", fmt.Sprintf("protocol %q; want %q", proto, ProtocolVersion)
	}
	if typ != TypeNotify {
		return envID, false, "unknown_type", fmt.Sprintf("expected type %q, got %q", TypeNotify, typ)
	}
	if err := s.handleNotify(envID, body); err != nil {
		if fe, ok2 := err.(*frameError); ok2 {
			return envID, false, fe.code, fe.message
		}
		return envID, false, "error", err.Error()
	}
	return envID, true, "", ""
}

// renderNotify looks up the answered ask nf.ID refers to and delivers it:
// preferentially by editing that ask's own chat message in place (so the
// human sees the outcome right where they made the decision), falling back
// to a standalone message when editing isn't available or fails. A nil
// lookup (unknown/expired id) is logged and dropped — there is nowhere to
// render it and no reply frame to report the failure through.
func (s *Server) renderNotify(nf *NotifyFrame) {
	info := s.registry.getAnswered(nf.ID)
	if info == nil {
		askgwLog.Debugf("notify for unknown or expired ask id=%s", nf.ID)
		return
	}
	text := formatNotifyText(nf)
	if info.msgID != "" && s.editMessage != nil && s.editMessage(info.agentID, info.sessionKey, info.msgID, text) {
		return
	}
	if s.notifyFallback != nil {
		s.notifyFallback(info.agentID, info.sessionKey, text)
	}
}

// formatNotifyText renders a NotifyFrame into the text shown in chat: a
// checkmark/cross plus "completed, exit N" when an exit code is present
// (the common case — aisudo's update_completion_status notify), else a
// generic status line, plus any free-form Message appended on its own line.
func formatNotifyText(nf *NotifyFrame) string {
	var status string
	switch {
	case nf.ExitCode != nil:
		icon := "✅"
		if *nf.ExitCode != 0 {
			icon = "❌"
		}
		status = fmt.Sprintf("%s completed, exit %d", icon, *nf.ExitCode)
	case nf.Status != "":
		status = "✅ " + nf.Status
	default:
		status = "✅ completed"
	}
	if nf.Message != "" {
		status += "\n" + nf.Message
	}
	return status
}

func (s *Server) frameTimeout(ask *AskFrame) time.Duration {
	if ask.TimeoutSeconds > 0 {
		return time.Duration(ask.TimeoutSeconds * float64(time.Second))
	}
	return s.defaultTimeout
}

func (s *Server) checkParentDir() error {
	parent := filepath.Dir(s.socketPath)
	fi, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("stat parent dir %s: %w", parent, err)
	}
	if fi.Mode()&0o022 != 0 {
		return fmt.Errorf("parent dir %s is group- or world-writable (mode %o)", parent, fi.Mode().Perm())
	}
	return nil
}

func (s *Server) removeStaleSocket() error {
	fi, err := os.Lstat(s.socketPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return &os.PathError{Op: "remove", Path: s.socketPath, Err: os.ErrExist}
	}
	return os.Remove(s.socketPath)
}

func (s *Server) setGroup(name string) error {
	gid, err := groupGID(name)
	if err != nil {
		askgwLog.Warnf("group %q not found: %v (socket keeps default group)", name, err)
		return nil
	}
	return os.Chown(s.socketPath, -1, gid)
}

func askgwMsgID(askID string, idx int) string {
	return "askgw-" + askID + "-q" + strconv.Itoa(idx)
}

func askFrameToQuestions(ask *AskFrame) []AskQuestion {
	return ask.Questions
}

func questionFromAsk(q *AskQuestion) *question.Question {
	return &question.Question{
		Question:    q.Question,
		Header:      q.Header,
		MultiSelect: q.MultiSelect,
		Options:     askOptionsToQuestionOptions(q.Options),
	}
}

func askOptionsToQuestionOptions(opts []AskOption) []question.Option {
	out := make([]question.Option, len(opts))
	for i, o := range opts {
		out[i] = question.Option{Label: o.Label, Description: o.Description}
	}
	return out
}

func buildChoices(q *AskQuestion) []question.Choice {
	qq := questionFromAsk(q)
	return question.Choices(qq)
}

type syncConnWriter struct {
	conn net.Conn
	mu   sync.Mutex
}

func (w *syncConnWriter) WriteFrame(v any) error {
	b, err := Encode(v)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.conn.Write(b)
	return err
}

func (w *syncConnWriter) Close() error {
	return w.conn.Close()
}
