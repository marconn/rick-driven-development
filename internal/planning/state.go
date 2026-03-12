package planning

import (
	"encoding/json"
	"strings"
	"sync"
)

// PlanningState holds per-workflow planning context shared between handlers
// in the same process. Keyed by correlation ID.
type PlanningState struct {
	mu     sync.RWMutex
	states map[string]*WorkflowPlan
}

// NewPlanningState creates a new shared state store.
func NewPlanningState() *PlanningState {
	return &PlanningState{states: make(map[string]*WorkflowPlan)}
}

// Get returns the plan state for a correlation, creating if absent.
func (ps *PlanningState) Get(correlationID string) *WorkflowPlan {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if wp, ok := ps.states[correlationID]; ok {
		return wp
	}
	wp := &WorkflowPlan{}
	ps.states[correlationID] = wp
	return wp
}

// Delete removes a workflow's state (cleanup after completion).
func (ps *PlanningState) Delete(correlationID string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.states, correlationID)
}

// WorkflowPlan holds all context accumulated during planning.
type WorkflowPlan struct {
	mu sync.RWMutex

	// Set by confluence-reader
	PageID        string
	BTUTitle      string
	BTUContent    string // plain-text extracted from HTML
	BTURawHTML    string // original HTML for section targeting
	UserTypes     string
	Devices       string
	Microservices []string

	// Set by codebase-researcher
	ResearchFindings string

	// Set by plan-architect
	Plan *TechnicalPlan

	// Set by estimator
	EstimatedPlan *TechnicalPlan
	TotalPoints   int
}

// TechnicalPlan is the structured output from the plan architect.
type TechnicalPlan struct {
	Summary         string       `json:"summary"`
	Tasks           []Task       `json:"tasks"`
	Microservices   []string     `json:"microservices"`
	Risks           []Risk       `json:"risks"`
	Dependencies    []Dependency `json:"dependencies"`
	UserDeviceNotes string       `json:"user_device_notes"`
}

// Task represents a single implementation task.
type Task struct {
	Description   string   `json:"description"`
	Microservice  string   `json:"microservice"`
	Category      string   `json:"category"` // "frontend", "backend", "infra"
	Files         []string `json:"files"`
	Notes         string   `json:"notes,omitempty"`
	Points        int      `json:"points,omitempty"`        // filled by estimator
	Justification string   `json:"justification,omitempty"` // filled by estimator
}

// Risk represents a technical risk.
type Risk struct {
	Description string `json:"description"`
	Probability string `json:"probability"` // "alta", "media", "baja"
	Impact      string `json:"impact"`
	Mitigation  string `json:"mitigation"`
}

// Dependency represents a blocking dependency.
type Dependency struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ParseTechnicalPlan extracts a TechnicalPlan from AI output.
// Tries JSON extraction first, falls back to wrapping raw text.
func ParseTechnicalPlan(output string) (*TechnicalPlan, error) {
	extracted := extractJSON(output)
	if extracted != "" {
		var plan TechnicalPlan
		if err := json.Unmarshal([]byte(extracted), &plan); err == nil {
			return &plan, nil
		}
	}

	// Fallback: wrap raw text as summary (AI didn't produce valid JSON)
	return &TechnicalPlan{Summary: output}, nil
}

// extractJSON finds the last valid JSON object in text.
// Searches from end to find the JSON block (AI output tends to have JSON at the end).
func extractJSON(text string) string {
	cleaned := stripCodeFences(text)

	// Search from the end for the last valid JSON object
	for i := len(cleaned) - 1; i >= 0; i-- {
		if cleaned[i] == '}' {
			depth := 0
			for j := i; j >= 0; j-- {
				switch cleaned[j] {
				case '}':
					depth++
				case '{':
					depth--
					if depth == 0 {
						candidate := cleaned[j : i+1]
						if json.Valid([]byte(candidate)) {
							return candidate
						}
					}
				}
			}
		}
	}

	// Forward search fallback: try json.Decoder from each { position
	for i := 0; i < len(cleaned); i++ {
		if cleaned[i] == '{' {
			dec := json.NewDecoder(strings.NewReader(cleaned[i:]))
			var raw json.RawMessage
			if err := dec.Decode(&raw); err == nil {
				return string(raw)
			}
		}
	}

	return ""
}

// stripCodeFences removes markdown code fence markers.
func stripCodeFences(text string) string {
	lines := strings.Split(text, "\n")
	var result []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

func renderTemplate(tmpl string, data map[string]string) string {
	result := tmpl
	for key, value := range data {
		result = strings.ReplaceAll(result, "{{."+key+"}}", value)
	}
	return result
}

func splitParagraphs(text string) []string {
	var paragraphs []string
	for _, p := range strings.Split(text, "\n\n") {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			paragraphs = append(paragraphs, trimmed)
		}
	}
	if len(paragraphs) == 0 && strings.TrimSpace(text) != "" {
		return []string{strings.TrimSpace(text)}
	}
	return paragraphs
}

func escapeHTML(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
	)
	return replacer.Replace(s)
}

func splitWords(s string) []string {
	var words []string
	word := ""
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' {
			if word != "" {
				words = append(words, word)
				word = ""
			}
		} else {
			word += string(r)
		}
	}
	if word != "" {
		words = append(words, word)
	}
	return words
}
