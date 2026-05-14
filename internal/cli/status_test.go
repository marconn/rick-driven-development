package cli

import (
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/engine"
	"github.com/marconn/rick-event-driven-development/internal/event"
)

// mustMarshal wraps event.MustMarshal with a t.Helper() for clearer failures.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	return event.MustMarshal(v)
}

func TestComputeInFlightPersonas_StartedNoResponse(t *testing.T) {
	now := time.Now()
	events := []event.Envelope{
		{
			Type:      event.AIRequestStarted,
			Timestamp: now.Add(-5 * time.Minute),
			Payload: mustMarshal(t, event.AIRequestStartedPayload{
				Persona:       "developer",
				Backend:       "claude",
				SpawnUnixNano: now.Add(-5 * time.Minute).UnixNano(),
			}),
		},
	}
	got := computeInFlightPersonas(events)
	if len(got) != 1 {
		t.Fatalf("expected 1 in-flight persona, got %d", len(got))
	}
	if got[0].Persona != "developer" {
		t.Errorf("persona=%q, want developer", got[0].Persona)
	}
	if got[0].Backend != "claude" {
		t.Errorf("backend=%q, want claude", got[0].Backend)
	}
}

func TestComputeInFlightPersonas_StartedThenResponse(t *testing.T) {
	now := time.Now()
	events := []event.Envelope{
		{
			Type:      event.AIRequestStarted,
			Timestamp: now.Add(-5 * time.Minute),
			Payload: mustMarshal(t, event.AIRequestStartedPayload{
				Persona:       "developer",
				Backend:       "claude",
				SpawnUnixNano: now.Add(-5 * time.Minute).UnixNano(),
			}),
		},
		{
			Type:      event.AIResponseReceived,
			Timestamp: now.Add(-1 * time.Minute),
			Payload: mustMarshal(t, event.AIResponsePayload{
				Persona: "developer",
				Backend: "claude",
			}),
		},
	}
	if got := computeInFlightPersonas(events); len(got) != 0 {
		t.Errorf("expected no in-flight personas after response, got %+v", got)
	}
}

func TestComputeInFlightPersonas_RestartedAfterCompletion(t *testing.T) {
	// Same persona runs iter 1 (started+received), then iter 2 starts but
	// hasn't received yet. Must be reported as in-flight.
	now := time.Now()
	events := []event.Envelope{
		{
			Type:      event.AIRequestStarted,
			Timestamp: now.Add(-10 * time.Minute),
			Payload: mustMarshal(t, event.AIRequestStartedPayload{
				Persona:       "developer",
				Backend:       "claude",
				SpawnUnixNano: now.Add(-10 * time.Minute).UnixNano(),
			}),
		},
		{
			Type:      event.AIResponseReceived,
			Timestamp: now.Add(-7 * time.Minute),
			Payload:   mustMarshal(t, event.AIResponsePayload{Persona: "developer", Backend: "claude"}),
		},
		{
			Type:      event.AIRequestStarted,
			Timestamp: now.Add(-2 * time.Minute),
			Payload: mustMarshal(t, event.AIRequestStartedPayload{
				Persona:       "developer",
				Backend:       "claude",
				SpawnUnixNano: now.Add(-2 * time.Minute).UnixNano(),
			}),
		},
	}
	got := computeInFlightPersonas(events)
	if len(got) != 1 || got[0].Persona != "developer" {
		t.Fatalf("expected developer in-flight on iter 2 restart, got %+v", got)
	}
	// Must report the LATEST start, not the earlier completed iter.
	if elapsed := time.Since(got[0].StartedAt); elapsed > 3*time.Minute {
		t.Errorf("StartedAt should reflect iter 2 (~2m ago), got elapsed=%v", elapsed)
	}
}

