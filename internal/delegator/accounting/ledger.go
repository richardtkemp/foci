// Package accounting is foci's cost ledger (#2111): one row per API call on
// every backend, counts stored by token class, and cost computed as-of each
// call's billing time by SQL views over a rate table rendered from modelinfo.
//
// Backends are adapters. Each emits normalised Call records when a call's
// usage is final, and Report records carrying the backend's own totals at its
// native grain; booking, dedup and the checks live here, once. See
// docs/WIRING.md "Cost ledger" and clutch notes/2111.md for the design.
package accounting

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"foci/internal/modelinfo"
	"foci/internal/sqlite"
)

// Backend names a backend adapter.
const (
	BackendCCStream = "ccstream"
	BackendCodex    = "codex"
	BackendOpencode = "opencode"
	BackendAPI      = "api"
)

// Call kinds.
const (
	// KindCall is one API call the backend made and reported.
	KindCall = "call"
	// KindCompaction is a compaction, booked on its own turn.
	KindCompaction = "compaction"
	// KindOverhead is spend a backend's cumulative totals count and no call
	// record holds (CC utility calls, stopless true output), booked at a quiet
	// point with no turn (R6).
	KindOverhead = "overhead"
	// KindSummary and KindSpawn are the direct API's summary and spawn calls.
	KindSummary = "summary"
	KindSpawn   = "spawn"
	// KindLegacy is one pre-ledger api.db row carried over by the migration.
	KindLegacy = "legacy"
)

// Call finalities: how final a call's counts are.
const (
	FinalityCompleted = "completed"
	// FinalityStopless is a subagent message with no completed line: final
	// input and cache counts, placeholder output (#2111 §4).
	FinalityStopless = "stopless"
	// FinalityInterrupted is a CC main-thread call that was interrupted: sent
	// and billed, but counted nowhere by CC itself (#2112 P0-e). Booked AND
	// priced like any call (Dick, 2026-09-29 12:08, #2111 §13.8), and kept out
	// of the remainder subtraction.
	FinalityInterrupted = "interrupted"
	// FinalityDerived is a remainder: overhead or a compaction.
	FinalityDerived = "derived"
	FinalityLegacy  = "legacy"
)

// Cost bases: where the views take a call's cost from (Dick, 2026-09-29).
const (
	// CostBasisCounts prices the call's counts at the rates in effect when it
	// was billed. Every live call, and every legacy call whose counts are
	// what was billed.
	CostBasisCounts = "counts"
	// CostBasisRecorded takes the call's LegacyCalculatedCostUSD verbatim: a
	// legacy row whose stored counts are not the billed figure (a delegated
	// row with no turn totals holds a context-fill snapshot), so re-pricing
	// them would rewrite history with a wrong number.
	CostBasisRecorded = "recorded"
)

// Class methods: how a call's cache-write TTL classes were decided.
const (
	ClassMethodObserved    = "observed"     // the provider reported the split
	ClassMethodBackendRule = "backend_rule" // fixed by the backend (direct API: always 1h)
	ClassMethodSolved      = "solved"       // solved from CC's reported cost (legacy rows; a CC remainder, #2130)
	ClassMethodKind        = "kind"         // legacy: inferred from the record kind
	ClassMethodUnknown     = "unknown"      // TTL not reported
)

// Turn sources.
const (
	SourceUser       = "user"
	SourceAutonomous = "autonomous"
	SourceKeepalive  = "keepalive"
	SourceCompaction = "compaction"
	SourceBatch      = "batch"
	SourceSystem     = "system"
)

// Report grains.
const (
	GrainCumulative = "cumulative"
	GrainPerCall    = "per_call"
)

