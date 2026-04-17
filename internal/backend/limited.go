package backend

import "context"

// limitedBackend wraps an inner Backend with a concurrency limiter. The
// wrapper forwards Name() unchanged so metrics, event payloads, and sticky
// routing keys see the same identifier they'd see on the raw backend.
//
// The semaphore is acquired before Run and released after Run returns, whether
// the call succeeded or errored. Context cancellation while blocked on the
// semaphore surfaces as the ctx error — the inner backend is never invoked.
type limitedBackend struct {
	inner   Backend
	limiter *Limiter
}

// NewLimitedBackend wraps b with a per-backend concurrency cap. limit <= 0
// returns b unchanged (no allocation, no indirection). The optional recorder
// observes acquire/release timing; pass nil to disable.
//
// The wrapper is most useful for drivers that shell out to a CLI subprocess
// — the cap prevents running so many subprocesses in parallel that the
// underlying provider rate-limits every one of them into retry storms.
func NewLimitedBackend(b Backend, limit int, recorder Recorder) Backend {
	if limit <= 0 {
		return b
	}
	return &limitedBackend{
		inner:   b,
		limiter: NewLimiter(limit, recorder),
	}
}

func (lb *limitedBackend) Name() string { return lb.inner.Name() }

func (lb *limitedBackend) Run(ctx context.Context, req Request) (*Response, error) {
	name := lb.inner.Name()
	if err := lb.limiter.Acquire(ctx, name); err != nil {
		return nil, err
	}
	defer lb.limiter.Release(name)
	return lb.inner.Run(ctx, req)
}
