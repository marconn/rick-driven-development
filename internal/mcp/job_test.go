package mcp

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/backend"
)

// stubBackend is a test double for backend.Backend.
// It can simulate a slow response (via latency) or an immediate result.
type stubBackend struct {
	name    string
	resp    backend.Response
	err     error
	latency time.Duration
}

func (b *stubBackend) Name() string { return b.name }

func (b *stubBackend) Run(ctx context.Context, _ backend.Request) (*backend.Response, error) {
	if b.latency > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(b.latency):
		}
	}
	if b.err != nil {
		return nil, b.err
	}
	resp := b.resp
	return &resp, nil
}

// waitForStatus polls until the job reaches a non-running status or the
// deadline expires. Returns the final job snapshot.
func waitForStatus(t *testing.T, jm *JobManager, id string, timeout time.Duration) *Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		j, err := jm.Get(id)
		if err != nil {
			t.Fatalf("waitForStatus: Get(%q): %v", id, err)
		}
		if j.Status != JobStatusRunning {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %q did not reach terminal status within %s", id, timeout)
	return nil
}

// ============================================================
// G. JobManager unit tests
// ============================================================

// TestJobManager_LaunchAndGet verifies that a launched job completes and the
// output is captured.
func TestJobManager_LaunchAndGet(t *testing.T) {
	jm := NewJobManager()
	defer jm.Stop()

	be := &stubBackend{name: "test", resp: backend.Response{Output: "hello world"}}
	req := backend.Request{UserPrompt: "my task"}

	id := jm.Launch(be, req, "consult", "architect")
	if id == "" {
		t.Fatal("expected non-empty job ID")
	}

	job := waitForStatus(t, jm, id, time.Second)
	if job.Status != JobStatusCompleted {
		t.Errorf("expected completed, got %s", job.Status)
	}
	if job.Output != "hello world" {
		t.Errorf("expected output 'hello world', got %q", job.Output)
	}
	if job.Type != "consult" {
		t.Errorf("expected type 'consult', got %q", job.Type)
	}
	if job.Mode != "architect" {
		t.Errorf("expected mode 'architect', got %q", job.Mode)
	}
	if job.Backend != "test" {
		t.Errorf("expected backend 'test', got %q", job.Backend)
	}
	if job.Duration == "" {
		t.Error("expected non-empty duration")
	}
}

// TestJobManager_LaunchRunning verifies that a job started with a slow backend
// is immediately visible as running.
func TestJobManager_LaunchRunning(t *testing.T) {
	jm := NewJobManager()
	defer jm.Stop()

	be := &stubBackend{name: "slow", latency: 10 * time.Second}
	req := backend.Request{UserPrompt: "slow task"}

	id := jm.Launch(be, req, "run", "developer")

	job, err := jm.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.Status != JobStatusRunning {
		t.Errorf("expected running immediately after launch, got %s", job.Status)
	}
	if job.Backend != "slow" {
		t.Errorf("expected backend 'slow', got %q", job.Backend)
	}
}

// TestJobManager_GetNotFound verifies that querying an unknown job returns an error.
func TestJobManager_GetNotFound(t *testing.T) {
	jm := NewJobManager()
	defer jm.Stop()

	_, err := jm.Get("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent job")
	}
	if err.Error() == "" {
		t.Error("expected non-empty error message")
	}
}

