package engine

import (
	"testing"
)

// TestApplyEnvOverrides_MaxIterations verifies the RICK_MAX_ITERATION env-var
// replaces a workflow def's hardcoded MaxIterations. Critical because the
// override is the only runtime knob — without it, raising the cap requires
// recompile + redeploy.
func TestApplyEnvOverrides_MaxIterations(t *testing.T) {
	cases := []struct {
		name    string
		envVal  string
		baseMax int
		wantMax int
	}{
		{"unset leaves baseline", "", 3, 3},
		{"empty string leaves baseline", "", 3, 3},
		{"valid positive replaces baseline", "5", 3, 5},
		{"valid larger replaces baseline", "10", 3, 10},
		{"zero is rejected (would silently disable feedback)", "0", 3, 3},
		{"negative is rejected", "-1", 3, 3},
		{"non-numeric is rejected", "abc", 3, 3},
		{"trailing whitespace is rejected (strict parse)", "5 ", 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(maxIterationsEnvVar, tc.envVal)
			def := WorkflowDef{ID: "t", MaxIterations: tc.baseMax}
			got := applyEnvOverrides(def)
			if got.MaxIterations != tc.wantMax {
				t.Errorf("MaxIterations = %d, want %d", got.MaxIterations, tc.wantMax)
			}
			// Other fields untouched.
			if got.ID != "t" {
				t.Errorf("ID mutated: %q", got.ID)
			}
		})
	}
}

// TestEngineRegisterWorkflow_AppliesEnvOverride verifies the override fires
// at the actual call site Engine uses, not just the helper in isolation.
func TestEngineRegisterWorkflow_AppliesEnvOverride(t *testing.T) {
	t.Setenv(maxIterationsEnvVar, "7")
	eng, _, _ := newTestEngine(t)
	eng.RegisterWorkflow(WorkflowDef{ID: "wf-override", MaxIterations: 3})
	def, ok := eng.GetWorkflowDef("wf-override")
	if !ok {
		t.Fatal("workflow not registered")
	}
	if def.MaxIterations != 7 {
		t.Errorf("MaxIterations = %d, want 7", def.MaxIterations)
	}
}

// TestPersonaRunnerRegisterWorkflow_AppliesEnvOverride verifies both
// engine-side and runner-side stores see the same effective cap. If they
// drifted, the engine could decide to escalate while the runner still
// thought there were iterations remaining.
func TestPersonaRunnerRegisterWorkflow_AppliesEnvOverride(t *testing.T) {
	t.Setenv(maxIterationsEnvVar, "7")
	runner, _, _, _ := newTestPersonaRunner(t)
	runner.RegisterWorkflow(WorkflowDef{ID: "wf-override-runner", MaxIterations: 3})
	def, ok := runner.resolver.getWorkflowDef("wf-override-runner")
	if !ok {
		t.Fatal("workflow not registered in resolver")
	}
	if def.MaxIterations != 7 {
		t.Errorf("MaxIterations = %d, want 7", def.MaxIterations)
	}
}
