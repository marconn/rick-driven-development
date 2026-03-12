package estimation

import (
	"context"
	"testing"
)

func TestNewStoreAndSaveBatch(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	estimates := []Estimate{
		{TicketID: "PROJ-1", TaskDescription: "Add endpoint", Microservice: "api", Category: "backend", EstimatedPoints: 3},
		{TicketID: "PROJ-1", TaskDescription: "Add UI form", Microservice: "web", Category: "frontend", EstimatedPoints: 5},
	}

	if err := store.SaveBatch(ctx, estimates); err != nil {
		t.Fatalf("SaveBatch: %v", err)
	}

	// IDs should be auto-generated.
	if estimates[0].ID == "" {
		t.Error("want auto-generated ID on first estimate")
	}
}

func TestCalibrationSummaryEmpty(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	summary, err := store.CalibrationSummary(context.Background())
	if err != nil {
		t.Fatalf("CalibrationSummary: %v", err)
	}
	if summary != "No calibration data available yet. Use base estimation rules." {
		t.Errorf("unexpected summary: %q", summary)
	}
}

func TestSimilarEstimatesEmpty(t *testing.T) {
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	result, err := store.SimilarEstimates(context.Background(), "api", "")
	if err != nil {
		t.Fatalf("SimilarEstimates: %v", err)
	}
	if result != "" {
		t.Errorf("want empty for no data, got %q", result)
	}
}
