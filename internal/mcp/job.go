package mcp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/marconn/rick-event-driven-development/internal/backend"
)

// JobStatus describes the state of an async job.
type JobStatus string

const (
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
	JobStatusCancelled JobStatus = "cancelled"
)

// Job is a snapshot of an async backend execution (safe to copy).
type Job struct {
	ID            string    `json:"id"`
	Type          string    `json:"type"` // "consult" or "run"
	Status        JobStatus `json:"status"`
	PromptPreview string    `json:"prompt_preview"`
	StartedAt     time.Time `json:"started_at"`
	CompletedAt   time.Time `json:"completed_at,omitempty"`
	Duration      string    `json:"duration,omitempty"`
	Output        string    `json:"output,omitempty"`
	OutputSize    int       `json:"output_size"`
	Error         string    `json:"error,omitempty"`
	Backend       string    `json:"backend,omitempty"`
	Mode          string    `json:"mode,omitempty"`
}

// trackedJob wraps a Job with concurrency control.
type trackedJob struct {
	mu     sync.RWMutex
	data   Job
	cancel context.CancelFunc
}

func (t *trackedJob) snapshot() Job {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.data
}

// JobManager manages async AI jobs in-memory with a background reaper.
type JobManager struct {
	mu   sync.RWMutex
	jobs map[string]*trackedJob

	reaperDone chan struct{}
}

const (
	maxJobAge  = 2 * time.Hour
	reaperTick = 5 * time.Minute
)

// NewJobManager creates a job manager with a background reaper for zombie jobs.
func NewJobManager() *JobManager {
	jm := &JobManager{
		jobs:       make(map[string]*trackedJob),
		reaperDone: make(chan struct{}),
	}
	go jm.reaper()
	return jm
}

// Stop shuts down the background reaper.
func (jm *JobManager) Stop() {
	close(jm.reaperDone)
}

func (jm *JobManager) reaper() {
	ticker := time.NewTicker(reaperTick)
	defer ticker.Stop()
	for {
		select {
		case <-jm.reaperDone:
			return
		case <-ticker.C:
			jm.mu.Lock()
			now := time.Now()
			for id, tj := range jm.jobs {
				tj.mu.Lock()
				if tj.data.Status == JobStatusRunning && now.Sub(tj.data.StartedAt) > maxJobAge {
					tj.data.Status = JobStatusFailed
					tj.data.Error = "job timed out (>2h)"
					tj.data.CompletedAt = now
					if tj.cancel != nil {
						tj.cancel()
					}
					tj.mu.Unlock()
					continue
				}
				if tj.data.Status != JobStatusRunning && now.Sub(tj.data.CompletedAt) > maxJobAge {
					tj.mu.Unlock()
					delete(jm.jobs, id)
					continue
				}
				tj.mu.Unlock()
			}
			jm.mu.Unlock()
		}
	}
}

// Launch starts an async backend execution and returns the job ID immediately.
func (jm *JobManager) Launch(be backend.Backend, req backend.Request, jobType, mode string) string {
	id := uuid.New().String()
	ctx, cancel := context.WithTimeout(context.Background(), maxJobAge)

	preview := req.UserPrompt
	if len(preview) > 100 {
		preview = preview[:100] + "..."
	}

	tj := &trackedJob{
		data: Job{
			ID:            id,
			Type:          jobType,
			Status:        JobStatusRunning,
			PromptPreview: preview,
			StartedAt:     time.Now(),
			Backend:       be.Name(),
			Mode:          mode,
		},
		cancel: cancel,
	}

	jm.mu.Lock()
	jm.jobs[id] = tj
	jm.mu.Unlock()

	go func() {
		defer cancel()
		resp, err := be.Run(ctx, req)

		tj.mu.Lock()
		defer tj.mu.Unlock()

		if tj.data.Status == JobStatusCancelled {
			return
		}

		tj.data.CompletedAt = time.Now()
		tj.data.Duration = tj.data.CompletedAt.Sub(tj.data.StartedAt).Round(time.Millisecond).String()

		if err != nil {
			tj.data.Status = JobStatusFailed
			tj.data.Error = err.Error()
			return
		}

		tj.data.Status = JobStatusCompleted
		tj.data.Output = resp.Output
		tj.data.OutputSize = len(resp.Output)
	}()

	return id
}

// Get returns a snapshot of a job by ID.
func (jm *JobManager) Get(id string) (*Job, error) {
	jm.mu.RLock()
	tj, ok := jm.jobs[id]
	jm.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("job not found: %s", id)
	}
	snap := tj.snapshot()
	return &snap, nil
}

// Cancel cancels a running job.
func (jm *JobManager) Cancel(id string) error {
	jm.mu.RLock()
	tj, ok := jm.jobs[id]
	jm.mu.RUnlock()
	if !ok {
		return fmt.Errorf("job not found: %s", id)
	}
	tj.mu.Lock()
	defer tj.mu.Unlock()
	if tj.data.Status != JobStatusRunning {
		return fmt.Errorf("cannot cancel job in %s state", tj.data.Status)
	}
	tj.data.Status = JobStatusCancelled
	tj.data.CompletedAt = time.Now()
	tj.data.Duration = tj.data.CompletedAt.Sub(tj.data.StartedAt).Round(time.Millisecond).String()
	if tj.cancel != nil {
		tj.cancel()
	}
	return nil
}

// List returns all tracked jobs (without full output).
func (jm *JobManager) List() []Job {
	jm.mu.RLock()
	defer jm.mu.RUnlock()
	result := make([]Job, 0, len(jm.jobs))
	for _, tj := range jm.jobs {
		snap := tj.snapshot()
		snap.Output = "" // omit full output from list
		result = append(result, snap)
	}
	return result
}
