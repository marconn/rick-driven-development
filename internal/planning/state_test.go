package planning

import "testing"

func TestParseTechnicalPlan(t *testing.T) {
	tests := []struct {
		name      string
		output    string
		wantTasks int
		wantErr   bool
	}{
		{
			name: "valid JSON in text",
			output: `Here is the plan:
` + "```json" + `
{
  "summary": "Implement upload functionality",
  "tasks": [
    {"description": "Add upload card", "microservice": "practice-web", "category": "frontend", "files": ["src/components/Upload.vue"]},
    {"description": "Add endpoint", "microservice": "backend-api", "category": "backend", "files": ["internal/handler/upload.go"]}
  ],
  "microservices": ["practice-web", "backend-api"],
  "risks": [],
  "dependencies": [],
  "user_device_notes": "Works on all devices"
}
` + "```",
			wantTasks: 2,
		},
		{
			name:      "no JSON fallback to summary",
			output:    "Just a text plan without JSON",
			wantTasks: 0,
		},
		{
			name: "JSON without code fence",
			output: `Plan: {"summary": "test", "tasks": [{"description": "task1", "microservice": "svc", "category": "backend", "files": []}], "microservices": [], "risks": [], "dependencies": [], "user_device_notes": ""}`,
			wantTasks: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := ParseTechnicalPlan(tt.output)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if len(plan.Tasks) != tt.wantTasks {
				t.Errorf("tasks = %d, want %d", len(plan.Tasks), tt.wantTasks)
			}
		})
	}
}

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`before {"key": "value"} after`, `{"key": "value"}`},
		{`no json here`, ""},
		{`nested {"a": {"b": 1}} end`, `{"a": {"b": 1}}`},
		{`unclosed {`, ""},
	}

	for _, tt := range tests {
		got := extractJSON(tt.input)
		if got != tt.want {
			t.Errorf("extractJSON(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPlanningStateGetCreate(t *testing.T) {
	state := NewPlanningState()

	wp1 := state.Get("corr-1")
	if wp1 == nil {
		t.Fatal("Get returned nil")
	}
	wp1.BTUTitle = "Test BTU"

	wp2 := state.Get("corr-1")
	if wp2.BTUTitle != "Test BTU" {
		t.Errorf("expected same state, got title=%q", wp2.BTUTitle)
	}

	wp3 := state.Get("corr-2")
	if wp3.BTUTitle != "" {
		t.Error("expected empty state for new correlation")
	}
}

func TestPlanningStateDelete(t *testing.T) {
	state := NewPlanningState()

	state.Get("corr-1").BTUTitle = "Test"
	state.Delete("corr-1")

	wp := state.Get("corr-1")
	if wp.BTUTitle != "" {
		t.Error("expected clean state after delete")
	}
}
