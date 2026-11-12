// Package configapprove runs agent-requested config changes past the user
// (#479). An agent cannot write foci.toml: the file is root:foci-secrets 0664
// and only the gateway holds foci-secrets. It asks instead — `foci config set
// <path> <value>` or `/config set` over HTTP — and the gateway shows the
// owner an Approve/Deny prompt with the path, the old value and the new one.
// Nothing is written unless the user approves.
//
// A request is validated BEFORE any prompt and again before any write: the
// edit must change the file only at the requested path (config.PlanSetPath /
// PlanUnsetPath, verified by config.VerifyPathEdit) and the result must pass
// the same check as `foci check config` (configcheck.CheckContentAllowUnknown),
// except that a key this binary does not know does not fail it (#3046): the
// prompt warns about each unknown key the change writes, and the user
// decides (a typo, or a key for a later version written ahead of a deploy).
// On Approve the file is re-read: if the value at the path is no longer the
// old value shown, or any check fails, nothing is written.
//
// Requests live in memory only: after a gateway restart a waiting client
// gets "unknown request" and reports the request failed.
package configapprove

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/log"
)

var logger = log.NewComponentLogger("configapprove")

// Op is what a request does to the path.
type Op string

// The operations. A path may end in an element selector (arr[name],
// arr[3], arr[field=value], #2938): set then replaces that one element and
// unset removes it. add appends one element to the array the path names.
const (
	OpSet   Op = "set"
	OpUnset Op = "unset"
	OpAdd   Op = "add"
)

// Outcome is where a request stands.
type Outcome string

// Outcomes. Every request ends in exactly one of the last four.
const (
	Pending  Outcome = "pending"
	Applied  Outcome = "applied"
	Denied   Outcome = "denied"
	TimedOut Outcome = "timed_out"
	Failed   Outcome = "failed"
)

// Timeout bounds for one request's wait for the user's answer. The CLI may
// ask for any wait within them; DefaultTimeout applies when it asks for none.
const (
	DefaultTimeout = 10 * time.Minute
	MaxTimeout     = time.Hour
	MinTimeout     = 10 * time.Second
)

// retainResolved is how long a resolved request stays answerable, so a
// client polling between long-poll calls still reads its outcome.
const retainResolved = 15 * time.Minute

// Status is a request's state as the HTTP API returns it.
type Status struct {
	ID      string  `json:"id,omitempty"`
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason,omitempty"`
	Path    string  `json:"path,omitempty"`
	Old     string  `json:"old,omitempty"`
	New     string  `json:"new,omitempty"`
	// Warning names the keys the change writes that this binary does not
	// know (#3046); the approval prompt shows the same text. Empty when none.
	Warning string `json:"warning,omitempty"`
}

// Prompt is what the presenter shows the user.
type Prompt struct {
	ID         string
	AgentID    string // the requesting agent: the prompt goes to its owner's default chat
	SessionKey string // the requesting session, named in Text
	Summary    string
	Text       string
	// OnAnswer is called once with the user's choice; it returns the text
	// the prompt is edited to.
	OnAnswer func(approve bool) string
	// OnExpire is called if the platform expires the prompt unanswered.
	OnExpire func()
}

// Deps is the service's outside world.
type Deps struct {
	ConfigPath string
	FileMode   os.FileMode // only for a config file that does not exist (never, in practice)
	// CheckProtected judges the file's ownership before a write; nil skips it
	// (skip_security_checks, or a platform without the protection model).
	CheckProtected func(path string) error
	// CheckContent is the foci check config verdict on candidate content,
	// with unknown keys returned instead of failed
	// (configcheck.CheckContentAllowUnknown). unknown is read even with err.
	CheckContent func(content, path string) (unknown []string, err error)
	// Present shows the prompt; an error means no chat could show it.
	Present func(p Prompt) error
	// Close edits a shown prompt to finalText and removes its buttons.
	Close func(id, finalText string)
	// Apply live-applies the written path where an applier exists.
	Apply func(p config.KeyPath)
	// Now and AfterFunc are the clock, injectable for tests.
	Now       func() time.Time
	AfterFunc func(d time.Duration, f func()) (stop func() bool)
}

// Submission is one request.
type Submission struct {
	AgentID    string
	SessionKey string
	Op         Op
	Path       string
	Value      string // TOML value text; unused for OpUnset
	Timeout    time.Duration
}

type request struct {
	Submission
	path     config.KeyPath
	id       string
	oldValue any
	oldSet   bool
	status   Status
	done     chan struct{}
	stop     func() bool
	resolved time.Time
}

