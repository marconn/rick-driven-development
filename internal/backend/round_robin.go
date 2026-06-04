package backend

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
)

// RoundRobin distributes Run calls across a fixed list of backends using
// an atomic counter. Each call picks the next backend in order, wrapping
// around at the end. Used by review-phase handlers so that a wedged or
// rate-limited CLI only affects 1/N of review invocations instead of all.
type RoundRobin struct {
	backends []Backend
	counter  atomic.Uint64
	name     string
}

// NewRoundRobin constructs a RoundRobin over the given backends. The caller
// must provide at least one backend. The Name() of the returned backend is
// "round-robin(a,b,c)" where a,b,c are the underlying backend names.
func NewRoundRobin(backends ...Backend) (*RoundRobin, error) {
	if len(backends) == 0 {
		return nil, fmt.Errorf("round-robin: at least one backend required")
	}
	names := make([]string, 0, len(backends))
	for _, b := range backends {
		names = append(names, b.Name())
	}
	return &RoundRobin{
		backends: backends,
		name:     fmt.Sprintf("round-robin(%s)", strings.Join(names, ",")),
	}, nil
}

// Name returns a composite identifier, e.g. "round-robin(claude,gemini,codex)".
// This is the rotation's *identity* for registry/config use, not the backend
// that runs any given call. For per-call attribution, callers resolve the
// concrete inner backend via Select (or backend.Resolve) and read its Name —
// the AIHandler does this so AIRequestSent/AIResponseReceived record the CLI
// that actually executed, not the composite.
func (r *RoundRobin) Name() string { return r.name }

// Select returns the concrete inner backend this RoundRobin would dispatch to
// for ctx, advancing rotation state exactly as Run would. Callers that need to
// attribute events BEFORE invoking the backend (AIRequestSent/AIRequestStarted
// are emitted before Run) resolve the backend here and then invoke the returned
// backend's Run directly.
//
// CONTRACT: the returned backend MUST be the one the caller invokes. On the
// non-sticky atomic-counter path Select advances the counter, so calling Select
// and then RoundRobin.Run would advance twice and run a different slot than was
// attributed. The sticky path is purely deterministic (no counter advance), so
// Select is idempotent there.
func (r *RoundRobin) Select(ctx context.Context) Backend {
	return r.backends[r.selectIndex(ctx)]
}

// selectIndex picks the slot for ctx. Sticky key (+ optional rotation offset)
// is deterministic and advances no shared state; absent a key it advances the
// atomic counter. Centralized here so Select and Run share one selection rule
// and cannot drift.
func (r *RoundRobin) selectIndex(ctx context.Context) int {
	n := len(r.backends)
	if key := stickyKey(ctx); key != "" {
		idx := stickyIndex(key, n)
		// Auto-retry path: the AIHandler passes a non-zero rotation offset
		// so the retry lands on a different slot deterministically instead
		// of re-hashing a modified key (which has a 1/n chance of colliding
		// back to the same slot when n is small).
		if off := rotateOffset(ctx); off > 0 {
			idx = (idx + off) % n
		}
		return idx
	}
	return int((r.counter.Add(1) - 1) % uint64(n))
}

// Run selects a backend and delegates. When the context carries a sticky key
// (see WithStickyKey), selection is deterministic based on the key — same key
// always maps to the same backend. Otherwise selection is per-call via the
// atomic counter: two concurrent review handlers land on different backends
// (provided len(backends) > 1).
//
// Sticky selection exists so that reviewer/qa personas stay on one backend
// across feedback-loop iterations, avoiding the "three iterations, three
// different backends, three different sets of issues, developer never
// converges" failure mode seen in production.
//
// Run is retained for callers that dispatch through the rotation directly
// (tests, non-attributing call sites). The AIHandler instead resolves the
// concrete backend via Select and invokes it directly to attribute events.
func (r *RoundRobin) Run(ctx context.Context, req Request) (*Response, error) {
	return r.backends[r.selectIndex(ctx)].Run(ctx, req)
}

// Backends exposes the underlying list for inspection (primarily for logs
// and tests). The returned slice aliases internal state — do not mutate.
//
// 0008's required-knowledge pinning ranges over these and calls Capabilities()
// per member to find the capable subset — the conservative aggregate from
// Capabilities() below is the wrong lens for "pin to a capable member".
func (r *RoundRobin) Backends() []Backend { return r.backends }

// Capabilities returns the conservative INTERSECTION of its members'
// capabilities: a caller may only assume a feature that EVERY rotation member
// supports, because any member can serve a given call. So
// round-robin(codex,opencode,claude).Capabilities().MCP is false even though
// claude alone supports MCP — sending an MCP tool config to the rotation would
// no-op on 2 of 3 backends. (Per-member capabilities for capable-member
// selection are read via Backends().)
func (r *RoundRobin) Capabilities() Capabilities {
	caps := Capabilities{
		MCP:             true,
		SystemPrompt:    true,
		SessionResume:   true,
		TokenAccounting: true,
		ReasoningEffort: true,
	}
	for _, b := range r.backends {
		c := b.Capabilities()
		caps.MCP = caps.MCP && c.MCP
		caps.SystemPrompt = caps.SystemPrompt && c.SystemPrompt
		caps.SessionResume = caps.SessionResume && c.SessionResume
		caps.TokenAccounting = caps.TokenAccounting && c.TokenAccounting
		caps.ReasoningEffort = caps.ReasoningEffort && c.ReasoningEffort
	}
	return caps
}
