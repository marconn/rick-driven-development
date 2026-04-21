package backend

import (
	"context"
	"hash/fnv"
)

// stickyKeyCtxKey is the context key for the sticky backend key. Using an
// unexported struct type avoids collisions with other packages that stash
// values in the same context.
type stickyKeyCtxKey struct{}

// rotateOffsetCtxKey is the context key for the sticky rotation offset.
// Paired with stickyKey, it shifts the hashed index deterministically so a
// retry on attempt N lands on a different RoundRobin slot than attempt 0.
// Keeping this separate from the key string lets callers fold the attempt
// counter in without changing the key (which is used for logs /
// observability attribution).
type rotateOffsetCtxKey struct{}

// WithStickyKey annotates ctx with a key that pins RoundRobin selection to a
// deterministic backend. Same key → same backend across retries and iterations.
// Empty key is ignored (caller fall back to counter-based rotation).
//
// Intended callers: review-phase AI handlers. A typical key is
// "{correlationID}:{persona}" so that a single reviewer/qa persona stays on
// one backend across all iterations of a feedback loop — the developer then
// gets coherent, non-oscillating review feedback instead of three different
// backends flagging three different issues on three iterations.
func WithStickyKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, stickyKeyCtxKey{}, key)
}

// WithRotateOffset returns ctx annotated with a positive offset that shifts
// the RoundRobin sticky slot deterministically. The final slot is
// (stickyIndex(key, n) + offset) % n, which guarantees a different backend
// when offset ∈ [1, n-1] — whereas rehashing the key (e.g. appending a
// "retry1" suffix) has a 1/n chance of colliding back to the same slot.
//
// Intended caller: AIHandler on auto-retry. attempt=1 picks the slot to the
// right of the original; attempt=2 picks two slots right; attempts ≥ n wrap
// and may revisit, which is acceptable given MaxAutoRetriesPerPersona is
// capped at 1 today.
//
// Negative offsets are clamped to 0. Zero offset is a no-op, so callers can
// unconditionally pass the attempt counter without branching.
func WithRotateOffset(ctx context.Context, offset int) context.Context {
	if offset <= 0 {
		return ctx
	}
	return context.WithValue(ctx, rotateOffsetCtxKey{}, offset)
}

// stickyKey returns the sticky key stored in ctx, or "" if none is set.
func stickyKey(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(stickyKeyCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// StickyKeyFromContext is the exported accessor for the sticky key.
// Call sites outside this package (tests, observability middleware) that
// need to see which key a RoundRobin will hash can read it without going
// through Run. Returns "" when none is set.
func StickyKeyFromContext(ctx context.Context) string { return stickyKey(ctx) }

// rotateOffset returns the sticky rotation offset stored in ctx, or 0 if
// none is set.
func rotateOffset(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	if v, ok := ctx.Value(rotateOffsetCtxKey{}).(int); ok && v > 0 {
		return v
	}
	return 0
}

// RotateOffsetFromContext is the exported accessor for the rotation offset,
// mirroring StickyKeyFromContext. Lets tests and diagnostic middleware verify
// that the auto-retry rotation is actually being applied.
func RotateOffsetFromContext(ctx context.Context) int { return rotateOffset(ctx) }

// stickyIndex hashes key into a stable index in [0, n). Uses FNV-1a because
// it's fast, deterministic across runs, and the key space is small enough
// that cryptographic quality doesn't matter.
func stickyIndex(key string, n int) int {
	if n <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum64() % uint64(n))
}