// Service holds the pending requests.
type Service struct {
	d Deps

	mu   sync.Mutex
	reqs map[string]*request

	// writeMu serialises the re-read, check and write of an approval.
	writeMu sync.Mutex
}

// New returns a Service.
func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.AfterFunc == nil {
		d.AfterFunc = func(dur time.Duration, f func()) func() bool { return time.AfterFunc(dur, f).Stop }
	}
	return &Service{d: d, reqs: map[string]*request{}}
}

// ClampTimeout bounds a requested wait: zero means DefaultTimeout.
func ClampTimeout(t time.Duration) time.Duration {
	switch {
	case t <= 0:
		return DefaultTimeout
	case t < MinTimeout:
		return MinTimeout
	case t > MaxTimeout:
		return MaxTimeout
	}
	return t
}

// plan computes the edited content from cur.
func plan(cur string, p config.KeyPath, op Op, value string) (string, error) {
	switch op {
	case OpUnset:
		return config.PlanUnsetPath(cur, p)
	case OpAdd:
		return config.PlanAddElement(cur, p, value)
	}
	return config.PlanSetPath(cur, p, value)
}

// validate plans the edit on the current file and runs every check that
// does not need the user. It returns the current and planned content, the
// old value and the unknown keys the change writes (for the warning).
func (s *Service) validate(sub Submission, p config.KeyPath) (cur, next string, old any, oldSet bool, unknown []string, err error) {
	data, err := os.ReadFile(s.d.ConfigPath)
	if err != nil {
		return "", "", nil, false, nil, fmt.Errorf("read config: %w", err)
	}
	cur = string(data)
	if sub.Op == OpAdd && p.Elem != nil {
		return "", "", nil, false, nil, fmt.Errorf("add appends to an array: name the array without a selector (%s), not %s", p.Array(), p)
	}
	old, oldSet, err = lookupOld(cur, p, sub.Op)
	if err != nil {
		return "", "", nil, false, nil, err
	}
	next, err = plan(cur, p, sub.Op, sub.Value)
	if err != nil {
		return "", "", nil, false, nil, err
	}
	if s.d.CheckContent != nil {
		nextUnknown, err := s.d.CheckContent(next, s.d.ConfigPath)
		if err != nil {
			return "", "", nil, false, nil, fmt.Errorf("the changed config would not pass the config check: %w", err)
		}
		if len(nextUnknown) > 0 {
			// The current file's unknown keys tell a key this change writes
			// from one already there; its own verdict does not matter.
			curUnknown, _ := s.d.CheckContent(cur, s.d.ConfigPath)
			unknown = unknownWritten(p, curUnknown, nextUnknown)
		}
	}
	return cur, next, old, oldSet, unknown, nil
}

// unknownWritten is the unknown keys of the changed file that the change
// writes: a key at, under or above the requested path (an unknown key set
// again is still warned about), or one the current file does not have (an
// added element or table value carrying an unknown field). Keys are the
// decoder's dotted form, where an array element's selector is absent
// (agents[main].model is agents.model), so a key of another element of the
// same array also counts: an over-warning, never a missed one. Unknown keys
// already in the file elsewhere are not this change's and are not named.
func unknownWritten(p config.KeyPath, curUnknown, nextUnknown []string) []string {
	var segs []string
	if p.AgentID != "" {
		segs = append(segs, "agents")
	}
	segs = append(segs, p.Keys...)
	segs = append(segs, p.Sub...)
	rel := strings.Join(segs, ".")
	had := make(map[string]bool, len(curUnknown))
	for _, k := range curUnknown {
		had[k] = true
	}
	var out []string
	for _, k := range nextUnknown {
		if !had[k] || k == rel || strings.HasPrefix(k, rel+".") || strings.HasPrefix(rel, k+".") {
			out = append(out, k)
		}
	}
	return out
}

// unknownWarning is the prompt's warning for keys this binary does not know.
func unknownWarning(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	what := "this key"
	if len(keys) > 1 {
		what = "these keys"
	}
	return fmt.Sprintf("⚠️ This foci binary does not know %s: %s. A typo, or a key from a later version? If you approve, it is written, and this binary ignores it until a version that knows it runs.",
		what, strings.Join(keys, ", "))
}

// lookupOld is the value the request changes, as the prompt shows it and the
// approval re-checks it: the key's value, or the one element a selector
// names. An add changes no existing value, so it has none.
func lookupOld(cur string, p config.KeyPath, op Op) (any, bool, error) {
	if op == OpAdd {
		return nil, false, nil
	}
	return config.LookupPath(cur, p)
}

