package engine

import (
	"sync"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/handler"
)

// pauseController manages workflow pause/resume/cancel state and blocked
// dispatch recording. Thread-safe.
type pauseController struct {
	mu      sync.RWMutex
	paused  map[string]bool             // correlationID → paused
	blocked map[string][]blockedDispatch // correlationID → pending dispatches
}

func newPauseController() *pauseController {
	return &pauseController{
		paused:  make(map[string]bool),
		blocked: make(map[string][]blockedDispatch),
	}
}

// isPaused returns true if the given correlation is paused.
func (p *pauseController) isPaused(correlationID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.paused[correlationID]
}

// addBlocked records a blocked dispatch for replay on resume.
func (p *pauseController) addBlocked(correlationID string, h handler.Handler, env event.Envelope) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blocked[correlationID] = append(p.blocked[correlationID], blockedDispatch{handler: h, env: env})
}

// pause marks a correlation as paused.
func (p *pauseController) pause(correlationID string) {
	p.mu.Lock()
	p.paused[correlationID] = true
	p.mu.Unlock()
}

// resume unpauses a correlation and returns any blocked dispatches for replay.
func (p *pauseController) resume(correlationID string) []blockedDispatch {
	p.mu.Lock()
	delete(p.paused, correlationID)
	replay := p.blocked[correlationID]
	delete(p.blocked, correlationID)
	p.mu.Unlock()
	return replay
}

// markCancelled marks a correlation as permanently paused (cancelled) and
// discards any blocked dispatches.
func (p *pauseController) markCancelled(correlationID string) {
	p.mu.Lock()
	p.paused[correlationID] = true
	delete(p.blocked, correlationID)
	p.mu.Unlock()
}
