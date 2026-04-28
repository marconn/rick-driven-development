package event

import (
	"encoding/json"
	"testing"
)

// TestVerdictPayloadBackCompatReplay asserts that pre-PR VerdictPayload JSON
// (without the verdict_source field) deserializes cleanly with the
// VerdictSource zero value. This locks in the back-compat contract: any future
// rename or non-pointer enum change that breaks replay will fail this test.
func TestVerdictPayloadBackCompatReplay(t *testing.T) {
	prePR := []byte(`{"phase":"develop","source_phase":"review","outcome":"pass","summary":"passed review"}`)

	var got VerdictPayload
	if err := json.Unmarshal(prePR, &got); err != nil {
		t.Fatalf("unmarshal pre-PR VerdictPayload: %v", err)
	}
	if got.Phase != "develop" || got.SourcePhase != "review" {
		t.Errorf("phase fields lost: %+v", got)
	}
	if got.Outcome != VerdictPass {
		t.Errorf("outcome lost: %s", got.Outcome)
	}
	if got.Source != VerdictSourceUnspecified {
		t.Errorf("Source should be zero-value for pre-PR events, got %q", got.Source)
	}

	// Re-marshal must NOT introduce a verdict_source key (omitempty contract).
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if containsKey(out, `"verdict_source"`) {
		t.Errorf("verdict_source leaked into marshaled bytes despite omitempty: %s", out)
	}
}

// TestAIResponsePayloadBackCompatReplay asserts that pre-PR AIResponsePayload
// JSON (without output_raw) deserializes cleanly with OutputRaw=nil. Same
// motivation as the VerdictPayload back-compat test.
func TestAIResponsePayloadBackCompatReplay(t *testing.T) {
	prePR := []byte(`{"phase":"develop","backend":"claude","tokens_used":100,"duration_ms":1000,"structured":false,"output":"raw text"}`)

	var got AIResponsePayload
	if err := json.Unmarshal(prePR, &got); err != nil {
		t.Fatalf("unmarshal pre-PR AIResponsePayload: %v", err)
	}
	if got.Phase != "develop" || got.Backend != "claude" {
		t.Errorf("base fields lost: %+v", got)
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
		GroundingDropLineNotInChanged: "line_not_in_changed",
		GroundingDropTokenNotNearLine: "token_not_near_line",
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