// droppedNote names the comments an edit removes, so none goes silently.
func droppedNote(cur, next string) string {
	gone := config.DroppedComments(cur, next)
	if len(gone) == 0 {
		return ""
	}
	return "\n(also removes the comment: " + strings.Join(gone, " / ") + ")"
}

// Submit validates a request and, when it passes, shows the approval prompt
// and returns its pending status. A request that fails validation returns
// Failed at once with the reason, and no prompt is shown.
func (s *Service) Submit(sub Submission) Status {
	s.prune()
	if sub.Op != OpSet && sub.Op != OpUnset && sub.Op != OpAdd {
		return Status{Outcome: Failed, Reason: fmt.Sprintf("unknown operation %q (want set, unset or add)", sub.Op)}
	}
	p, err := config.ParseKeyPath(sub.Path)
	if err != nil {
		return Status{Outcome: Failed, Reason: err.Error()}
	}
	cur, next, old, oldSet, unknown, err := s.validate(sub, p)
	if err != nil {
		return Status{Outcome: Failed, Path: p.String(), Reason: err.Error()}
	}
	newText := "(unset)"
	if p.Elem != nil && len(p.Sub) == 0 {
		newText = "(element removed)"
	}
	if sub.Op != OpUnset {
		v, _ := config.DecodeTOMLValue(sub.Value) // validate already decoded it
		newText = config.RenderTOMLValue(v)
	}
	oldText := "(unset)"
	switch {
	case sub.Op == OpAdd:
		oldText = "(none: a new element is appended)"
	case oldSet:
		oldText = config.RenderTOMLValue(old)
	}
	newText += droppedNote(cur, next)
	shownPath := p.String()
	if sub.Op == OpAdd {
		shownPath += " (add element)"
	}

	r := &request{
		Submission: sub,
		path:       p,
		id:         newID(),
		oldValue:   old,
		oldSet:     oldSet,
		done:       make(chan struct{}),
	}
	warning := unknownWarning(unknown)
	r.status = Status{ID: r.id, Outcome: Pending, Path: shownPath, Old: oldText, New: newText, Warning: warning}
	s.mu.Lock()
	s.reqs[r.id] = r
	s.mu.Unlock()

	err = s.d.Present(Prompt{
		ID:         r.id,
		AgentID:    sub.AgentID,
		SessionKey: sub.SessionKey,
		Summary:    "config change " + shownPath,
		Text:       promptText(sub.SessionKey, shownPath, oldText, newText, warning),
		OnAnswer:   func(approve bool) string { return s.answer(r.id, approve) },
		OnExpire:   func() { s.timeout(r.id) },
	})
	if err != nil {
		s.resolve(r, Failed, "the approval prompt could not be shown in any chat: "+err.Error())
		return s.statusOf(r)
	}
	r.stop = s.d.AfterFunc(ClampTimeout(sub.Timeout), func() { s.timeout(r.id) })
	if len(unknown) > 0 {
		logger.Infof("config change %s (%s) writes key(s) this binary does not know: %s", p, r.id, strings.Join(unknown, ", "))
	}
	logger.Infof("config change %s requested by %s (%s): %s -> %s", p, sub.SessionKey, r.id, oldText, newText)
	return s.statusOf(r)
}

// promptText is the approval prompt's markdown. Each value goes on its own
// lines in a fenced code block (#2936): monospace on the app, Telegram and
// Discord, and a long value (an array of tables) cannot run into its label or
// the other value. A non-empty warning (unknownWarning) goes just above the
// closing line, where it is read before the buttons.
func promptText(sessionKey, p string, oldText, newText, warning string) string {
	if warning != "" {
		warning += "\n\n"
	}
	return fmt.Sprintf("🔧 Config change requested\n\nSession: %s\nSetting: %s\n\nCurrent value:\n%s\n\nNew value:\n%s\n\n%sNothing changes unless you approve.",
		sessionKey, p, fenced(oldText), fenced(newText), warning)
}

// fenced wraps a rendered value in a fenced code block. RenderTOMLValue
// output is a single line (strings escape their newlines), so the value can
// never form a closing fence line itself.
func fenced(v string) string {
	f := codeFence(v)
	return f + "\n" + v + "\n" + f
}

// codeFence is the fence for v: three backticks, or one more than v's
// longest backtick run when that run is three or longer. CommonMark closes a
// fence only on a run at least as long as the opener, so a value holding
// backticks still renders as one block. (Telegram's converter and Discord
// recognise only a three-backtick fence; a value holding ``` is rare enough
// that they degrade there rather than in the app.)
func codeFence(v string) string {
	longest, run := 0, 0
	for _, r := range v {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "cfg-" + hex.EncodeToString(b)
}

func (s *Service) statusOf(r *request) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return r.status
}

