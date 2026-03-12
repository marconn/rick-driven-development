package event

import (
	"encoding/json"
	"testing"
)

func TestRegistryBasic(t *testing.T) {
	r := NewRegistry()
	r.Register("test.event", "A test event", 1)

	if !r.IsRegistered("test.event") {
		t.Error("expected test.event to be registered")
	}
	if r.IsRegistered("unknown.event") {
		t.Error("expected unknown.event to not be registered")
	}

	types := r.Types()
	if len(types) != 1 {
		t.Fatalf("expected 1 type, got %d", len(types))
	}
}

func TestRegistryUpcast(t *testing.T) {
	r := NewRegistry()
	r.Register("test.event", "A test event", 3)

	// v1 → v2: add "new_field"
	err := r.RegisterUpcaster("test.event", 1, func(p json.RawMessage) (json.RawMessage, error) {
		var data map[string]any
		if err := json.Unmarshal(p, &data); err != nil {
			return nil, err
		}
		data["new_field"] = "default"
		return json.Marshal(data)
	})
	if err != nil {
		t.Fatalf("register upcaster v1: %v", err)
	}

	// v2 → v3: rename "name" to "title"
	err = r.RegisterUpcaster("test.event", 2, func(p json.RawMessage) (json.RawMessage, error) {
		var data map[string]any
		if err := json.Unmarshal(p, &data); err != nil {
			return nil, err
		}
		if name, ok := data["name"]; ok {
			data["title"] = name
			delete(data, "name")
		}
		return json.Marshal(data)
	})
	if err != nil {
		t.Fatalf("register upcaster v2: %v", err)
	}

	// Upcast from v1
	original := json.RawMessage(`{"name":"test"}`)
	result, version, err := r.Upcast("test.event", 1, original)
	if err != nil {
		t.Fatalf("upcast: %v", err)
	}
	if version != 3 {
		t.Errorf("expected version 3, got %d", version)
	}

	var data map[string]any
	if err := json.Unmarshal(result, &data); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if data["title"] != "test" {
		t.Errorf("expected title=test, got %v", data["title"])
	}
	if data["new_field"] != "default" {
		t.Errorf("expected new_field=default, got %v", data["new_field"])
	}
	if _, ok := data["name"]; ok {
		t.Error("expected name to be removed")
	}
}

func TestRegistryUpcastCurrentVersion(t *testing.T) {
	r := NewRegistry()
	r.Register("test.event", "A test event", 1)

	payload := json.RawMessage(`{"key":"value"}`)
	result, version, err := r.Upcast("test.event", 1, payload)
	if err != nil {
		t.Fatalf("upcast: %v", err)
	}
	if version != 1 {
		t.Errorf("expected version 1, got %d", version)
	}
	if string(result) != string(payload) {
		t.Error("payload should be unchanged when already at current version")
	}
}

func TestRegistryUpcastUnknownType(t *testing.T) {
	r := NewRegistry()
	payload := json.RawMessage(`{"key":"value"}`)
	result, version, err := r.Upcast("unknown.event", 1, payload)
	if err != nil {
		t.Fatalf("upcast unknown type should not error: %v", err)
	}
	if version != 1 {
		t.Errorf("expected version 1, got %d", version)
	}
	if string(result) != string(payload) {
		t.Error("payload should pass through for unknown types")
	}
}

func TestRegistryUpcastMissingUpcaster(t *testing.T) {
	r := NewRegistry()
	r.Register("test.event", "A test event", 3)
	// Only register v1→v2, skip v2→v3

	_ = r.RegisterUpcaster("test.event", 1, func(p json.RawMessage) (json.RawMessage, error) {
		return p, nil
	})

	_, _, err := r.Upcast("test.event", 1, json.RawMessage(`{}`))
	if err == nil {
		t.Error("expected error for missing upcaster v2→v3")
	}
}

func TestRegistryRegisterUpcasterUnknownType(t *testing.T) {
	r := NewRegistry()
	err := r.RegisterUpcaster("unknown.event", 1, func(p json.RawMessage) (json.RawMessage, error) {
		return p, nil
	})
	if err == nil {
		t.Error("expected error for unknown type")
	}
}

func TestDefaultRegistry(t *testing.T) {
	r := DefaultRegistry()
	// Verify core types are registered
	coreTypes := []Type{
		WorkflowRequested, WorkflowStarted, WorkflowCompleted, WorkflowFailed,
		WorkflowCancelled, AIRequestSent, AIResponseReceived,
		VerdictRendered, FeedbackGenerated,
	}
	for _, et := range coreTypes {
		if !r.IsRegistered(et) {
			t.Errorf("expected %s to be registered in default registry", et)
		}
	}
}
