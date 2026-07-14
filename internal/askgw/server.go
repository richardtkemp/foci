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

type PresentFn func(agentID, sessionKey, msgID, text, summary string, choices []question.Choice, onResponse func(data string)) bool

type CancelFn func(msgID, finalText string)

type ResolveSessionFn func(frameAgent string) (agentID, sessionKey string)

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
	case TypeNotify, TypeError:
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

	return s.present(e.agentID, e.sessionKey, msgID, text, summary, choices, func(data string) {
		s.onAnswer(connID, askID, data)
	})
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