// Invariants the ledger alarms on (#2111 §8). Never absorbed.
const (
	InvSameIDDifferentUsage = "invSameIDDifferentUsage"
	InvClassNoRate          = "invClassNoRate"
	InvModelNotInTable      = "invModelNotInTable"
	// InvNegativeRemainder: a backend's cumulative report minus everything
	// booked in its scope went negative in some class, so the booked set is
	// wrong. Nothing is booked for the remainder and nothing is clamped.
	InvNegativeRemainder = "invNegativeRemainder"
	// InvStreamIdBooked: a call the backend's stream named never appeared in
	// the source its usage is booked from within the bound.
	InvStreamIdBooked = "invStreamIdBooked"
	// InvRemainderTTLUnsolved: a remainder's cache writes carry no TTL and
	// the backend's reported cost did not solve their 5m/1h split, so they
	// are booked TTL-unknown, at the 1h rate (#2130).
	InvRemainderTTLUnsolved = "invRemainderTTLUnsolved"
)

// Alarm raises an invariant violation an adapter found outside a booking.
func (l *Ledger) Alarm(a Alarm) {
	if l != nil && l.OnAlarm != nil {
		l.OnAlarm(a)
	}
}

// Call is one API call, normalised by its backend's adapter.
type Call struct {
	// Key is the provider's message id, or a stable id the adapter builds. It
	// is the dedup key: booking the same (Backend, Key) twice books once.
	// Empty only for a legacy call.
	Key string

	Backend, Provider, Model string

	Session, AgentID string
	// TurnID is the turn the call belongs to; empty only for overhead.
	TurnID string
	// Actor is "" for the session's own thread, else the subagent that made
	// the call (CC's Agent tool_use id, a codex sub-thread id).
	Actor string

	Kind, Finality, ClassMethod string

	// BilledAt is when the provider billed the call. It dates the call and
	// picks the rates it is priced at.
	BilledAt time.Time

	Tokens     modelinfo.Tokens
	StopReason string

	SessionFile string
	SessionLine int

	// Detail holds adapter extras kept for forensics; never priced.
	Detail map[string]any

	// LegacyID and LegacyCalculatedCostUSD exist for the migration only: a
	// legacy call keeps its pre-ledger row id, and foci's old recorded figure
	// as an audit column the cost views never read (Dick, 2026-09-29 10:46).
	LegacyID                int64
	LegacyCalculatedCostUSD *float64
	// CostBasis is CostBasisCounts (the default, "") or, for a legacy call
	// only, CostBasisRecorded.
	CostBasis string
}

// Report is a backend's own reported totals at its native grain (R5): CC's
// cumulative per-process ModelUsage, codex's cumulative thread total (tokens
// only), opencode's per-message cost.
type Report struct {
	Backend, Session string
	// ScopeKey names what the totals cover: a CC process launch, a codex
	// thread, an opencode message.
	ScopeKey string
	Model    string
	Grain    string
	At       time.Time
	// CostUSD is the backend's figure verbatim; nil when it reports none.
	CostUSD *float64
	Tokens  modelinfo.Tokens
}

// Turn is one turn's facts. RecordTurn upserts it, so the agent layer writes
// it at turn start and again at turn end; a zero field leaves the stored value
// alone.
type Turn struct {
	TurnID, Session, AgentID, Backend string
	Source, Purpose                   string

	StartedAt, EndedAt time.Time
	AskCycles          int
	StopReason         string
	FinalModel         string
	// ActivityClosedAt is when the last spend attributable to the turn
	// stopped; zero means still running (R8).
	ActivityClosedAt time.Time

	// Legacy is the context fill copied from a pre-ledger row (R4); nil for
	// every turn the ledger books itself.
	Legacy *LegacyFill
}

// LegacyFill is a pre-ledger turn's final-cycle context fill, copied verbatim.
type LegacyFill struct {
	Input, Output, CacheRead, CacheWrite int
}

// Alarm is one invariant violation. The ledger never absorbs one: it is
// delivered to Ledger.OnAlarm after the transaction that found it commits.
type Alarm struct {
	Invariant string
	Backend   string
	Detail    string
}

// Ledger is the cost ledger in api.db. It is the database's only writer.
type Ledger struct {
	db *sql.DB
	// mu serialises writes. Every writer in the process books through Update,
	// and some transactions read before they write (a subagent share looks for
	// its call before extending it); under WAL a second writer committing in
	// between would fail that upgrade with SQLITE_BUSY at once, not wait.
	mu sync.Mutex

	// OnAlarm receives every invariant violation. nil drops them, which only
	// a caller that counts them itself (the migration) should allow.
	OnAlarm func(Alarm)

	// shadow marks a shadow ledger (Options.Shadow): its bookings reach no
	// observer.
	shadow bool
}

