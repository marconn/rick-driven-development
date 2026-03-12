package jiraplanner

import (
	"strings"
	"sync"
	"testing"
)

// --- PlanningState ---

func TestPlanningStateGet_CreatesOnMiss(t *testing.T) {
	state := NewPlanningState()
	wd := state.Get("corr-1")
	if wd == nil {
		t.Fatal("Get returned nil for new correlation")
	}
}

func TestPlanningStateGet_ReturnsSameInstance(t *testing.T) {
	state := NewPlanningState()
	a := state.Get("corr-1")
	a.PageTitle = "hello"

	b := state.Get("corr-1")
	if b.PageTitle != "hello" {
		t.Errorf("expected same instance, got PageTitle=%q", b.PageTitle)
	}
}

func TestPlanningStateGet_IsolatesCorrelations(t *testing.T) {
	state := NewPlanningState()
	state.Get("corr-1").PageTitle = "one"
	state.Get("corr-2").PageTitle = "two"

	if state.Get("corr-1").PageTitle != "one" {
		t.Error("corr-1 was contaminated by corr-2")
	}
	if state.Get("corr-2").PageTitle != "two" {
		t.Error("corr-2 was contaminated by corr-1")
	}
}

func TestPlanningStateDelete(t *testing.T) {
	state := NewPlanningState()
	state.Get("corr-1").PageTitle = "to delete"
	state.Delete("corr-1")

	fresh := state.Get("corr-1")
	if fresh.PageTitle != "" {
		t.Errorf("Delete did not remove state, PageTitle=%q", fresh.PageTitle)
	}
}

func TestPlanningStateConcurrentAccess(t *testing.T) {
	// Verify no race conditions under concurrent Get calls.
	state := NewPlanningState()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wd := state.Get("shared-corr")
			wd.mu.Lock()
			wd.PageTitle = "concurrent"
			wd.mu.Unlock()
		}()
	}
	wg.Wait()
}

// --- ParseProjectPlan ---

func TestParseProjectPlan_ValidJSON(t *testing.T) {
	input := `{"goal":"build API","epic_title":"API Epic","epic_description":"desc","tasks":[{"title":"Task 1","description":"do it","priority":1,"story_points":3}],"risks":[],"dependencies":[]}`

	plan, err := ParseProjectPlan(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Goal != "build API" {
		t.Errorf("goal=%q, want %q", plan.Goal, "build API")
	}
	if plan.EpicTitle != "API Epic" {
		t.Errorf("epic_title=%q, want %q", plan.EpicTitle, "API Epic")
	}
	if len(plan.Tasks) != 1 {
		t.Fatalf("tasks len=%d, want 1", len(plan.Tasks))
	}
	if plan.Tasks[0].Title != "Task 1" {
		t.Errorf("task title=%q, want %q", plan.Tasks[0].Title, "Task 1")
	}
	if plan.Tasks[0].StoryPoints != 3 {
		t.Errorf("story_points=%v, want 3", plan.Tasks[0].StoryPoints)
	}
}

func TestParseProjectPlan_JSONAfterProse(t *testing.T) {
	// AI typically outputs analysis text before the JSON block.
	input := `Here is the project plan based on the document:

Some analysis text here.

{"goal":"migrate DB","epic_title":"DB Migration","epic_description":"","tasks":[],"risks":[],"dependencies":[]}`

	plan, err := ParseProjectPlan(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Goal != "migrate DB" {
		t.Errorf("goal=%q, want %q", plan.Goal, "migrate DB")
	}
}

func TestParseProjectPlan_WrappedInCodeFence(t *testing.T) {
	input := "```json\n{\"goal\":\"test\",\"epic_title\":\"T\",\"epic_description\":\"\",\"tasks\":[],\"risks\":[],\"dependencies\":[]}\n```"

	plan, err := ParseProjectPlan(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Goal != "test" {
		t.Errorf("goal=%q, want %q", plan.Goal, "test")
	}
}

func TestParseProjectPlan_NoJSON(t *testing.T) {
	_, err := ParseProjectPlan("no json here at all")
	if err == nil {
		t.Error("expected error for input with no JSON, got nil")
	}
}

func TestParseProjectPlan_TaskPrioritiesAndPoints(t *testing.T) {
	input := `{"goal":"g","epic_title":"e","epic_description":"d","tasks":[
		{"title":"critical","description":"","priority":1,"story_points":8},
		{"title":"normal","description":"","priority":3,"story_points":2}
	],"risks":[{"description":"risk1","probability":"alta","mitigation":"m1"}],"dependencies":[]}`

	plan, err := ParseProjectPlan(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Tasks) != 2 {
		t.Fatalf("tasks len=%d, want 2", len(plan.Tasks))
	}
	if plan.Tasks[0].Priority != 1 || plan.Tasks[0].StoryPoints != 8 {
		t.Errorf("first task: priority=%d points=%v, want 1/8", plan.Tasks[0].Priority, plan.Tasks[0].StoryPoints)
	}
	if len(plan.Risks) != 1 || plan.Risks[0].Probability != "alta" {
		t.Errorf("risks not parsed correctly: %+v", plan.Risks)
	}
}

// --- extractJSON (internal, tested via ParseProjectPlan edge cases) ---

func TestExtractJSON_MultipleObjects_TakesLast(t *testing.T) {
	// When AI outputs multiple JSON blobs, we want the last (final answer).
	input := `{"intermediate":"value"} some text {"goal":"final","epic_title":"E","epic_description":"","tasks":[],"risks":[],"dependencies":[]}`

	plan, err := ParseProjectPlan(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Goal != "final" {
		t.Errorf("expected last JSON object, got goal=%q", plan.Goal)
	}
}

func TestExtractJSON_NestedObjects(t *testing.T) {
	// Nested JSON (tasks array with objects) must be extracted correctly.
	input := `{"goal":"g","epic_title":"e","epic_description":"d","tasks":[{"title":"t","description":"d","priority":2,"story_points":5}],"risks":[],"dependencies":[]}`

	plan, err := ParseProjectPlan(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Tasks) != 1 {
		t.Errorf("tasks=%d, want 1", len(plan.Tasks))
	}
}

// --- stripCodeFences ---

func TestStripCodeFences(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no fences",
			input: "hello\nworld",
			want:  "hello\nworld",
		},
		{
			name:  "json fence",
			input: "```json\n{}\n```",
			want:  "{}",
		},
		{
			name:  "plain fence",
			input: "```\ncontent\n```",
			want:  "content",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripCodeFences(tt.input)
			if got != tt.want {
				t.Errorf("stripCodeFences(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- truncateStr ---

func TestTruncateStr(t *testing.T) {
	if got := truncateStr("hello", 10); got != "hello" {
		t.Errorf("truncateStr short: got %q", got)
	}
	long := strings.Repeat("a", 200)
	if got := truncateStr(long, 50); len(got) != 50 { // maxLen total including "..."
		t.Errorf("truncateStr long: len=%d, want 50", len(got))
	}
	if !strings.HasSuffix(truncateStr(long, 50), "...") {
		t.Error("truncateStr should end with ...")
	}
}