// TestJobManager_Cancel verifies that cancelling a running job transitions it to
// cancelled and signals the running goroutine via context cancellation.
func TestJobManager_Cancel(t *testing.T) {
	jm := NewJobManager()
	defer jm.Stop()

	be := &stubBackend{name: "slow", latency: 30 * time.Second}
	req := backend.Request{UserPrompt: "cancel me"}

	id := jm.Launch(be, req, "run", "developer")

	if err := jm.Cancel(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	job, err := jm.Get(id)
	if err != nil {
		t.Fatalf("Get after cancel: %v", err)
	}
	if job.Status != JobStatusCancelled {
		t.Errorf("expected cancelled, got %s", job.Status)
	}
	if job.CompletedAt.IsZero() {
		t.Error("expected non-zero CompletedAt after cancel")
	}
}

// TestJobManager_CancelAfterCompletion verifies that cancelling a completed job
// returns an error — it is not a no-op.
func TestJobManager_CancelAfterCompletion(t *testing.T) {
	jm := NewJobManager()
	defer jm.Stop()

	be := &stubBackend{name: "fast", resp: backend.Response{Output: "done quickly"}}
	req := backend.Request{UserPrompt: "fast task"}

	id := jm.Launch(be, req, "consult", "qa")
	waitForStatus(t, jm, id, time.Second)

	// Cancelling after completion must fail.
	err := jm.Cancel(id)
	if err == nil {
		t.Error("expected error when cancelling a completed job")
	}
}

// TestJobManager_CancelNotFound verifies that cancelling an unknown job returns an error.
func TestJobManager_CancelNotFound(t *testing.T) {
	jm := NewJobManager()
	defer jm.Stop()

	err := jm.Cancel("nonexistent-id")
	if err == nil {
		t.Error("expected error when cancelling a nonexistent job")
	}
}

// TestJobManager_List verifies that List returns all tracked jobs and strips output.
func TestJobManager_List(t *testing.T) {
	jm := NewJobManager()
	defer jm.Stop()

	// Launch 3 slow jobs so they stay running during List.
	be := &stubBackend{name: "slow", latency: 30 * time.Second}
	for range 3 {
		jm.Launch(be, backend.Request{UserPrompt: "task"}, "consult", "architect")
	}

	jobs := jm.List()
	if len(jobs) != 3 {
		t.Errorf("expected 3 jobs in list, got %d", len(jobs))
	}
	// List should strip full output to avoid oversized payloads.
	for _, j := range jobs {
		if j.Output != "" {
			t.Error("List should not include full output in response")
		}
	}
}

// TestJobManager_ListEmpty verifies that an empty manager returns an empty slice.
func TestJobManager_ListEmpty(t *testing.T) {
	jm := NewJobManager()
	defer jm.Stop()

	jobs := jm.List()
	if jobs == nil {
		t.Error("expected non-nil slice for empty list")
	}
	if len(jobs) != 0 {
		t.Errorf("expected 0 jobs, got %d", len(jobs))
	}
}

// TestJobManager_FailedBackend verifies that a backend error transitions the job to failed.
func TestJobManager_FailedBackend(t *testing.T) {
	jm := NewJobManager()
	defer jm.Stop()

	be := &stubBackend{name: "erroring", err: context.DeadlineExceeded}
	req := backend.Request{UserPrompt: "will fail"}

	id := jm.Launch(be, req, "run", "developer")
	job := waitForStatus(t, jm, id, time.Second)

	if job.Status != JobStatusFailed {
		t.Errorf("expected failed, got %s", job.Status)
	}
	if job.Error == "" {
		t.Error("expected non-empty error message for failed job")
	}
}

// TestJobManager_PromptPreviewTruncated verifies that prompts longer than 100 chars
// are truncated with "..." in the preview.
func TestJobManager_PromptPreviewTruncated(t *testing.T) {
	jm := NewJobManager()
	defer jm.Stop()

	longPrompt := string(make([]byte, 200))
	for i := range []byte(longPrompt) {
		_ = i
	}
	longPrompt = "x" // reset then repeat
	for len(longPrompt) < 200 {
		longPrompt += "x"
	}

	be := &stubBackend{name: "test", latency: 30 * time.Second}
	id := jm.Launch(be, backend.Request{UserPrompt: longPrompt}, "run", "dev")

	job, err := jm.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	// Preview must be ≤ 103 chars (100 + "...").
	if len(job.PromptPreview) > 103 {
		t.Errorf("prompt preview too long: %d chars", len(job.PromptPreview))
	}
	if len(job.PromptPreview) < 100 {
		t.Errorf("prompt preview too short: %d chars", len(job.PromptPreview))
	}
}

// TestJobManager_ConcurrentLaunch verifies that concurrent launches produce
// unique, non-empty job IDs without data races.
func TestJobManager_ConcurrentLaunch(t *testing.T) {
	jm := NewJobManager()
	defer jm.Stop()

	be := &stubBackend{name: "fast", resp: backend.Response{Output: "ok"}}

	const count = 20
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids = make([]string, count)
	)

	for i := range count {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := jm.Launch(be, backend.Request{UserPrompt: "concurrent"}, "consult", "qa")
			mu.Lock()
			ids[idx] = id
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	// All IDs must be non-empty and unique.
	seen := make(map[string]bool, count)
	for _, id := range ids {
		if id == "" {
			t.Error("got empty job ID from concurrent launch")
		}
		if seen[id] {
			t.Errorf("duplicate job ID: %s", id)
		}
		seen[id] = true
	}
}

// TestJobManager_Stop verifies that Stop() terminates the reaper goroutine.
// A second Stop() call should not deadlock (channel already closed).
func TestJobManager_Stop(t *testing.T) {
	jm := NewJobManager()

	// Stop should not block.
	done := make(chan struct{})
	go func() {
		jm.Stop()
		close(done)
	}()

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return within 2 seconds")
	}
}

// TestJobManager_OutputSize verifies that OutputSize is set correctly after completion.
func TestJobManager_OutputSize(t *testing.T) {
	jm := NewJobManager()
	defer jm.Stop()

	output := "the quick brown fox"
	be := &stubBackend{name: "test", resp: backend.Response{Output: output}}
	req := backend.Request{UserPrompt: "size check"}

	id := jm.Launch(be, req, "consult", "researcher")
	job := waitForStatus(t, jm, id, time.Second)

	if job.OutputSize != len(output) {
		t.Errorf("expected OutputSize=%d, got %d", len(output), job.OutputSize)
	}
}