// Close closes the ledger's database.
func (l *Ledger) Close() error { return l.db.Close() }

// Tx is one ledger transaction.
type Tx struct {
	tx     *sql.Tx
	alarms []Alarm
	// booked is every call this transaction booked or extended, handed to the
	// observers once it commits.
	booked []Booking
	// quiet books without observing: the migration carries history over, and
	// history is neither re-logged to api.jsonl nor re-sent as generations.
	quiet bool
}

// Update runs fn in one transaction and commits if it returns nil. Alarms
// raised inside, and the bookings the observers see (api.jsonl, BookedHook),
// are delivered only once the transaction has committed, so a rolled-back
// booking never alarms and is never observed.
func (l *Ledger) Update(fn func(*Tx) error) error {
	tx, err := l.commit(fn)
	if err != nil {
		return err
	}
	// Delivered outside the write lock: an observer or alarm sink is never
	// on the booking path.
	if l.OnAlarm != nil {
		for _, a := range tx.alarms {
			l.OnAlarm(a)
		}
	}
	if !l.shadow {
		for _, b := range tx.booked {
			observe(b)
		}
	}
	return nil
}

// commit runs fn in one transaction under the write lock and commits it.
func (l *Ledger) commit(fn func(*Tx) error) (*Tx, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sqlTx, err := l.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("ledger: begin: %w", err)
	}
	tx := &Tx{tx: sqlTx}
	if err := fn(tx); err != nil {
		_ = sqlTx.Rollback()
		return nil, err
	}
	if err := sqlTx.Commit(); err != nil {
		return nil, fmt.Errorf("ledger: commit: %w", err)
	}
	return tx, nil
}

func (tx *Tx) alarm(inv, backend, format string, args ...any) {
	tx.alarms = append(tx.alarms, Alarm{Invariant: inv, Backend: backend, Detail: fmt.Sprintf(format, args...)})
}

// Booked says what Book did with a call.
type Booked struct {
	ID int64
	// Duplicate is true when (Backend, Key) was already booked; the stored
	// call is never changed.
	Duplicate bool
}