func TestComputeInFlightPersonas_PersonaFailedTerminates(t *testing.T) {
	// AIRequestStarted followed by PersonaFailed for the same persona = not in-flight.
	now := time.Now()
	events := []event.Envelope{
		{
			Type:      event.AIRequestStarted,
			Timestamp: now.Add(-3 * time.Minute),
			Payload: mustMarshal(t, event.AIRequestStartedPayload{
				Persona: "developer", Backend: "claude",
				SpawnUnixNano: now.Add(-3 * time.Minute).UnixNano(),
			}),
		},
		{
			Type:      event.PersonaFailed,
			Timestamp: now.Add(-30 * time.Second),
			Payload:   mustMarshal(t, event.PersonaFailedPayload{Persona: "developer"}),
		},
	}
	if got := computeInFlightPersonas(events); len(got) != 0 {
		t.Errorf("PersonaFailed should terminate in-flight, got %+v", got)
	}
}

func TestComputeInFlightPersonas_ParallelPersonas(t *testing.T) {
	// reviewer + qa fire concurrently after developer — both should appear.
	now := time.Now()
	events := []event.Envelope{
		{
			Type:      event.AIRequestStarted,
			Timestamp: now.Add(-2 * time.Minute),
			Payload: mustMarshal(t, event.AIRequestStartedPayload{
				Persona: "reviewer", Backend: "round-robin(claude,gemini)",
				SpawnUnixNano: now.Add(-2 * time.Minute).UnixNano(),
			}),
		},
		{
			Type:      event.AIRequestStarted,
			Timestamp: now.Add(-2 * time.Minute),
			Payload: mustMarshal(t, event.AIRequestStartedPayload{
				Persona: "qa", Backend: "round-robin(claude,gemini)",
				SpawnUnixNano: now.Add(-2 * time.Minute).UnixNano(),
			}),
		},
	}
	got := computeInFlightPersonas(events)
	if len(got) != 2 {
		t.Fatalf("expected 2 in-flight personas, got %d", len(got))
	}
	names := map[string]bool{got[0].Persona: true, got[1].Persona: true}
	if !names["reviewer"] || !names["qa"] {
		t.Errorf("expected reviewer + qa, got %+v", names)
	}
}

func TestLastActivity_LatestWins(t *testing.T) {
	now := time.Now()
	events := []event.Envelope{
		{Type: event.WorkflowRequested, Timestamp: now.Add(-1 * time.Hour)},
		{Type: event.FeedbackGenerated, Timestamp: now.Add(-5 * time.Minute)},
		{Type: event.PersonaTracked, Timestamp: now.Add(-30 * time.Second)},
	}
	typ, age := lastActivity(events, now)
	if typ != string(event.PersonaTracked) {
		t.Errorf("type=%q, want %q", typ, event.PersonaTracked)
	}
	if age < 25*time.Second || age > 35*time.Second {
		t.Errorf("age=%v, want ~30s", age)
	}
}

func TestLastActivity_Empty(t *testing.T) {
	typ, age := lastActivity(nil, time.Now())
	if typ != "" || age != 0 {
		t.Errorf("empty input should yield zero values, got (%q, %v)", typ, age)
	}
}

func TestFormatAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{12 * time.Second, "12s"},
		{59 * time.Second, "59s"},
		{1 * time.Minute, "1m00s"},
		{8*time.Minute + 37*time.Second, "8m37s"},
		{1 * time.Hour, "1h00m"},
		{1*time.Hour + 12*time.Minute, "1h12m"},
		{-5 * time.Second, "0s"}, // clamp negatives
	}
	for _, c := range cases {
		if got := formatAge(c.d); got != c.want {
			t.Errorf("formatAge(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestIsTerminalStatus(t *testing.T) {
	terminal := []engine.WorkflowStatus{
		engine.StatusCompleted, engine.StatusFailed, engine.StatusCancelled,
	}
	for _, s := range terminal {
		if !isTerminalStatus(s) {
			t.Errorf("status %q should be terminal", s)
		}
	}
	nonTerminal := []engine.WorkflowStatus{
		engine.StatusRequested, engine.StatusRunning, engine.StatusPaused,
	}
	for _, s := range nonTerminal {
		if isTerminalStatus(s) {
			t.Errorf("status %q should NOT be terminal", s)
		}
	}
}
