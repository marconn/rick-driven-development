package engine

import (
	"context"
	"sync"
)

// correlationContexts manages per-correlation contexts for cancellation
// propagation. Created on first dispatch for a correlation, cancelled on
// WorkflowCancelled or Close(). Thread-safe.
type correlationContexts struct {
	mu       sync.Mutex
	contexts map[string]corrCtxEntry
}

func newCorrelationContexts() *correlationContexts {
	return &correlationContexts{
		contexts: make(map[string]corrCtxEntry),
	}
}

// get returns a per-correlation context, creating one (as a child of parent) if needed.
func (cc *correlationContexts) get(parent context.Context, correlationID string) context.Context {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if entry, ok := cc.contexts[correlationID]; ok {
		return entry.ctx
	}
	ctx, cancel := context.WithCancel(parent)
	cc.contexts[correlationID] = corrCtxEntry{ctx: ctx, cancel: cancel}
	return ctx
}

// cancel cancels the per-correlation context, terminating in-flight dispatches
// (AI backend subprocesses) for that workflow.
func (cc *correlationContexts) cancel(correlationID string) {
	cc.mu.Lock()
	entry, ok := cc.contexts[correlationID]
	if ok {
		delete(cc.contexts, correlationID)
	}
	cc.mu.Unlock()
	if ok {
		entry.cancel()
	}
}

// cancelAll cancels all per-correlation contexts. Used during Close().
func (cc *correlationContexts) cancelAll() {
	cc.mu.Lock()
	for corrID, entry := range cc.contexts {
		entry.cancel()
		delete(cc.contexts, corrID)
	}
	cc.mu.Unlock()
}
