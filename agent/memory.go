package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Memory is a single persistent memory entry.
type Memory struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	Category  string    `json:"category"` // user, preference, environment, workflow
	CreatedAt time.Time `json:"created_at"`
}

// MemoryStore persists operator memories to ~/.config/rick/memories.json.
type MemoryStore struct {
	path     string
	mu       sync.RWMutex
	memories []Memory
	version  atomic.Int64
}

// NewMemoryStore creates a store backed by ~/.config/rick/memories.json.
func NewMemoryStore() (*MemoryStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("memory: resolve home dir: %w", err)
	}
	dir := filepath.Join(home, ".config", "rick")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("memory: create config dir: %w", err)
	}
	ms := &MemoryStore{path: filepath.Join(dir, "memories.json")}
	ms.load()
	// Start version at 1 if memories were loaded so the first session injects them.
	if len(ms.memories) > 0 {
		ms.version.Store(1)
	}
	return ms, nil
}

func (ms *MemoryStore) load() {
	data, err := os.ReadFile(ms.path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &ms.memories)
}

func (ms *MemoryStore) save() error {
	data, err := json.MarshalIndent(ms.memories, "", "  ")
	if err != nil {
		return fmt.Errorf("memory: marshal: %w", err)
	}
	return os.WriteFile(ms.path, data, 0o644)
}

// Version returns a monotonically increasing counter bumped on each mutation.
func (ms *MemoryStore) Version() int64 {
	return ms.version.Load()
}

// Add persists a new memory entry.
func (ms *MemoryStore) Add(content, category string) (Memory, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if category == "" {
		category = "general"
	}

	m := Memory{
		ID:        uuid.New().String()[:8],
		Content:   strings.TrimSpace(content),
		Category:  strings.ToLower(strings.TrimSpace(category)),
		CreatedAt: time.Now().UTC(),
	}
	ms.memories = append(ms.memories, m)
	if err := ms.save(); err != nil {
		// Roll back.
		ms.memories = ms.memories[:len(ms.memories)-1]
		return Memory{}, err
	}
	ms.version.Add(1)
	return m, nil
}

// List returns all stored memories.
func (ms *MemoryStore) List() []Memory {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	result := make([]Memory, len(ms.memories))
	copy(result, ms.memories)
	return result
}

// Delete removes a memory by ID or ID prefix.
func (ms *MemoryStore) Delete(id string) bool {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	for i, m := range ms.memories {
		if m.ID == id || strings.HasPrefix(m.ID, id) {
			ms.memories = append(ms.memories[:i], ms.memories[i+1:]...)
			ms.save() //nolint:errcheck
			ms.version.Add(1)
			return true
		}
	}
	return false
}

// Search returns memories matching a query (case-insensitive substring).
func (ms *MemoryStore) Search(query string) []Memory {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	q := strings.ToLower(query)
	var results []Memory
	for _, m := range ms.memories {
		if strings.Contains(strings.ToLower(m.Content), q) ||
			strings.Contains(m.Category, q) {
			results = append(results, m)
		}
	}
	return results
}

// FormatForPrompt renders all memories as a block suitable for injection
// into the conversation. Returns empty string if no memories exist.
func (ms *MemoryStore) FormatForPrompt() string {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	if len(ms.memories) == 0 {
		return ""
	}

	// Group by category for readability.
	grouped := make(map[string][]Memory)
	for _, m := range ms.memories {
		grouped[m.Category] = append(grouped[m.Category], m)
	}

	var sb strings.Builder
	sb.WriteString("[Operator Memory — saved from previous sessions]\n")
	for cat, mems := range grouped {
		fmt.Fprintf(&sb, "\n### %s\n", cat)
		for _, m := range mems {
			fmt.Fprintf(&sb, "- %s\n", m.Content)
		}
	}
	return sb.String()
}
