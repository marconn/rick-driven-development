package engine

import "sync"

// hookLookup provides read access to before-hooks. Used by workflowResolver
// to merge hooks into DAG predecessor lists without depending on hookRegistry directly.
type hookLookup interface {
	hooksFor(name string) []string
}

// hookRegistry manages before-hooks: persona name → additional personas that
// must complete first. Thread-safe.
type hookRegistry struct {
	mu    sync.RWMutex
	hooks map[string][]string
}

func newHookRegistry(initial map[string][]string) *hookRegistry {
	if initial == nil {
		initial = make(map[string][]string)
	}
	return &hookRegistry{hooks: initial}
}

// hooksFor returns the hook personas for the given handler name.
func (hr *hookRegistry) hooksFor(name string) []string {
	hr.mu.RLock()
	defer hr.mu.RUnlock()
	return hr.hooks[name]
}

// register adds before-hooks for a persona (additive).
func (hr *hookRegistry) register(persona string, hookPersonas ...string) {
	hr.mu.Lock()
	hr.hooks[persona] = append(hr.hooks[persona], hookPersonas...)
	hr.mu.Unlock()
}

// unregister removes a specific hook handler from a persona's before-hook list.
func (hr *hookRegistry) unregister(persona, hookName string) {
	hr.mu.Lock()
	defer hr.mu.Unlock()
	hooks := hr.hooks[persona]
	for i, h := range hooks {
		if h == hookName {
			hr.hooks[persona] = append(hooks[:i], hooks[i+1:]...)
			break
		}
	}
	if len(hr.hooks[persona]) == 0 {
		delete(hr.hooks, persona)
	}
}