// resolve ends a pending request; it reports false when it had already ended.
func (s *Service) resolve(r *request, o Outcome, reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.status.Outcome != Pending {
		return false
	}
	r.status.Outcome = o
	r.status.Reason = reason
	r.resolved = s.d.Now()
	if r.stop != nil {
		r.stop()
	}
	close(r.done)
	logger.Infof("config change %s (%s): %s %s", r.status.Path, r.id, o, reason)
	return true
}

func (s *Service) get(id string) *request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[id]
}

// answer handles the user's button press and returns the prompt's new text.
func (s *Service) answer(id string, approve bool) string {
	r := s.get(id)
	if r == nil || s.statusOf(r).Outcome != Pending {
		return "⏱ This config request is no longer pending; nothing was changed."
	}
	if !approve {
		if !s.resolve(r, Denied, "the user denied the change") {
			return "⏱ This config request is no longer pending; nothing was changed."
		}
		return "❌ Denied: " + r.status.Path + " not changed"
	}
	switch o := s.approve(r); o {
	case Applied:
		return "✅ Applied: " + r.status.Path + " = " + r.status.New
	case Failed:
		return "⚠️ Approved, but not applied: " + s.statusOf(r).Reason
	default:
		return "⏱ This config request is no longer pending; nothing was changed."
	}
}

// approve applies an approved request and resolves it, all under writeMu so
// a timeout cannot land between the write and the Applied outcome.
func (s *Service) approve(r *request) Outcome {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.statusOf(r).Outcome != Pending {
		return s.statusOf(r).Outcome
	}
	if err := s.apply(r); err != nil {
		s.resolve(r, Failed, err.Error())
		return Failed
	}
	s.resolve(r, Applied, "")
	if s.d.Apply != nil {
		s.d.Apply(r.path)
	}
	return Applied
}

// apply re-reads the file, re-runs every check and writes the edit. The
// caller holds writeMu.
func (s *Service) apply(r *request) error {
	if s.d.CheckProtected != nil {
		if err := s.d.CheckProtected(s.d.ConfigPath); err != nil {
			return err
		}
	}
	data, err := os.ReadFile(s.d.ConfigPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	cur := string(data)
	old, oldSet, err := lookupOld(cur, r.path, r.Op)
	if err != nil {
		return err
	}
	if oldSet != r.oldSet || !reflect.DeepEqual(old, r.oldValue) {
		now := "(unset)"
		if oldSet {
			now = config.RenderTOMLValue(old)
		}
		return fmt.Errorf("%s changed since the request (now %s, was %s); nothing was written", r.path, now, r.status.Old)
	}
	next, err := plan(cur, r.path, r.Op, r.Value)
	if err != nil {
		return err
	}
	if s.d.CheckContent != nil {
		if _, err := s.d.CheckContent(next, s.d.ConfigPath); err != nil {
			return fmt.Errorf("the changed config would not pass the config check: %w", err)
		}
	}
	return config.WriteConfigFile(s.d.ConfigPath, next, s.d.FileMode)
}

// timeout ends a request nobody answered in time and edits its prompt.
func (s *Service) timeout(id string) {
	r := s.get(id)
	if r == nil {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.resolve(r, TimedOut, "nobody answered the approval prompt in time") && s.d.Close != nil {
		s.d.Close(id, "⏱ Timed out: "+r.status.Path+" not changed")
	}
}

// Wait returns the request's status once it is no longer pending, or after
// maxWait, or when ctx ends. known is false for an id this gateway never
// issued (or issued before a restart).
func (s *Service) Wait(ctx context.Context, id string, maxWait time.Duration) (st Status, known bool) {
	r := s.get(id)
	if r == nil {
		return Status{ID: id, Outcome: Failed, Reason: "unknown request: the gateway restarted or the request expired; nothing was changed"}, false
	}
	t := time.NewTimer(maxWait)
	defer t.Stop()
	select {
	case <-r.done:
	case <-t.C:
	case <-ctx.Done():
	}
	return s.statusOf(r), true
}

// prune forgets requests resolved more than retainResolved ago.
func (s *Service) prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.d.Now()
	for id, r := range s.reqs {
		if r.status.Outcome != Pending && now.Sub(r.resolved) > retainResolved {
			delete(s.reqs, id)
		}
	}
}
