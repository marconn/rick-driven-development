package handler

import (
	"errors"
	"fmt"
	"sync"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// Registry manages handler registration and lookup.
// It is safe for concurrent use. The byEvent index enables O(1) dispatch
// without scanning all handlers on every event.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Handler
	byEvent  map[event.Type][]Handler
}

// NewRegistry creates an empty handler registry.
func NewRegistry() *Registry {
	return &Registry{
		handlers: make(map[string]Handler),
		byEvent:  make(map[event.Type][]Handler),
	}
}

// Register adds a handler to the registry.
// Returns an error if a handler with the same name is already registered.
// If the handler implements LifecycleHook, Init is called before registration completes.
// If Init fails, the handler is not registered.
func (r *Registry) Register(h Handler) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := h.Name()
	if _, exists := r.handlers[name]; exists {
		return fmt.Errorf("handler: %q already registered", name)
	}

	if lh, ok := h.(LifecycleHook); ok {
		if err := lh.Init(); err != nil {
			return fmt.Errorf("handler: init %q: %w", name, err)
		}
	}

	r.handlers[name] = h
	for _, eventType := range h.Subscribes() {
		r.byEvent[eventType] = append(r.byEvent[eventType], h)
	}
	return nil
}

// Unregister removes a handler from the registry.
// If the handler implements LifecycleHook, Shutdown is called before removal.
func (r *Registry) Unregister(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	h, exists := r.handlers[name]
	if !exists {
		return fmt.Errorf("handler: %q not registered", name)
	}

	if lh, ok := h.(LifecycleHook); ok {
		if err := lh.Shutdown(); err != nil {
			return fmt.Errorf("handler: shutdown %q: %w", name, err)
		}
	}

	for _, eventType := range h.Subscribes() {
		handlers := r.byEvent[eventType]
		for i, existing := range handlers {
			if existing.Name() == name {
				r.byEvent[eventType] = append(handlers[:i], handlers[i+1:]...)
				break
			}
		}
		if len(r.byEvent[eventType]) == 0 {
			delete(r.byEvent, eventType)
		}
	}

	delete(r.handlers, name)
	return nil
}

// Replace atomically swaps an existing handler with a new one.
// The new handler must have the same name as the existing handler.
// If the new handler implements LifecycleHook, Init is called first — if it
// fails, the old handler is preserved and an error is returned (no rollback
// needed since nothing was mutated yet).
// If the old handler implements LifecycleHook, Shutdown is called after the
// swap succeeds; shutdown failure is best-effort and does not reverse the swap.
func (r *Registry) Replace(name string, newHandler Handler) error {
	if newHandler.Name() != name {
		return fmt.Errorf("handler: replacement name %q does not match existing %q", newHandler.Name(), name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	old, exists := r.handlers[name]
	if !exists {
		return fmt.Errorf("handler: %q not registered", name)
	}

	// Init new handler before mutating state — keeps old handler intact on failure.
	if lh, ok := newHandler.(LifecycleHook); ok {
		if err := lh.Init(); err != nil {
			return fmt.Errorf("handler: init replacement %q: %w", name, err)
		}
	}

	// Remove old handler from byEvent index.
	for _, eventType := range old.Subscribes() {
		handlers := r.byEvent[eventType]
		for i, existing := range handlers {
			if existing.Name() == name {
				r.byEvent[eventType] = append(handlers[:i], handlers[i+1:]...)
				break
			}
		}
		if len(r.byEvent[eventType]) == 0 {
			delete(r.byEvent, eventType)
		}
	}

	// Register new handler in both indexes.
	r.handlers[name] = newHandler
	for _, eventType := range newHandler.Subscribes() {
		r.byEvent[eventType] = append(r.byEvent[eventType], newHandler)
	}

	// Shutdown old handler after the swap — best-effort, failure is non-fatal.
	if lh, ok := old.(LifecycleHook); ok {
		_ = lh.Shutdown() //nolint:errcheck // best-effort; new handler is already live
	}

	return nil
}

// Get returns a handler by name.
func (r *Registry) Get(name string) (Handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[name]
	return h, ok
}

// HandlersFor returns all handlers subscribed to a specific event type.
// Returns a copy of the slice to avoid data races if the caller iterates
// while another goroutine mutates the registry.
func (r *Registry) HandlersFor(eventType event.Type) []Handler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	handlers := r.byEvent[eventType]
	result := make([]Handler, len(handlers))
	copy(result, handlers)
	return result
}

// All returns all registered handlers as a snapshot slice.
func (r *Registry) All() []Handler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Handler, 0, len(r.handlers))
	for _, h := range r.handlers {
		result = append(result, h)
	}
	return result
}

// Names returns the names of all registered handlers.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.handlers))
	for name := range r.handlers {
		names = append(names, name)
	}
	return names
}

// ShutdownAll calls Shutdown on all handlers that implement LifecycleHook.
// It attempts to shut down every handler even if some fail, returning all
// errors joined together. This ensures best-effort cleanup on process exit.
func (r *Registry) ShutdownAll() error {
	r.mu.RLock()
	handlers := make([]Handler, 0, len(r.handlers))
	for _, h := range r.handlers {
		handlers = append(handlers, h)
	}
	r.mu.RUnlock()

	var errs []error
	for _, h := range handlers {
		if lh, ok := h.(LifecycleHook); ok {
			if err := lh.Shutdown(); err != nil {
				errs = append(errs, fmt.Errorf("handler: shutdown %q: %w", h.Name(), err))
			}
		}
	}
	return errors.Join(errs...)
}
