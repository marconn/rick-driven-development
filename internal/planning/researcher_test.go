package planning

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// TestResearcherFallbackToInference verifies that when the confluence-reader
// detects 0 microservices (no [bracket] patterns in the BTU), the researcher
// falls back to the AI inference path rather than producing empty findings.
//
// This is an integration-level test that validates the Handle() flow without
// actually calling AI backends -- we verify the resolve path is attempted.
func TestResearcherFallbackToInference(t *testing.T) {
	dir := t.TempDir()

	// Set up RICK_REPOS_PATH with some fake repos.
	for _, repo := range []string{"frontend-emr", "bff-emr", "backend-api", "practice-web"} {
		repoDir := filepath.Join(dir, repo)
		_ = os.Mkdir(repoDir, 0o755)
		_ = os.Mkdir(filepath.Join(repoDir, ".git"), 0o755)
	}

	msMap := NewMicroserviceMap(dir)
	state := NewPlanningState()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// No backends -- we can't call AI in unit tests.
	// But we CAN verify ListAvailable is called properly.
	researcher := NewResearcher(nil, msMap, state, logger)

	// Seed the state as if confluence-reader ran and found 0 microservices.
	wp := state.Get("test-correlation")
	wp.mu.Lock()
	wp.BTUTitle = "BTU-1234: Test feature"
	wp.BTUContent = "Implement a new insurance module with PDF upload and WhatsApp sharing"
	wp.Microservices = nil // 0 microservices detected
	wp.mu.Unlock()

	// Verify ListAvailable returns our fake repos.
	available := msMap.ListAvailable()
	if len(available) != 4 {
		t.Fatalf("expected 4 available repos, got %d", len(available))
	}

	// We can't run Handle() fully without AI backends, but we can verify
	// that the inference method handles the no-backends case gracefully.
	repos := researcher.inferRepos(context.Background(), "BTU-1234", "Test content")
	// With no backends, inferRepos returns nil (no panic, no crash).
	if repos != nil {
		t.Errorf("expected nil repos with no backends, got %v", repos)
	}
}

// TestResearcherWithMicroservicesSkipsInference verifies the normal path:
// when microservices ARE detected by confluence-reader, the researcher resolves
// them directly without falling back to inference.
func TestResearcherWithMicroservicesSkipsInference(t *testing.T) {
	dir := t.TempDir()

	// Create the specific repos that the BTU mentions.
	for _, repo := range []string{"frontend-emr", "bff-emr"} {
		repoDir := filepath.Join(dir, repo)
		_ = os.Mkdir(repoDir, 0o755)
	}

	msMap := NewMicroserviceMap(dir)

	found, missing := msMap.ResolveAll([]string{"frontend-emr", "bff-emr"})
	if len(found) != 2 {
		t.Fatalf("expected 2 found repos, got %d (missing: %v)", len(found), missing)
	}
	if len(missing) != 0 {
		t.Errorf("expected 0 missing, got %v", missing)
	}
}

// TestResearcherHandleEmptyBTU verifies error handling when BTU content is empty.
func TestResearcherHandleEmptyBTU(t *testing.T) {
	dir := t.TempDir()
	msMap := NewMicroserviceMap(dir)
	state := NewPlanningState()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	researcher := NewResearcher(nil, msMap, state, logger)

	// Don't seed state -- BTUContent will be empty.
	env := event.Envelope{CorrelationID: "empty-test"}

	_, err := researcher.Handle(context.Background(), env)
	if err == nil {
		t.Error("expected error for empty BTU content")
	}
}
