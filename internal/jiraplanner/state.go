package jiraplanner

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// PlanningState holds per-workflow data shared between handlers in the
// plan-jira pipeline. Thread-safe: handlers running in the same correlation
// read/write through this instead of passing payloads via events.
type PlanningState struct {
	mu     sync.RWMutex
	states map[string]*WorkflowData
}

// NewPlanningState creates an empty state store.
func NewPlanningState() *PlanningState {
	return &PlanningState{states: make(map[string]*WorkflowData)}
}

// Get returns the WorkflowData for a correlation, creating it lazily.
func (ps *PlanningState) Get(correlationID string) *WorkflowData {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if wd, ok := ps.states[correlationID]; ok {
		return wd
	}
	wd := &WorkflowData{}
	ps.states[correlationID] = wd
	return wd
}

// Delete removes the state for a correlation (cleanup after workflow ends).
func (ps *PlanningState) Delete(correlationID string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.states, correlationID)
}

// WorkflowData holds Confluence page content and the generated plan for a
// single plan-jira workflow run.
type WorkflowData struct {
	mu          sync.RWMutex
	PageID      string
	PageTitle   string
	PageContent string
	Plan        *ProjectPlan
}

// ProjectPlan is the structured output from the project-manager AI.
type ProjectPlan struct {
	Goal         string     `json:"goal"`
	EpicTitle    string     `json:"epic_title"`
	EpicDesc     string     `json:"epic_description"`
	Tasks        []JiraTask `json:"tasks"`
	Risks        []Risk     `json:"risks"`
	Dependencies []Dep      `json:"dependencies"`
}

// JiraTask represents a single task to create in Jira.
type JiraTask struct {
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Priority     int      `json:"priority"`
	StoryPoints  float64  `json:"story_points"`
	Tags         []string `json:"tags,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
}

// Risk is an identified project risk with mitigation.
type Risk struct {
	Description string `json:"description"`
	Probability string `json:"probability"`
	Mitigation  string `json:"mitigation"`
}

// Dep is an external dependency the project relies on.
type Dep struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ParseProjectPlan extracts a ProjectPlan from LLM output that may contain
// markdown fences and surrounding text.
func ParseProjectPlan(output string) (*ProjectPlan, error) {
	extracted := extractJSON(output)
	if extracted == "" {
		return nil, fmt.Errorf("no valid JSON object found in output")
	}
	var plan ProjectPlan
	if err := json.Unmarshal([]byte(extracted), &plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// extractJSON finds the last valid JSON object in text.
func extractJSON(text string) string {
	cleaned := stripCodeFences(text)

	// Scan backwards for the last closing brace — LLMs often append commentary.
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

	// Fallback: forward scan with streaming decoder.
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

func stripCodeFences(text string) string {
	lines := strings.Split(text, "\n")
	var result []string
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

func renderTemplate(tmpl string, vars map[string]string) string {
	for k, v := range vars {
		tmpl = strings.ReplaceAll(tmpl, "{{."+k+"}}", v)
	}
	return tmpl
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
