package projection

import (
	"context"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// at builds an envelope with an explicit timestamp so dwell math is
// deterministic (event.New stamps time.Now()).
func at(typ event.Type, corr string, ts time.Time, payload any) event.Envelope {
	e := event.New(typ, 1, event.MustMarshal(payload)).WithCorrelation(corr)
	e.Timestamp = ts
	return e
}

// dwellScenario returns a synthetic correlation that started, was dispatched,
// dropped once on an unsatisfied join, then completed — the canonical
// blocked-then-ran trace.
func dwellScenario(corr string, t0 time.Time) []event.Envelope {
	return []event.Envelope{
		at(event.WorkflowStartedFor("workspace-dev"), corr, t0,
			event.WorkflowStartedPayload{WorkflowID: "workspace-dev"}),
		at(event.DispatchStarted, corr, t0.Add(1*time.Second),
			event.DispatchStartedPayload{
				Persona:       "committer",
				SpawnUnixNano: t0.Add(1 * time.Second).UnixNano(),
			}),
		at(event.DispatchDropped, corr, t0.Add(2*time.Second),
			event.DispatchDroppedPayload{Handler: "committer", DropReason: "join_unsatisfied"}),
		at(event.PersonaCompleted, corr, t0.Add(10*time.Second),
			event.PersonaCompletedPayload{Persona: "committer"}),
	}
}

func TestDwellProjection_BlockedThenRan(t *testing.T) {
	p := NewDwellProjection()
	t0 := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	for _, e := range dwellScenario("corr-dwell", t0) {
		if err := p.Handle(context.Background(), e); err != nil {
			t.Fatalf("Handle %s: %v", e.Type, err)
		}
	}

	recs := p.ForWorkflow("corr-dwell")
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if rec.Persona != "committer" {
		t.Errorf("persona = %q", rec.Persona)
	}
	if rec.Terminal != "completed" {
		t.Errorf("terminal = %q, want completed", rec.Terminal)
	}
	if rec.LastReason != "join_unsatisfied" {
		t.Errorf("last_reason = %q", rec.LastReason)
	}
	if rec.DropsByReason["join_unsatisfied"] != 1 {
		t.Errorf("drops = %v", rec.DropsByReason)
	}

	blocked, ok := rec.BlockedDwell()
	if !ok {
		t.Fatal("expected a blocked dwell")
	}
	if blocked != 8*time.Second {
		t.Errorf("blocked dwell = %s, want 8s", blocked)
	}
	exec, ok := rec.ExecutionDuration()
	if !ok {
		t.Fatal("expected an execution duration")
	}
	if exec != 9*time.Second {
		t.Errorf("exec duration = %s, want 9s", exec)
	}

	if start, ok := p.WorkflowStartedAt("corr-dwell"); !ok || !start.Equal(t0) {
		t.Errorf("workflow start = %v (ok=%v), want %v", start, ok, t0)
	}
}

func TestDwellProjection_SummaryByReason(t *testing.T) {
	p := NewDwellProjection()
	t0 := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	// Two correlations dropped on join_unsatisfied (8s, 4s); one on ctx_cancelled (2s).
	for _, e := range dwellScenario("c1", t0) {
		_ = p.Handle(context.Background(), e)
	}
	// c2: dropped join_unsatisfied at +1s, completed at +5s → 4s blocked.
	_ = p.Handle(context.Background(), at(event.DispatchDropped, "c2", t0.Add(1*time.Second),
		event.DispatchDroppedPayload{Handler: "qa", DropReason: "join_unsatisfied"}))
	_ = p.Handle(context.Background(), at(event.PersonaCompleted, "c2", t0.Add(5*time.Second),
		event.PersonaCompletedPayload{Persona: "qa"}))
	// c3: dropped ctx_cancelled at +0s, completed at +2s → 2s blocked.
	_ = p.Handle(context.Background(), at(event.DispatchDropped, "c3", t0,
		event.DispatchDroppedPayload{Handler: "reviewer", DropReason: "ctx_cancelled"}))
	_ = p.Handle(context.Background(), at(event.PersonaCompleted, "c3", t0.Add(2*time.Second),
		event.PersonaCompletedPayload{Persona: "reviewer"}))

	byReason := map[string]ReasonDwell{}
	for _, r := range p.SummaryByReason() {
		byReason[r.Reason] = r
	}

	ju := byReason["join_unsatisfied"]
	if ju.Drops != 2 || ju.BlockedRecords != 2 {
		t.Errorf("join_unsatisfied: drops=%d blocked=%d, want 2/2", ju.Drops, ju.BlockedRecords)
	}
	if ju.TotalBlockedSeconds != 12 { // 8 + 4
		t.Errorf("join_unsatisfied total blocked = %.0f, want 12", ju.TotalBlockedSeconds)
	}
	if ju.MaxBlockedSeconds != 8 {
		t.Errorf("join_unsatisfied max blocked = %.0f, want 8", ju.MaxBlockedSeconds)
	}
	cc := byReason["ctx_cancelled"]
	if cc.Drops != 1 || cc.BlockedRecords != 1 || cc.TotalBlockedSeconds != 2 {
		t.Errorf("ctx_cancelled = %+v, want drops=1 blocked=1 total=2", cc)
	}
}

// TestDwellProjection_RebuildDeterminism replays the same log into two fresh
// projections and asserts identical results — the projection must be a pure,
// rebuildable fold over the event log (the catch-up rebuild is the
// authoritative path, since diagnostics are not on the live bus).
func TestDwellProjection_RebuildDeterminism(t *testing.T) {
	t0 := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	log := dwellScenario("corr-rebuild", t0)

	build := func() []DwellRecord {
		p := NewDwellProjection()
		for _, e := range log {
			if err := p.Handle(context.Background(), e); err != nil {
				t.Fatalf("Handle: %v", err)
			}
		}
		return p.Records()
	}

	a := build()
	b := build()
	if len(a) != len(b) {
		t.Fatalf("rebuild lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		ab, _ := a[i].BlockedDwell()
		bb, _ := b[i].BlockedDwell()
		if a[i].Persona != b[i].Persona || ab != bb || a[i].Terminal != b[i].Terminal {
			t.Errorf("record %d differs across rebuilds: %+v vs %+v", i, a[i], b[i])
		}
	}
}

// TestDwellProjection_AIBackendAttribution verifies that the resolved backend
// (0001) recorded on AIResponseReceived is captured for review-handler
// bucketing.
func TestDwellProjection_AIBackendAttribution(t *testing.T) {
	p := NewDwellProjection()
	t0 := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	_ = p.Handle(context.Background(), at(event.AIResponseReceived, "c", t0,
		event.AIResponsePayload{Persona: "reviewer", Backend: "codex"}))
	_ = p.Handle(context.Background(), at(event.PersonaCompleted, "c", t0.Add(time.Second),
		event.PersonaCompletedPayload{Persona: "reviewer"}))

	recs := p.ForWorkflow("c")
	if len(recs) != 1 || recs[0].Backend != "codex" {
		t.Fatalf("want backend=codex, got %+v", recs)
	}
}
