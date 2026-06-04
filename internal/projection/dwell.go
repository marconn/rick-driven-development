package projection

import (
	"context"
	"encoding/json"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// DwellProjection measures how long persona handlers sit blocked before they
// finally run, and how long they take to execute, bucketed by drop reason and
// persona. It is the telemetry that decides WHICH stall class actually strands
// workflows — the go/no-go gate for the dispatch-projection track (0010).
//
// It is a PURE READ MODEL: a fold over the existing event log, rebuildable from
// the store, writing nothing back. It consumes two STORAGE-ONLY diagnostics —
// DispatchDropped ({correlationID}:drops) and DispatchStarted
// ({correlationID}:dispatch) — plus PersonaCompleted/PersonaFailed and the
// workflow-started family.
//
// Live-vs-rebuild semantics (important): DispatchDropped and DispatchStarted
// are never published on the bus (they are written to dedicated diagnostic
// aggregates), so the Runner's LIVE subscription does not deliver them — only
// its catch-up phase (store.LoadAll) does. Authoritative dwell data therefore
// comes from a fresh catch-up rebuild over the log (process restart or an
// offline rebuild), which sees every diagnostic. A long-lived process's live
// view captures the completion side but not new drops/starts. This is the
// deliberate trade-off for keeping diagnostics off the bus; the dataset is read
// after accumulating traffic, not as a live SLO meter.
type DwellProjection struct {
	mu sync.RWMutex
	// workflowStart is the clock origin for a persona's first-predecessor wait:
	// the workflow-started timestamp per correlation (F-agy5 — a handler waiting
	// on its first predecessor emits no drop, so drop-based dwell alone
	// undercounts the longest waits).
	workflowStart map[string]time.Time
	records       map[dwellKey]*DwellRecord
}

type dwellKey struct {
	CorrelationID string
	Persona       string
}

// DwellRecord is the accumulated dwell/execution state for one
// (correlation, persona) pair.
type DwellRecord struct {
	CorrelationID string
	Persona       string
	// Backend is the concrete backend that ran (resolved per 0001 from
	// AIRequestSent/AIResponseReceived, never the composite rotation name).
	// Empty for non-AI handlers.
	Backend string

	// Drop accounting.
	FirstDropAt   time.Time      // earliest DispatchDropped for this pair
	LastDropAt    time.Time      // latest DispatchDropped
	LastReason    string         // drop_reason of the latest drop (what finally held it up)
	DropsByReason map[string]int // count of drops per reason

	// Execution / completion.
	StartedAt   time.Time // DispatchStarted spawn (execution start)
	CompletedAt time.Time // PersonaCompleted/PersonaFailed timestamp
	Terminal    string    // "completed" | "failed" | "" (still pending)
}

// BlockedDwell is the time from the first observed drop to terminal completion
// — how long the persona was stranded in a blocked state. ok is false when the
// pair never dropped or never completed.
func (r *DwellRecord) BlockedDwell() (time.Duration, bool) {
	if r.FirstDropAt.IsZero() || r.CompletedAt.IsZero() {
		return 0, false
	}
	return r.CompletedAt.Sub(r.FirstDropAt), true
}

// ExecutionDuration is the time from execution start (DispatchStarted) to
// terminal completion. ok is false when either bound is missing.
func (r *DwellRecord) ExecutionDuration() (time.Duration, bool) {
	if r.StartedAt.IsZero() || r.CompletedAt.IsZero() {
		return 0, false
	}
	return r.CompletedAt.Sub(r.StartedAt), true
}

// NewDwellProjection creates an empty dwell projection.
func NewDwellProjection() *DwellProjection {
	return &DwellProjection{
		workflowStart: make(map[string]time.Time),
		records:       make(map[dwellKey]*DwellRecord),
	}
}

func (p *DwellProjection) Name() string { return "dwell" }

// Handle folds one event into the dwell state. Deterministic on an in-order
// replay over a fresh projection (how the Runner's catch-up drives it), so the
// model is rebuildable from the log. DropsByReason counts drop events and so is
// NOT idempotent against double-delivery of the same event — the Runner runs
// catch-up exactly once on a fresh projection, matching the VerdictProjection
// append convention.
func (p *DwellProjection) Handle(_ context.Context, env event.Envelope) error {
	corr := env.CorrelationID
	if corr == "" {
		corr = env.AggregateID
	}

	switch {
	case event.IsWorkflowStarted(env.Type):
		p.mu.Lock()
		if cur, ok := p.workflowStart[corr]; !ok || env.Timestamp.Before(cur) {
			p.workflowStart[corr] = env.Timestamp
		}
		p.mu.Unlock()

	case env.Type == event.DispatchDropped:
		var pl event.DispatchDroppedPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return err
		}
		p.mu.Lock()
		rec := p.getOrCreate(corr, pl.Handler)
		if rec.FirstDropAt.IsZero() || env.Timestamp.Before(rec.FirstDropAt) {
			rec.FirstDropAt = env.Timestamp
		}
		if env.Timestamp.After(rec.LastDropAt) {
			rec.LastDropAt = env.Timestamp
			rec.LastReason = pl.DropReason
		}
		rec.DropsByReason[pl.DropReason]++
		p.mu.Unlock()

	case env.Type == event.DispatchStarted:
		var pl event.DispatchStartedPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return err
		}
		started := env.Timestamp
		if pl.SpawnUnixNano > 0 {
			started = time.Unix(0, pl.SpawnUnixNano)
		}
		p.mu.Lock()
		rec := p.getOrCreate(corr, pl.Persona)
		if rec.StartedAt.IsZero() || started.Before(rec.StartedAt) {
			rec.StartedAt = started
		}
		p.mu.Unlock()

	case env.Type == event.AIResponseReceived:
		// Captures the resolved backend (0001) for review-handler bucketing.
		var pl event.AIResponsePayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return err
		}
		if pl.Persona == "" {
			return nil
		}
		p.mu.Lock()
		rec := p.getOrCreate(corr, pl.Persona)
		if pl.Backend != "" {
			rec.Backend = pl.Backend
		}
		p.mu.Unlock()

	case env.Type == event.PersonaCompleted:
		var pl event.PersonaCompletedPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return err
		}
		p.mu.Lock()
		rec := p.getOrCreate(corr, pl.Persona)
		rec.CompletedAt = env.Timestamp
		rec.Terminal = "completed"
		p.mu.Unlock()

	case env.Type == event.PersonaFailed:
		var pl event.PersonaFailedPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return err
		}
		p.mu.Lock()
		rec := p.getOrCreate(corr, pl.Persona)
		rec.CompletedAt = env.Timestamp
		rec.Terminal = "failed"
		if rec.Backend == "" && pl.Backend != "" {
			rec.Backend = pl.Backend
		}
		p.mu.Unlock()
	}

	return nil
}

