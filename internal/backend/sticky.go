package backend

import (
	"context"
	"hash/fnv"
)

// stickyKeyCtxKey is the context key for the sticky backend key. Using an
// unexported struct type avoids collisions with other packages that stash
// values in the same context.
type stickyKeyCtxKey struct{}

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