// Book books one call (#2111 §3): it resolves the call's rate_model and
// inserts the call and its counts, or does nothing if (Backend, Key) is
// already booked. A duplicate whose counts differ from the stored ones raises
// invSameIDDifferentUsage; the first booking stands. An unresolvable model
// raises invModelNotInTable and a billed class with no rate raises
// invClassNoRate; the call is booked either way and its cost reads NULL.
func (tx *Tx) Book(c Call) (Booked, error) {
	if err := c.validate(); err != nil {
		return Booked{}, err
	}
	// A call whose model is not known yet (a codex usage notification before
	// any message named it, #1290) is unresolved without asking modelinfo,
	// whose unpriced-model warning is for a model it does not know.
	var rateModel sql.NullString
	if c.Model != "" {
		if rm, ok := modelinfo.ResolveRateModel(c.Model, c.BilledAt); ok {
			rateModel = sql.NullString{String: rm, Valid: true}
		}
	}

	var detail sql.NullString
	if len(c.Detail) > 0 {
		b, err := json.Marshal(c.Detail)
		if err != nil {
			return Booked{}, fmt.Errorf("ledger: detail: %w", err)
		}
		detail = sql.NullString{String: string(b), Valid: true}
	}
	var legacyID any
	if c.LegacyID > 0 {
		legacyID = c.LegacyID
	}
	res, err := tx.tx.Exec(`INSERT OR IGNORE INTO api_calls
		(id, call_key, billed_at, backend, provider, model, rate_model, session, agent_id,
		 turn_id, actor, kind, finality, class_method, stop_reason, session_file, session_line,
		 detail, legacy_calculated_cost_usd, cost_basis)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		legacyID, nullIfEmpty(c.Key), formatTime(c.BilledAt), c.Backend, c.Provider, c.Model, rateModel,
		c.Session, nullIfEmpty(c.AgentID), nullIfEmpty(c.TurnID), c.Actor, c.Kind, c.Finality,
		c.ClassMethod, nullIfEmpty(c.StopReason), nullIfEmpty(c.SessionFile), nullIfZero(c.SessionLine),
		detail, c.LegacyCalculatedCostUSD, c.basis())
	if err != nil {
		return Booked{}, fmt.Errorf("ledger: book %s %q: %w", c.Backend, c.Key, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tx.duplicate(c)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Booked{}, fmt.Errorf("ledger: book id: %w", err)
	}
	for _, class := range slices.Sorted(maps.Keys(c.Tokens)) {
		n := c.Tokens[class]
		if n == 0 {
			continue
		}
		if _, err := tx.tx.Exec(`INSERT INTO call_tokens (call_id, class, count) VALUES (?, ?, ?)`,
			id, string(class), n); err != nil {
			return Booked{}, fmt.Errorf("ledger: book tokens %s %q: %w", c.Backend, c.Key, err)
		}
	}
	if c.basis() == CostBasisCounts {
		tx.checkPriced(c, rateModel)
	}
	if !tx.quiet {
		b, err := tx.booking(c, id)
		if err != nil {
			return Booked{}, err
		}
		tx.booked = append(tx.booked, b)
	}
	return Booked{ID: id}, nil
}

// booking is what the observers see of call c, booked as id: its cost priced
// the way the call_costs view prices it, and the facts they need from its
// turn.
func (tx *Tx) booking(c Call, id int64) (Booking, error) {
	b := Booking{Call: c, ID: id, CostUSD: c.cost(), Fill: c.fill()}
	if c.TurnID == "" {
		return b, nil
	}
	var purpose sql.NullString
	var li, lr, lw sql.NullInt64
	err := tx.tx.QueryRow(`SELECT purpose, source, legacy_input, legacy_cache_read, legacy_cache_write
		FROM turns WHERE turn_id = ?`, c.TurnID).Scan(&purpose, &b.TurnSource, &li, &lr, &lw)
	if err != nil {
		return Booking{}, fmt.Errorf("ledger: read turn %q: %w", c.TurnID, err)
	}
	b.Purpose = purpose.String
	// A legacy call's counts are its turn's totals, not a context size: the
	// fill a pre-ledger parent row left is the copy on its turn (R4). A legacy
	// subagent share has none of its own.
	if c.Kind == KindLegacy {
		b.Fill = 0
		if c.Actor == "" {
			b.Fill = int(li.Int64 + lr.Int64 + lw.Int64)
		}
	}
	return b, nil
}

// duplicate handles a call whose (Backend, Key) is already booked.
func (tx *Tx) duplicate(c Call) (Booked, error) {
	var id int64
	if err := tx.tx.QueryRow(`SELECT id FROM api_calls WHERE backend = ? AND call_key = ?`,
		c.Backend, c.Key).Scan(&id); err != nil {
		return Booked{}, fmt.Errorf("ledger: find booked %s %q: %w", c.Backend, c.Key, err)
	}
	stored, err := tx.tokens(id)
	if err != nil {
		return Booked{}, err
	}
	if !maps.Equal(stored, nonZero(c.Tokens)) {
		tx.alarm(InvSameIDDifferentUsage, c.Backend,
			"%s call %q booked with %v, seen again with %v — the first booking stands", c.Backend, c.Key, stored, nonZero(c.Tokens))
	}
	return Booked{ID: id, Duplicate: true}, nil
}

// tokens reads one booked call's counts.
func (tx *Tx) tokens(id int64) (modelinfo.Tokens, error) {
	rows, err := tx.tx.Query(`SELECT class, count FROM call_tokens WHERE call_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("ledger: read tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	t := modelinfo.Tokens{}
	for rows.Next() {
		var class string
		var n int
		if err := rows.Scan(&class, &n); err != nil {
			return nil, fmt.Errorf("ledger: scan tokens: %w", err)
		}
		t[modelinfo.Class(class)] = n
	}
	return t, rows.Err()
}

// checkPriced raises invModelNotInTable / invClassNoRate for a call the views
// will show as unpriced.
func (tx *Tx) checkPriced(c Call, rateModel sql.NullString) {
	if len(nonZero(c.Tokens)) == 0 {
		return
	}
	if !rateModel.Valid {
		tx.alarm(InvModelNotInTable, c.Backend, "%s call %q: model %q has no rate group — its cost is NULL", c.Backend, c.Key, c.Model)
		return
	}
	rates, _ := modelinfo.RatesAsOf(rateModel.String, c.BilledAt)
	for _, class := range slices.Sorted(maps.Keys(c.Tokens)) {
		if c.Tokens[class] == 0 {
			continue
		}
		if _, known := modelinfo.ClassUnit(class); !known {
			tx.alarm(InvClassNoRate, c.Backend, "%s call %q: class %q is outside the vocabulary — its cost is NULL", c.Backend, c.Key, class)
			continue
		}
		if _, ok := rates[class]; !ok {
			tx.alarm(InvClassNoRate, c.Backend, "%s call %q: %s has no %s rate as of %s — its cost is NULL",
				c.Backend, c.Key, rateModel.String, class, c.BilledAt.UTC().Format("2006-01-02"))
		}
	}
}

// Report stores one backend report. The checks it feeds (cumulative
// divergence, token conservation) run in #2111 P3.
func (tx *Tx) Report(r Report) error {
	if r.Backend == "" || r.ScopeKey == "" || r.At.IsZero() {
		return fmt.Errorf("ledger: report needs a backend, scope and time: %+v", r)
	}
	if r.Grain != GrainCumulative && r.Grain != GrainPerCall {
		return fmt.Errorf("ledger: report grain %q", r.Grain)
	}
	tokens, err := json.Marshal(nonZero(r.Tokens))
	if err != nil {
		return fmt.Errorf("ledger: report tokens: %w", err)
	}
	if _, err := tx.tx.Exec(`INSERT INTO backend_reports
		(at, backend, session, scope_key, model, grain, cost_usd, tokens) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		formatTime(r.At), r.Backend, r.Session, r.ScopeKey, r.Model, r.Grain, r.CostUSD, string(tokens)); err != nil {
		return fmt.Errorf("ledger: report: %w", err)
	}
	return nil
}

// RecordTurn inserts a turn, or updates the stored one with every non-zero
// field of t.
func (tx *Tx) RecordTurn(t Turn) error {
	if t.TurnID == "" || t.Session == "" || t.Backend == "" || t.Source == "" || t.StartedAt.IsZero() {
		return fmt.Errorf("ledger: turn needs an id, session, backend, source and start: %+v", t)
	}
	var li, lo, lr, lw any
	if t.Legacy != nil {
		li, lo, lr, lw = t.Legacy.Input, t.Legacy.Output, t.Legacy.CacheRead, t.Legacy.CacheWrite
	}
	_, err := tx.tx.Exec(`INSERT INTO turns
		(turn_id, session, agent_id, backend, source, purpose, started_at, ended_at, ask_cycles,
		 stop_reason, final_model, activity_closed_at,
		 legacy_input, legacy_output, legacy_cache_read, legacy_cache_write)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (turn_id) DO UPDATE SET
			agent_id = COALESCE(excluded.agent_id, agent_id),
			purpose = COALESCE(excluded.purpose, purpose),
			ended_at = COALESCE(excluded.ended_at, ended_at),
			ask_cycles = COALESCE(excluded.ask_cycles, ask_cycles),
			stop_reason = COALESCE(excluded.stop_reason, stop_reason),
			final_model = COALESCE(excluded.final_model, final_model),
			activity_closed_at = COALESCE(excluded.activity_closed_at, activity_closed_at),
			legacy_input = COALESCE(excluded.legacy_input, legacy_input),
			legacy_output = COALESCE(excluded.legacy_output, legacy_output),
			legacy_cache_read = COALESCE(excluded.legacy_cache_read, legacy_cache_read),
			legacy_cache_write = COALESCE(excluded.legacy_cache_write, legacy_cache_write)`,
		t.TurnID, t.Session, nullIfEmpty(t.AgentID), t.Backend, t.Source, nullIfEmpty(t.Purpose),
		formatTime(t.StartedAt), nullTime(t.EndedAt), nullIfZero(t.AskCycles),
		nullIfEmpty(t.StopReason), nullIfEmpty(t.FinalModel), nullTime(t.ActivityClosedAt),
		li, lo, lr, lw)
	if err != nil {
		return fmt.Errorf("ledger: record turn %q: %w", t.TurnID, err)
	}
	return nil
}

// validate enforces the Call contract before anything reaches SQL, so a bad
// adapter fails with a message rather than a CHECK constraint number.
func (c Call) validate() error {
	var errs []error
	if c.BilledAt.IsZero() {
		errs = append(errs, errors.New("no billing time"))
	}
	if c.Backend == "" || c.Session == "" || c.Kind == "" || c.Finality == "" || c.ClassMethod == "" {
		errs = append(errs, errors.New("backend, session, kind, finality and class method are required"))
	}
	if (c.Kind == KindLegacy) != (c.Key == "") {
		errs = append(errs, errors.New("a key is required, and only a legacy call has none"))
	}
	if (c.Kind == KindOverhead) != (c.TurnID == "") {
		errs = append(errs, errors.New("a turn is required, and only overhead has none"))
	}
	if (c.LegacyID != 0 || c.LegacyCalculatedCostUSD != nil) && c.Kind != KindLegacy {
		errs = append(errs, errors.New("legacy fields on a non-legacy call"))
	}
	switch c.basis() {
	case CostBasisCounts:
	case CostBasisRecorded:
		if c.Kind != KindLegacy || c.LegacyCalculatedCostUSD == nil {
			errs = append(errs, errors.New("only a legacy call with a recorded figure can be priced from it"))
		}
	default:
		errs = append(errs, fmt.Errorf("cost basis %q", c.CostBasis))
	}
	for class, n := range c.Tokens {
		if n < 0 {
			errs = append(errs, fmt.Errorf("negative %s count %d", class, n))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("ledger: bad %s call %q: %w", c.Backend, c.Key, errors.Join(errs...))
	}
	return nil
}

// cost is c's cost as the call_costs view prices it: its recorded figure on
// the recorded basis, else its counts at the rates in effect when it was
// billed — 0 for a call with no counts, nil when any billed class is unpriced
// (never a silent 0, R9).
func (c Call) cost() *float64 {
	if c.basis() == CostBasisRecorded {
		return c.LegacyCalculatedCostUSD
	}
	if c.Model == "" {
		if len(nonZero(c.Tokens)) == 0 {
			return new(float64)
		}
		return nil // see Book: an unnamed model is unpriced, not unknown
	}
	usd, priced := modelinfo.CostAsOf(c.Model, c.BilledAt, c.Tokens)
	if !priced {
		return nil
	}
	return &usd
}

// fill is the context the call leaves: the sum of its in-context classes.
func (c Call) fill() int {
	n := 0
	for class, count := range c.Tokens {
		if modelinfo.InContext(class) {
			n += count
		}
	}
	return n
}

// basis is c's cost basis, counts by default.
func (c Call) basis() string {
	if c.CostBasis == "" {
		return CostBasisCounts
	}
	return c.CostBasis
}

// nonZero drops zero counts — the stored form, where only non-zero classes
// exist.
func nonZero(t modelinfo.Tokens) modelinfo.Tokens {
	out := modelinfo.Tokens{}
	for c, n := range t {
		if n != 0 {
			out[c] = n
		}
	}
	return out
}

// timeLayout is billed_at's stored form: UTC with fixed-width nanoseconds, so
// the text sorts chronologically and its first ten characters are the UTC date
// the rate lookup keys on.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return formatTime(t)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// open opens api.db with the pragmas the ledger needs: foreign keys ON, so a
// call cannot name a turn that does not exist, nor a count a call that does
// not.
func open(path string) (*sql.DB, error) {
	return sqlite.OpenInitPragmas(path, []string{"foreign_keys(1)"})
}