// getOrCreate must be called with p.mu held.
func (p *DwellProjection) getOrCreate(corr, persona string) *DwellRecord {
	key := dwellKey{CorrelationID: corr, Persona: persona}
	rec, ok := p.records[key]
	if !ok {
		rec = &DwellRecord{
			CorrelationID: corr,
			Persona:       persona,
			DropsByReason: make(map[string]int),
		}
		p.records[key] = rec
	}
	return rec
}

// WorkflowStartedAt returns the recorded workflow-start time for a correlation,
// the clock origin for first-predecessor wait. Zero time + false when unknown.
func (p *DwellProjection) WorkflowStartedAt(correlationID string) (time.Time, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	t, ok := p.workflowStart[correlationID]
	return t, ok
}

// ForWorkflow returns copies of the dwell records for a correlation, sorted by
// persona for stable output.
func (p *DwellProjection) ForWorkflow(correlationID string) []DwellRecord {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []DwellRecord
	for key, rec := range p.records {
		if key.CorrelationID == correlationID {
			out = append(out, cloneDwellRecord(rec))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Persona < out[j].Persona })
	return out
}

// Records returns copies of every accumulated dwell record (the raw dataset),
// sorted by correlation then persona.
func (p *DwellProjection) Records() []DwellRecord {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]DwellRecord, 0, len(p.records))
	for _, rec := range p.records {
		out = append(out, cloneDwellRecord(rec))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CorrelationID != out[j].CorrelationID {
			return out[i].CorrelationID < out[j].CorrelationID
		}
		return out[i].Persona < out[j].Persona
	})
	return out
}

// ReasonDwell aggregates dwell statistics for one drop reason.
type ReasonDwell struct {
	Reason string
	// Drops is the total number of DispatchDropped events with this reason.
	Drops int
	// BlockedRecords is the number of (correlation, persona) pairs whose LAST
	// (held-up-by) reason was this one AND that reached terminal completion, so
	// a blocked-dwell duration could be measured.
	BlockedRecords int
	// TotalBlockedSeconds / MaxBlockedSeconds summarize those pairs' blocked
	// dwell. Mean = TotalBlockedSeconds / BlockedRecords.
	TotalBlockedSeconds float64
	MaxBlockedSeconds   float64
}

// SummaryByReason aggregates dwell across all records, keyed by drop reason.
// Drop counts include every drop of that reason; blocked-dwell duration is
// attributed only to the reason that FINALLY held a pair up (its LastReason)
// and only when the pair completed — so the per-reason blocked totals partition
// the records (no double-counting) even though a pair may have dropped for
// several reasons over its life. Returned sorted by reason.
func (p *DwellProjection) SummaryByReason() []ReasonDwell {
	p.mu.RLock()
	defer p.mu.RUnlock()

	agg := make(map[string]*ReasonDwell)
	get := func(reason string) *ReasonDwell {
		r, ok := agg[reason]
		if !ok {
			r = &ReasonDwell{Reason: reason}
			agg[reason] = r
		}
		return r
	}

	for _, rec := range p.records {
		for reason, n := range rec.DropsByReason {
			get(reason).Drops += n
		}
		if dwell, ok := rec.BlockedDwell(); ok && rec.LastReason != "" {
			r := get(rec.LastReason)
			r.BlockedRecords++
			secs := dwell.Seconds()
			r.TotalBlockedSeconds += secs
			if secs > r.MaxBlockedSeconds {
				r.MaxBlockedSeconds = secs
			}
		}
	}

	out := make([]ReasonDwell, 0, len(agg))
	for _, r := range agg {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Reason < out[j].Reason })
	return out
}

func cloneDwellRecord(rec *DwellRecord) DwellRecord {
	c := *rec
	c.DropsByReason = make(map[string]int, len(rec.DropsByReason))
	maps.Copy(c.DropsByReason, rec.DropsByReason)
	return c
}
