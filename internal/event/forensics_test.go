package event

import (
	"encoding/json"
	"testing"
)

// TestVerdictPayloadBackCompatReplay asserts that pre-PR VerdictPayload JSON
// (without the verdict_source field) deserializes cleanly with the
// VerdictSource zero value, AND that legacy `phase`/`source_phase` keys with
// verb values translate to handler-name persona fields. This locks both the
// optional-field contract and the 2026-05-05 phase→persona rename's tolerant
// decoder; any future rename or non-pointer enum change that breaks replay
// will fail this test.
func TestVerdictPayloadBackCompatReplay(t *testing.T) {
	prePR := []byte(`{"phase":"develop","source_phase":"review","outcome":"pass","summary":"passed review"}`)

	var got VerdictPayload
	if err := json.Unmarshal(prePR, &got); err != nil {
		t.Fatalf("unmarshal pre-PR VerdictPayload: %v", err)
	}
	// Legacy phase verbs must translate into handler-name personas on read.
	if got.Persona != "developer" || got.SourcePersona != "reviewer" {
		t.Errorf("legacy phase verb did not translate to handler name: %+v", got)
	}
	if got.Outcome != VerdictPass {
		t.Errorf("outcome lost: %s", got.Outcome)
	}
	if got.Source != VerdictSourceUnspecified {
		t.Errorf("Source should be zero-value for pre-PR events, got %q", got.Source)
	}

	// Re-marshal must NOT introduce a verdict_source key (omitempty contract)
	// and must use the new persona keys, not the legacy phase keys.
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if containsKey(out, `"verdict_source"`) {
		t.Errorf("verdict_source leaked into marshaled bytes despite omitempty: %s", out)
	}
	if containsKey(out, `"phase"`) || containsKey(out, `"source_phase"`) {
		t.Errorf("legacy phase keys leaked into re-marshaled output: %s", out)
	}
	if !containsKey(out, `"persona"`) {
		t.Errorf("persona key missing from marshaled VerdictPayload: %s", out)
	}
}

// TestAIResponsePayloadBackCompatReplay asserts that pre-PR AIResponsePayload
// JSON (without output_raw) deserializes cleanly with OutputRaw=nil, and that
// the legacy `phase` field with a verb value translates to the handler-name
// Persona field. Same motivation as the VerdictPayload back-compat test.
func TestAIResponsePayloadBackCompatReplay(t *testing.T) {
	prePR := []byte(`{"phase":"develop","backend":"claude","tokens_used":100,"duration_ms":1000,"structured":false,"output":"raw text"}`)

	var got AIResponsePayload
	if err := json.Unmarshal(prePR, &got); err != nil {
		t.Fatalf("unmarshal pre-PR AIResponsePayload: %v", err)
	}
	if got.Persona != "developer" || got.Backend != "claude" {
		t.Errorf("legacy phase verb did not translate to handler name: %+v", got)
	}
	if got.OutputRaw != nil {
		t.Errorf("OutputRaw should be nil for pre-PR events, got %s", got.OutputRaw)
	}

	// Re-marshal must NOT introduce an output_raw key.
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if containsKey(out, `"output_raw"`) {
		t.Errorf("output_raw leaked into marshaled bytes despite omitempty: %s", out)
	}
}

// TestLegacyPersonaFromPhase locks the verb→handler-name translation table
// used by the tolerant UnmarshalJSON paths. Operators rely on this for replay
// of in-flight workflows that still have phase-verb-shaped events in the
// store. Adding or renaming a built-in handler must update legacyPersonaFromPhase.
func TestLegacyPersonaFromPhase(t *testing.T) {
	cases := map[string]string{
		"develop":          "developer",
		"research":         "researcher",
		"commit":           "committer",
		"review":           "reviewer",
		"feedback-analyze": "feedback-analyzer",
		"qa-analyze":       "qa-analyzer",
		"pr-reply":         "pr-replier",
		// Pass-through cases — no rename needed:
		"architect":          "architect",
		"qa":                 "qa",
		"pr-category-review": "pr-category-review",
		"unknown-verb":       "unknown-verb",
	}
	for verb, want := range cases {
		if got := legacyPersonaFromPhase(verb); got != want {
			t.Errorf("legacyPersonaFromPhase(%q) = %q; want %q", verb, got, want)
		}
	}
}

// TestFeedbackGeneratedPayloadLegacyKeys asserts the tolerant decoder for
// FeedbackGenerated reads the legacy target_phase/source_phase keys. Unlike
// VerdictPayload, the pre-collapse aggregate already wrote the resolved
// handler name into target_phase, so the value passes through verbatim.
func TestFeedbackGeneratedPayloadLegacyKeys(t *testing.T) {
	legacy := []byte(`{"target_phase":"developer","source_phase":"reviewer","iteration":1,"issues":[],"summary":"x"}`)
	var got FeedbackGeneratedPayload
	if err := json.Unmarshal(legacy, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TargetPersona != "developer" || got.SourcePersona != "reviewer" {
		t.Errorf("legacy keys did not populate persona fields: %+v", got)
	}
}

// TestDefaultRegistryRegistersGroundingSummary asserts the new event type is
// in DefaultRegistry and round-trips via Upcast at schema version 1.
func TestDefaultRegistryRegistersGroundingSummary(t *testing.T) {
	r := DefaultRegistry()
	if !r.IsRegistered(VerdictGroundingSummary) {
		t.Fatalf("VerdictGroundingSummary not registered in DefaultRegistry")
	}

	original := []byte(`{"reviewer":"pr-data","original_count":3,"grounded_count":1,"original_outcome":"fail","final_outcome":"fail"}`)
	upcasted, version, err := r.Upcast(VerdictGroundingSummary, 1, original)
	if err != nil {
		t.Fatalf("Upcast at v1: %v", err)
	}
	if version != 1 {
		t.Errorf("want version 1, got %d", version)
	}
	if string(upcasted) != string(original) {
		t.Errorf("Upcast at current version should be identity, got %s", upcasted)
	}
}

// TestGroundingDropReasonEnumValues guards against accidental rename/typo of
// the drop-reason taxonomy strings — operators query these from SQL by exact
// string match.
func TestGroundingDropReasonEnumValues(t *testing.T) {
	cases := map[GroundingDropReason]string{
		GroundingDropUnspecified:      "",
		GroundingDropFileNotInScope:   "file_not_in_scope",
		GroundingDropNoLineCited:      "no_line_cited",
		GroundingDropLineNotInChanged: "line_not_in_changed",
		GroundingDropTokenNotNearLine: "token_not_near_line",
		GroundingRescuedFileScope:     "rescued_file_scope",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("GroundingDropReason value drift: got %q want %q", string(got), want)
		}
	}
}

// TestVerdictSourceEnumValues — same lock for VerdictSource.
func TestVerdictSourceEnumValues(t *testing.T) {
	cases := map[VerdictSource]string{
		VerdictSourceUnspecified:          "",
		VerdictSourceExplicitPass:         "explicit_pass",
		VerdictSourceExplicitFail:         "explicit_fail",
		VerdictSourceDefaultOptimistic:    "default_optimistic",
		VerdictSourceDowngradedNoGrounded: "downgraded_no_grounded",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("VerdictSource value drift: got %q want %q", string(got), want)
		}
	}
}

func containsKey(payload []byte, key string) bool {
	// Cheap substring check — sufficient because keys are quoted in JSON.
	for i := 0; i+len(key) <= len(payload); i++ {
		if string(payload[i:i+len(key)]) == key {
			return true
		}
	}
	return false
}
